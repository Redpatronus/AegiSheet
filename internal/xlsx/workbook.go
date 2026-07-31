// Package xlsx wraps the spreadsheet library with the invariants AegiSheet needs.
//
// The original implementation addressed cells directly ("A"+strconv.Itoa(itr))
// and tracked the current row in a local counter inside every report function.
// Two classes of defect followed from that, and both are structurally impossible
// here:
//
//   - Row drift. A counter that was not incremented on some branch caused later
//     rows to overwrite earlier ones, silently losing data. Sheet owns the cursor
//     and advances it on every append.
//   - Column drift. Headers were written in one place and values in another, so a
//     column added to one and not the other misaligned the whole sheet. A sheet
//     is created from its column list, and AppendRow rejects a row whose arity
//     does not match it.
//
// The package also centralises presentation (header styling, frozen header row,
// auto-filter, column widths) and makes writing the workbook atomic and
// owner-only, because the output contains personal data.
package xlsx

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/Redpatronus/AegiSheet/internal/logging"
)

// ErrOutputExists is returned by Save when the target file is present and
// overwriting was not permitted.
var ErrOutputExists = errors.New("output file already exists")

// excelMaxRows is the row limit of the .xlsx format. Hitting it is a data-loss
// event, so it is reported rather than silently truncated.
const excelMaxRows = 1_048_576

// headerRow is the (1-based) row that holds column titles.
const headerRow = 1

// Column describes one worksheet column.
type Column struct {
	// Header is the column title written into the header row.
	Header string
	// Width is the column width in Excel's character units. Zero leaves the
	// default width in place.
	Width float64
}

// Col is a shorthand constructor for Column, which keeps the column definitions
// in the report tasks compact and readable.
func Col(header string, width float64) Column {
	return Column{Header: header, Width: width}
}

// Workbook is a spreadsheet under construction.
//
// It is not safe for concurrent use from multiple goroutines; report tasks run
// sequentially by design, so that the order of sheets in the workbook matches the
// order of sections in the configuration file.
type Workbook struct {
	file        *excelize.File
	log         *slog.Logger
	headerStyle int

	// defaultSheet is the placeholder sheet the library creates with every new
	// file. It is deleted on save unless a report claimed that exact name.
	defaultSheet string

	// used holds the lowercased final names of created sheets, for collision
	// detection.
	used   map[string]struct{}
	sheets []*Sheet

	activeSheetSet bool
}

// New creates an empty workbook. The caller must Close it to release the
// temporary files the spreadsheet library may allocate.
func New(log *slog.Logger) (*Workbook, error) {
	if log == nil {
		log = logging.Discard()
	}

	file := excelize.NewFile()

	// A single shared style for every header row: bold white on dark blue with a
	// rule underneath. Defined once because Excel deduplicates styles by ID, not
	// by content.
	headerStyle, err := file.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF", Size: 11},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"1F4E79"}},
		Alignment: &excelize.Alignment{
			Horizontal: "left",
			Vertical:   "center",
		},
		Border: []excelize.Border{{Type: "bottom", Style: 2, Color: "0F243E"}},
	})
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("create header style: %w", err)
	}

	return &Workbook{
		file:         file,
		log:          log,
		headerStyle:  headerStyle,
		defaultSheet: file.GetSheetName(0),
		used:         make(map[string]struct{}),
	}, nil
}

// AddSheet creates a worksheet with a styled, frozen header row.
//
// name is normalised to a legal, unique worksheet title; the effective title is
// available from Sheet.Name and is logged when it differs from the requested one,
// so an operator can find the sheet a section produced.
func (w *Workbook) AddSheet(name string, columns []Column) (*Sheet, error) {
	if len(columns) == 0 {
		return nil, fmt.Errorf("sheet %q: at least one column is required", name)
	}

	final := uniqueSheetName(w.used, SanitizeSheetName(name))
	if final != name {
		w.log.Warn("worksheet name adjusted to satisfy spreadsheet limits",
			slog.String("requested", name), slog.String("sheet", final))
	}

	index, err := w.file.NewSheet(final)
	if err != nil {
		return nil, fmt.Errorf("create sheet %q: %w", final, err)
	}
	w.used[strings.ToLower(final)] = struct{}{}

	sheet := &Sheet{
		book:    w,
		name:    final,
		columns: columns,
		nextRow: headerRow + 1,
	}

	if err := sheet.writeHeader(); err != nil {
		return nil, err
	}

	// The first report sheet becomes the active one, so the workbook does not open
	// on the empty placeholder sheet.
	if !w.activeSheetSet {
		w.file.SetActiveSheet(index)
		w.activeSheetSet = true
	}

	w.sheets = append(w.sheets, sheet)
	w.log.Debug("worksheet created", slog.String("sheet", final), slog.Int("columns", len(columns)))
	return sheet, nil
}

