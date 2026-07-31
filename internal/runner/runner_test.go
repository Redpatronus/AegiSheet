package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/gsuite"
	"github.com/Redpatronus/AegiSheet/internal/logging"
	"github.com/Redpatronus/AegiSheet/internal/tasks"
	"github.com/Redpatronus/AegiSheet/internal/xlsx"
)

// fakeTasks builds a lookup over stand-in implementations, so the orchestration
// policy can be exercised without any API access.
func fakeTasks(implementations map[config.TaskID]func(context.Context, *tasks.Run) error) func(config.TaskID) (tasks.Task, bool) {
	return func(id config.TaskID) (tasks.Task, bool) {
		execute, ok := implementations[id]
		if !ok {
			return tasks.Task{}, false
		}
		return tasks.Task{ID: id, Summary: "stand-in", Execute: execute}, true
	}
}

// writeOneRow is a stand-in task that produces a single row.
func writeOneRow(_ context.Context, run *tasks.Run) error {
	sheet, err := run.Sheet([]xlsx.Column{xlsx.Col("VALUE", 20)})
	if err != nil {
		return err
	}
	return sheet.AppendRow("data")
}

// failAfterOneRow writes a row and then fails, as a task interrupted partway
// through would.
func failAfterOneRow(ctx context.Context, run *tasks.Run) error {
	if err := writeOneRow(ctx, run); err != nil {
		return err
	}
	return errors.New("the API rejected the request")
}

func newTestRunner(
	t *testing.T,
	cfg *config.Config,
	implementations map[config.TaskID]func(context.Context, *tasks.Run) error,
) (*Runner, *xlsx.Workbook) {
	t.Helper()

	book, err := xlsx.New(logging.Discard())
	if err != nil {
		t.Fatalf("create workbook: %v", err)
	}
	t.Cleanup(func() { _ = book.Close() })

	// An empty Services is enough: the stand-in tasks never reach the API, but the
	// runner still populates the accessors on every tasks.Run it builds.
	runner := New(cfg, &gsuite.Services{}, book, logging.Discard())
	runner.lookup = fakeTasks(implementations)
	return runner, book
}

// specsFor builds enabled task specs whose section name matches the identifier.
func specsFor(ids ...config.TaskID) []config.TaskSpec {
	specs := make([]config.TaskSpec, 0, len(ids))
	for _, id := range ids {
		specs = append(specs, config.TaskSpec{
			Section: string(id),
			Sheet:   string(id),
			ID:      id,
			Enabled: true,
		})
	}
	return specs
}

func TestRunExecutesEverySection(t *testing.T) {
	cfg := config.Default()
	cfg.Tasks = specsFor(config.TaskGroups, config.TaskAdmins)

	runner, book := newTestRunner(t, cfg, map[config.TaskID]func(context.Context, *tasks.Run) error{
		config.TaskGroups: writeOneRow,
		config.TaskAdmins: writeOneRow,
	})

	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Succeeded != 2 || summary.Failed != 0 {
		t.Errorf("succeeded=%d failed=%d, want 2 and 0", summary.Succeeded, summary.Failed)
	}
	if summary.Rows() != 2 {
		t.Errorf("rows = %d, want 2", summary.Rows())
	}
	if summary.Aborted {
		t.Error("the run reported itself aborted")
	}
	if len(book.Sheets()) != 2 {
		t.Errorf("the workbook has %d sheets, want 2", len(book.Sheets()))
	}
	if err := summary.Err(); err != nil {
		t.Errorf("summary reported an error: %v", err)
	}
}

