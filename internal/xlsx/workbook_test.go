package xlsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/Redpatronus/AegiSheet/internal/logging"
)

func newTestWorkbook(t *testing.T) *Workbook {
	t.Helper()
	book, err := New(logging.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return book
}

var twoColumns = []Column{Col("E-MAIL", 30), Col("ROLE", 12)}

func TestSanitizeSheetName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "unchanged", in: "Group Members", want: "Group Members"},
		{name: "forbidden characters", in: "Audit: 2024/Q1", want: "Audit_ 2024_Q1"},
		{name: "brackets and star", in: "a[b]c*d?e", want: "a_b_c_d_e"},
		{name: "trimmed", in: "  Admins  ", want: "Admins"},
		{name: "empty", in: "   ", want: fallbackSheetName},
		{name: "leading apostrophe", in: "'quoted'", want: "quoted"},
		{name: "reserved", in: "history", want: "History_"},
		{
			name: "over the length limit",
			in:   "a section name that is definitely longer than thirty one characters",
			want: "a section name that is definite",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SanitizeSheetName(test.in)
			if got != test.want {
				t.Errorf("SanitizeSheetName(%q) = %q, want %q", test.in, got, test.want)
			}
			if len([]rune(got)) > MaxSheetNameLen {
				t.Errorf("result %q exceeds the %d character limit", got, MaxSheetNameLen)
			}
		})
	}
}

func TestAddSheetDisambiguatesCollidingNames(t *testing.T) {
	book := newTestWorkbook(t)

	// Two section names that differ only past the length limit collapse onto the
	// same worksheet title; the second must not be rejected or overwrite the first.
	long := "an extremely long section name variant "
	first, err := book.AddSheet(long+"one", twoColumns)
	if err != nil {
		t.Fatalf("AddSheet: %v", err)
	}
	second, err := book.AddSheet(long+"two", twoColumns)
	if err != nil {
		t.Fatalf("AddSheet: %v", err)
	}

	if first.Name() == second.Name() {
		t.Fatalf("both sections resolved to the same sheet %q", first.Name())
	}
	if !strings.HasSuffix(second.Name(), "~2") {
		t.Errorf("second sheet = %q, want a ~2 discriminator", second.Name())
	}
	if len([]rune(second.Name())) > MaxSheetNameLen {
		t.Errorf("disambiguated name %q exceeds the length limit", second.Name())
	}
}

func TestAddSheetRequiresColumns(t *testing.T) {
	book := newTestWorkbook(t)
	if _, err := book.AddSheet("Empty", nil); err == nil {
		t.Error("a sheet with no columns was accepted")
	}
}

func TestAppendRowRejectsWrongArity(t *testing.T) {
	// This guard is what makes column drift between the header and the data
	// impossible; the original code wrote the two independently.
	book := newTestWorkbook(t)
	sheet, err := book.AddSheet("Admins", twoColumns)
	if err != nil {
		t.Fatalf("AddSheet: %v", err)
	}

	if err := sheet.AppendRow("only-one-value"); err == nil {
		t.Error("a row with too few values was accepted")
	}
	if err := sheet.AppendRow("a", "b", "c"); err == nil {
		t.Error("a row with too many values was accepted")
	}
	if sheet.Rows() != 0 {
		t.Errorf("rejected rows still advanced the cursor: rows = %d", sheet.Rows())
	}
}

func TestRowsAreWrittenSequentiallyWithoutOverwriting(t *testing.T) {
	// The regression this covers: earlier versions tracked the row number in a
	// caller-managed counter that was not advanced on every branch, so later rows
	// overwrote earlier ones and data vanished from the middle of the report.
	book := newTestWorkbook(t)
	sheet, err := book.AddSheet("Members", twoColumns)
	if err != nil {
		t.Fatalf("AddSheet: %v", err)
	}

	const rows = 25
	for i := range rows {
		if err := sheet.AppendRow(cellValue(i), "MEMBER"); err != nil {
			t.Fatalf("AppendRow: %v", err)
		}
	}
	if sheet.Rows() != rows {
		t.Fatalf("Rows() = %d, want %d", sheet.Rows(), rows)
	}
	if err := sheet.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	path := filepath.Join(t.TempDir(), "report.xlsx")
	if err := book.Save(path, 0o600, true); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Read the workbook back: only the persisted file proves nothing was lost.
	reopened, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	got, err := reopened.GetRows("Members")
	if err != nil {
		t.Fatalf("GetRows: %v", err)
	}
	if len(got) != rows+1 {
		t.Fatalf("the sheet has %d rows, want %d data rows plus a header", len(got), rows)
	}
	if got[0][0] != "E-MAIL" {
		t.Errorf("header cell = %q, want E-MAIL", got[0][0])
	}
	for i := range rows {
		want := cellValue(i)
		if got[i+1][0] != want {
			t.Errorf("row %d holds %q, want %q", i+2, got[i+1][0], want)
		}
	}
}