// Sheets returns the sheets created so far, in creation order.
func (w *Workbook) Sheets() []*Sheet {
	return append([]*Sheet(nil), w.sheets...)
}

// Save writes the workbook to path.
//
// The write is atomic: the content goes to a temporary file in the destination
// directory and is renamed into place only after a successful flush, so an
// interrupted run cannot leave a truncated workbook where a valid previous report
// used to be. The file is created with mode 0600 and relaxed to the requested
// mode only afterwards, so its contents are never briefly world-readable.
func (w *Workbook) Save(path string, mode fs.FileMode, overwrite bool) error {
	if path == "" {
		return errors.New("no output path given")
	}

	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%w: %s", ErrOutputExists, path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat output %s: %w", path, err)
		}
	}

	// Drop the library's placeholder sheet unless a report happened to use that
	// exact name, and never drop the last remaining sheet: a workbook needs one.
	if _, claimed := w.used[strings.ToLower(w.defaultSheet)]; !claimed && len(w.sheets) > 0 {
		if err := w.file.DeleteSheet(w.defaultSheet); err != nil {
			// Not fatal: an extra empty sheet is cosmetic, a lost report is not.
			w.log.Warn("could not remove placeholder worksheet",
				slog.String("sheet", w.defaultSheet), slog.String("error", err.Error()))
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create output directory %s: %w", dir, err)
	}

	// The temporary file must live in the destination directory: rename is only
	// atomic within a single filesystem.
	tmp, err := os.CreateTemp(dir, ".aegisheet-*.xlsx")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup for every failure path below.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if err := w.file.Write(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode workbook: %w", err)
	}
	// fsync before the rename: the rename is only useful if the bytes it points at
	// are durable.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush workbook to %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set permissions on %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("move workbook into place at %s: %w", path, err)
	}
	tmpName = "" // ownership transferred; disarm the cleanup

	rows := 0
	for _, sheet := range w.sheets {
		rows += sheet.rows
	}
	w.log.Info("workbook written",
		slog.String("path", path),
		slog.String("mode", fmt.Sprintf("0%o", mode.Perm())),
		slog.Int("sheets", len(w.sheets)),
		slog.Int("rows", rows))
	return nil
}

// Close releases resources held by the spreadsheet library.
func (w *Workbook) Close() error {
	return w.file.Close()
}

// Sheet is a worksheet being filled in, owning its own row cursor.
type Sheet struct {
	book    *Workbook
	name    string
	columns []Column

	// nextRow is the 1-based row that the next append writes to.
	nextRow int
	// rows counts data rows written, excluding the header.
	rows int
	// maxRows optionally caps the data rows accepted; zero means unlimited.
	maxRows int
	// truncated records that at least one row was dropped by maxRows.
	truncated bool
}

// Name returns the effective worksheet title, after normalisation.
func (s *Sheet) Name() string { return s.name }

// Rows returns the number of data rows written so far.
func (s *Sheet) Rows() int { return s.rows }

// Truncated reports whether the row cap discarded any data.
func (s *Sheet) Truncated() bool { return s.truncated }

// LimitRows caps the number of data rows this sheet accepts. Appends past the cap
// are dropped and flagged via Truncated. Zero (the default) means unlimited.
func (s *Sheet) LimitRows(max int) {
	s.maxRows = max
}

// Full reports whether the row cap has been reached, which lets a task stop
// paging through an API instead of fetching data that will be discarded.
func (s *Sheet) Full() bool {
	return s.maxRows > 0 && s.rows >= s.maxRows
}

