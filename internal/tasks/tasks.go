// Package tasks implements the individual reports and the registry that maps a
// configured task name onto its implementation.
//
// A task receives a Run — its own configuration section, the API layer, the
// workbook and a logger already tagged with the section name — and writes one
// worksheet. Tasks do not decide whether to continue after an error, do not
// finalise their sheets and do not save the workbook: package runner owns the
// lifecycle so that error handling is uniform across every report.
package tasks

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/gsuite"
	"github.com/Redpatronus/AegiSheet/internal/xlsx"
)

// Task is one report implementation.
type Task struct {
	// ID is the canonical task identifier from the configuration vocabulary.
	ID config.TaskID
	// Summary is a one-line description, used in diagnostics.
	Summary string
	// Execute performs the report. It must respect ctx and stream its output
	// through Run.Sheet so that row and column bookkeeping stays consistent.
	Execute func(ctx context.Context, run *Run) error
}

// Run is the execution context of a single configured section.
type Run struct {
	// Spec is the section being executed.
	Spec config.TaskSpec
	// Cfg is the full configuration, for settings that are not section-specific
	// (concurrency, for instance).
	Cfg *config.Config
	// Directory and Reports are the Admin SDK accessors.
	Directory *gsuite.Directory
	Reports   *gsuite.Reports
	// Book is the workbook under construction.
	Book *xlsx.Workbook
	// Log is tagged with the section and task names.
	Log *slog.Logger

	// sheets are the worksheets this run created, so the caller can finalise them
	// without every task having to remember to.
	sheets []*xlsx.Sheet
}

// Sheet creates the worksheet for this section and applies the section's row
// limit.
func (r *Run) Sheet(columns []xlsx.Column) (*xlsx.Sheet, error) {
	sheet, err := r.Book.AddSheet(r.Spec.Sheet, columns)
	if err != nil {
		return nil, err
	}
	sheet.LimitRows(r.Spec.MaxRows)
	r.sheets = append(r.sheets, sheet)
	return sheet, nil
}

// Finalize completes every sheet this run created.
//
// It is called even when the task failed: rows already written are still worth
// having, and a sheet without its auto-filter would look broken to the reader.
func (r *Run) Finalize() error {
	for _, sheet := range r.sheets {
		if err := sheet.Finalize(); err != nil {
			return err
		}
	}
	return nil
}

// RowsWritten totals the data rows this run produced.
func (r *Run) RowsWritten() int {
	total := 0
	for _, sheet := range r.sheets {
		total += sheet.Rows()
	}
	return total
}

// Truncated reports whether a row limit discarded data.
func (r *Run) Truncated() bool {
	for _, sheet := range r.sheets {
		if sheet.Truncated() {
			return true
		}
	}
	return false
}

// concurrency returns the worker count for fan-out tasks.
func (r *Run) concurrency() int {
	if r.Cfg == nil || r.Cfg.API.Concurrency < 1 {
		return 1
	}
	return r.Cfg.API.Concurrency
}

// registry holds every implemented task, keyed by canonical identifier.
var registry = buildRegistry(
	usersWithout2FATask,
	suspendedUsersTask,
	adminsTask,
	appPasswordsTask,
	groupsTask,
	groupMembersTask,
	thirdPartyTokensTask,
	auditActivitiesTask,
)

func buildRegistry(tasks ...Task) map[config.TaskID]Task {
	out := make(map[config.TaskID]Task, len(tasks))
	for _, task := range tasks {
		if _, duplicate := out[task.ID]; duplicate {
			// A duplicate registration is a programming error that would silently
			// shadow one report; panicking at init makes it impossible to ship.
			panic(fmt.Sprintf("tasks: duplicate registration for %q", task.ID))
		}
		out[task.ID] = task
	}
	return out
}

// Lookup resolves a canonical task identifier to its implementation.
func Lookup(id config.TaskID) (Task, bool) {
	task, ok := registry[id]
	return task, ok
}

// Implemented returns the identifiers of all implemented tasks, sorted. Used by
// tests to assert that the configuration vocabulary and the implementations stay
// in step.
func Implemented() []config.TaskID {
	ids := make([]config.TaskID, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