func cellValue(index int) string {
	return "user" + string(rune('a'+index%26)) + "-" + string(rune('0'+index/26)) + "@example.com"
}

func TestLimitRowsTruncatesAndReports(t *testing.T) {
	book := newTestWorkbook(t)
	sheet, err := book.AddSheet("Capped", twoColumns)
	if err != nil {
		t.Fatalf("AddSheet: %v", err)
	}
	sheet.LimitRows(3)

	for i := range 10 {
		if err := sheet.AppendRow(cellValue(i), "MEMBER"); err != nil {
			t.Fatalf("AppendRow: %v", err)
		}
	}

	if sheet.Rows() != 3 {
		t.Errorf("Rows() = %d, want the cap of 3", sheet.Rows())
	}
	if !sheet.Truncated() {
		t.Error("Truncated() = false after dropping rows")
	}
	if !sheet.Full() {
		t.Error("Full() = false at the cap")
	}
}

func TestSaveIsAtomicAndRespectsPermissions(t *testing.T) {
	book := newTestWorkbook(t)
	sheet, err := book.AddSheet("Groups", twoColumns)
	if err != nil {
		t.Fatalf("AddSheet: %v", err)
	}
	if err := sheet.AppendRow("team@example.com", "OWNER"); err != nil {
		t.Fatalf("AppendRow: %v", err)
	}

	dir := t.TempDir()
	// A nested directory that does not exist yet must be created.
	path := filepath.Join(dir, "out", "report.xlsx")
	if err := book.Save(path, 0o600, true); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("output mode = 0%o, want 0600 (the report holds personal data)", perm)
	}

	// No temporary files may be left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read output directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".aegisheet-") {
			t.Errorf("a temporary file was left behind: %s", entry.Name())
		}
	}
}

func TestSaveRefusesToOverwriteWhenNotPermitted(t *testing.T) {
	book := newTestWorkbook(t)
	if _, err := book.AddSheet("Groups", twoColumns); err != nil {
		t.Fatalf("AddSheet: %v", err)
	}

	path := filepath.Join(t.TempDir(), "report.xlsx")
	if err := os.WriteFile(path, []byte("previous report"), 0o600); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	err := book.Save(path, 0o600, false)
	if !errors.Is(err, ErrOutputExists) {
		t.Fatalf("Save error = %v, want ErrOutputExists", err)
	}

	// The existing file must be untouched.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read existing file: %v", err)
	}
	if string(data) != "previous report" {
		t.Error("the refused save modified the existing file")
	}
}

func TestSaveDropsThePlaceholderSheet(t *testing.T) {
	book := newTestWorkbook(t)
	if _, err := book.AddSheet("Groups", twoColumns); err != nil {
		t.Fatalf("AddSheet: %v", err)
	}

	path := filepath.Join(t.TempDir(), "report.xlsx")
	if err := book.Save(path, 0o600, true); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	sheets := reopened.GetSheetList()
	if len(sheets) != 1 || sheets[0] != "Groups" {
		t.Errorf("workbook sheets = %v, want only the report sheet", sheets)
	}
}

func TestFinalizeOnEmptySheetSucceeds(t *testing.T) {
	// "No users without 2FA" is a legitimate, and welcome, result.
	book := newTestWorkbook(t)
	sheet, err := book.AddSheet("2FA", twoColumns)
	if err != nil {
		t.Fatalf("AddSheet: %v", err)
	}
	if err := sheet.Finalize(); err != nil {
		t.Fatalf("Finalize on an empty sheet: %v", err)
	}
}