func TestRunContinuesAfterAFailingSection(t *testing.T) {
	// The default policy: one broken report must not cost the others. Section order
	// puts the failure first so the following section proves it continued.
	cfg := config.Default()
	cfg.Tasks = specsFor(config.TaskGroups, config.TaskAdmins)

	runner, book := newTestRunner(t, cfg, map[config.TaskID]func(context.Context, *tasks.Run) error{
		config.TaskGroups: failAfterOneRow,
		config.TaskAdmins: writeOneRow,
	})

	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned an abandonment error for a recoverable failure: %v", err)
	}
	if summary.Succeeded != 1 || summary.Failed != 1 {
		t.Errorf("succeeded=%d failed=%d, want 1 and 1", summary.Succeeded, summary.Failed)
	}
	if summary.Err() == nil {
		t.Error("the summary hides the failure")
	}

	// Rows written before the failure are kept, and the sheet was still finalised.
	if summary.Rows() != 2 {
		t.Errorf("rows = %d, want 2 (one from each section, including the failed one)", summary.Rows())
	}
	if len(book.Sheets()) != 2 {
		t.Errorf("the workbook has %d sheets, want 2", len(book.Sheets()))
	}
}

func TestRunFailFastStopsAtTheFirstFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Run.FailFast = true
	cfg.Tasks = specsFor(config.TaskGroups, config.TaskAdmins)

	secondRan := false
	runner, _ := newTestRunner(t, cfg, map[config.TaskID]func(context.Context, *tasks.Run) error{
		config.TaskGroups: failAfterOneRow,
		config.TaskAdmins: func(ctx context.Context, run *tasks.Run) error {
			secondRan = true
			return writeOneRow(ctx, run)
		},
	})

	summary, err := runner.Run(context.Background())
	if err == nil {
		t.Fatal("Run reported success with -fail-fast and a failing section")
	}
	if secondRan {
		t.Error("the second section ran despite -fail-fast")
	}
	if !summary.Aborted {
		t.Error("the summary does not record the run as aborted")
	}
	if summary.Failed != 1 {
		t.Errorf("failed = %d, want 1", summary.Failed)
	}
}

func TestRunAbandonsOnCancellationEvenWithoutFailFast(t *testing.T) {
	// A dead context makes every remaining section pointless, so cancellation always
	// abandons the run.
	cfg := config.Default()
	cfg.Run.FailFast = false
	cfg.Tasks = specsFor(config.TaskGroups, config.TaskAdmins)

	ctx, cancel := context.WithCancel(context.Background())
	secondRan := false

	runner, _ := newTestRunner(t, cfg, map[config.TaskID]func(context.Context, *tasks.Run) error{
		config.TaskGroups: func(context.Context, *tasks.Run) error {
			cancel()
			return context.Canceled
		},
		config.TaskAdmins: func(ctx context.Context, run *tasks.Run) error {
			secondRan = true
			return nil
		},
	})

	summary, err := runner.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if secondRan {
		t.Error("a section ran after the context was cancelled")
	}
	if !summary.Aborted {
		t.Error("the summary does not record the run as aborted")
	}
}

func TestRunReportsUnimplementedTaskWithoutAbandoning(t *testing.T) {
	cfg := config.Default()
	cfg.Tasks = specsFor(config.TaskGroups, config.TaskAdmins)

	// Only one of the two identifiers resolves.
	runner, _ := newTestRunner(t, cfg, map[config.TaskID]func(context.Context, *tasks.Run) error{
		config.TaskAdmins: writeOneRow,
	})

	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Failed != 1 || summary.Succeeded != 1 {
		t.Errorf("succeeded=%d failed=%d, want 1 and 1", summary.Succeeded, summary.Failed)
	}
	if summary.Err() == nil {
		t.Fatal("the missing implementation was not reported")
	}
	if got := summary.Err().Error(); !strings.Contains(got, "not implemented") {
		t.Errorf("the error does not explain the problem: %s", got)
	}
}

func TestSummaryLogAttrs(t *testing.T) {
	summary := Summary{
		Succeeded: 1,
		Failed:    1,
		Results: []TaskResult{
			{Section: "Groups", Rows: 3},
			{Section: "Admins", Rows: 1, Err: errors.New("boom")},
		},
	}

	attrs := summary.LogAttrs()
	found := map[string]bool{}
	for _, attr := range attrs {
		found[attr.Key] = true
	}
	for _, key := range []string{"duration_ms", "tasks_succeeded", "tasks_failed", "rows", "results"} {
		if !found[key] {
			t.Errorf("the summary log record is missing %q", key)
		}
	}
	if summary.Rows() != 4 {
		t.Errorf("Rows() = %d, want 4", summary.Rows())
	}
}