// writeHeader emits the title row and applies styling, widths and freezing.
func (s *Sheet) writeHeader() error {
	headers := make([]any, len(s.columns))
	for i, column := range s.columns {
		headers[i] = column.Header
	}
	// SetSheetRow takes a pointer to a slice.
	if err := s.book.file.SetSheetRow(s.name, "A"+strconv.Itoa(headerRow), &headers); err != nil {
		return fmt.Errorf("sheet %q: write header row: %w", s.name, err)
	}

	lastColumn, err := excelize.ColumnNumberToName(len(s.columns))
	if err != nil {
		return fmt.Errorf("sheet %q: resolve last column: %w", s.name, err)
	}

	if err := s.book.file.SetCellStyle(s.name,
		"A"+strconv.Itoa(headerRow),
		lastColumn+strconv.Itoa(headerRow),
		s.book.headerStyle); err != nil {
		return fmt.Errorf("sheet %q: style header row: %w", s.name, err)
	}
	if err := s.book.file.SetRowHeight(s.name, headerRow, 18); err != nil {
		return fmt.Errorf("sheet %q: set header row height: %w", s.name, err)
	}

	for i, column := range s.columns {
		if column.Width <= 0 {
			continue
		}
		name, err := excelize.ColumnNumberToName(i + 1)
		if err != nil {
			return fmt.Errorf("sheet %q: resolve column %d: %w", s.name, i+1, err)
		}
		if err := s.book.file.SetColWidth(s.name, name, name, column.Width); err != nil {
			return fmt.Errorf("sheet %q: set width of column %s: %w", s.name, name, err)
		}
	}

	// Freeze the header so it stays visible while scrolling a long report.
	if err := s.book.file.SetPanes(s.name, &excelize.Panes{
		Freeze:      true,
		YSplit:      headerRow,
		TopLeftCell: "A" + strconv.Itoa(headerRow+1),
		ActivePane:  "bottomLeft",
		Selection: []excelize.Selection{
			{SQRef: "A" + strconv.Itoa(headerRow+1), ActiveCell: "A" + strconv.Itoa(headerRow+1), Pane: "bottomLeft"},
		},
	}); err != nil {
		return fmt.Errorf("sheet %q: freeze header row: %w", s.name, err)
	}

	return nil
}

// AppendRow writes one data row and advances the cursor.
//
// The number of values must equal the number of columns the sheet was created
// with; a mismatch is a programming error that would misalign every subsequent
// column, so it is reported instead of written.
func (s *Sheet) AppendRow(values ...any) error {
	if len(values) != len(s.columns) {
		return fmt.Errorf("sheet %q: got %d values for %d columns", s.name, len(values), len(s.columns))
	}
	if s.Full() {
		// Record the loss once; callers check Full to stop early.
		s.truncated = true
		return nil
	}
	if s.nextRow > excelMaxRows {
		return fmt.Errorf("sheet %q: spreadsheet row limit of %d reached", s.name, excelMaxRows)
	}

	if err := s.book.file.SetSheetRow(s.name, "A"+strconv.Itoa(s.nextRow), &values); err != nil {
		return fmt.Errorf("sheet %q: write row %d: %w", s.name, s.nextRow, err)
	}
	s.nextRow++
	s.rows++
	return nil
}

// Finalize applies the finishing touches that depend on the final row count. It
// must be called once, after the last append.
func (s *Sheet) Finalize() error {
	if s.rows == 0 {
		// An empty sheet is a legitimate result ("no users without 2FA"), and
		// auto-filtering an empty range is rejected by the format.
		s.book.log.Info("worksheet completed with no data rows", slog.String("sheet", s.name))
		return nil
	}

	lastColumn, err := excelize.ColumnNumberToName(len(s.columns))
	if err != nil {
		return fmt.Errorf("sheet %q: resolve last column: %w", s.name, err)
	}
	rangeRef := fmt.Sprintf("A%d:%s%d", headerRow, lastColumn, s.nextRow-1)

	// An auto-filter gives the reader sorting and filtering without any macro or
	// manual setup, which is how these reports are actually consumed.
	if err := s.book.file.AutoFilter(s.name, rangeRef, nil); err != nil {
		return fmt.Errorf("sheet %q: apply auto-filter over %s: %w", s.name, rangeRef, err)
	}

	if s.truncated {
		s.book.log.Warn("worksheet truncated by the configured row limit",
			slog.String("sheet", s.name), slog.Int("rows", s.rows), slog.Int("max_rows", s.maxRows))
	}
	return nil
}
