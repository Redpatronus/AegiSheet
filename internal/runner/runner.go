// Package runner orchestrates the configured report tasks.
//
// It owns the parts of the lifecycle that must behave identically for every
// report: creating each task's execution context, deciding what happens when one
// fails, finalising worksheets, and producing the run summary that determines the
// process exit code.
//
// The default policy is deliberate. A failing task is recorded and the run
// continues, because the tasks are independent and one missing API scope should
// not cost an operator the other six sheets. The failure is still visible: it is
// logged at error level, it appears in the summary, and it changes the exit code
// so that a scheduler notices. -fail-fast switches to stopping at the first error.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/gsuite"
	"github.com/Redpatronus/AegiSheet/internal/tasks"
	"github.com/Redpatronus/AegiSheet/internal/xlsx"
)

// Runner executes the report sections of one configuration.
type Runner struct {
	cfg      *config.Config
	services *gsuite.Services
	book     *xlsx.Workbook
	log      *slog.Logger

	// lookup resolves a task identifier to its implementation. It is a field
	// rather than a direct call so that the orchestration policy — what happens
	// when a section fails, when the context ends, when -fail-fast is set — can be
	// tested with stand-in tasks that touch no network. Production always uses
	// tasks.Lookup.
	lookup func(config.TaskID) (tasks.Task, bool)
}

// New builds a Runner.
func New(cfg *config.Config, services *gsuite.Services, book *xlsx.Workbook, log *slog.Logger) *Runner {
	return &Runner{
		cfg:      cfg,
		services: services,
		book:     book,
		log:      log,
		lookup:   tasks.Lookup,
	}
}

// TaskResult records the outcome of one section.
type TaskResult struct {
	Section   string
	Sheet     string
	Task      config.TaskID
	Rows      int
	Duration  time.Duration
	Truncated bool
	Err       error
}

// Status renders the outcome for logs and summaries.
func (r TaskResult) Status() string {
	if r.Err != nil {
		return "failed"
	}
	return "ok"
}

// Summary is the outcome of a whole run.
type Summary struct {
	Started   time.Time
	Duration  time.Duration
	Results   []TaskResult
	Succeeded int
	Failed    int
	// Aborted is set when the run stopped early, through cancellation or
	// -fail-fast, leaving configured sections unexecuted.
	Aborted bool
}

// Rows totals the data rows produced across all sections.
func (s Summary) Rows() int {
	total := 0
	for _, result := range s.Results {
		total += result.Rows
	}
	return total
}

// Err joins the task failures into a single error, or returns nil.
func (s Summary) Err() error {
	var errs []error
	for _, result := range s.Results {
		if result.Err != nil {
			errs = append(errs, fmt.Errorf("section %q: %w", result.Section, result.Err))
		}
	}
	return errors.Join(errs...)
}

// LogAttrs renders the summary for a single closing log record.
func (s Summary) LogAttrs() []slog.Attr {
	sections := make([]string, 0, len(s.Results))
	for _, result := range s.Results {
		sections = append(sections, fmt.Sprintf("%s=%s/%d", result.Section, result.Status(), result.Rows))
	}
	return []slog.Attr{
		// Milliseconds rather than a Duration: the JSON handler would emit raw
		// nanoseconds, and an integer of milliseconds is what dashboards aggregate.
		slog.Int64("duration_ms", s.Duration.Milliseconds()),
		slog.Int("tasks_succeeded", s.Succeeded),
		slog.Int("tasks_failed", s.Failed),
		slog.Int("rows", s.Rows()),
		slog.Bool("aborted", s.Aborted),
		slog.String("results", strings.Join(sections, " ")),
	}
}

// Run executes every configured section in file order.
//
// The returned error is non-nil only when the run was abandoned — the context
// ended, or -fail-fast tripped. Individual task failures are reported through the
// summary so that the caller can still write the sheets that did succeed.
func (r *Runner) Run(ctx context.Context) (*Summary, error) {
	started := time.Now()
	summary := &Summary{Started: started, Results: make([]TaskResult, 0, len(r.cfg.Tasks))}

	r.log.Info("starting report run",
		slog.Int("sections", len(r.cfg.Tasks)),
		slog.Bool("fail_fast", r.cfg.Run.FailFast))

	for _, spec := range r.cfg.Tasks {
		// Stop promptly on shutdown rather than starting another section that will
		// only fail on its first API call.
		if err := ctx.Err(); err != nil {
			summary.Aborted = true
			summary.Duration = time.Since(started)
			return summary, err
		}

		result, err := r.runTask(ctx, spec)
		summary.Results = append(summary.Results, result)
		if result.Err != nil {
			summary.Failed++
		} else {
			summary.Succeeded++
		}

		if err != nil {
			// Abandonment: cancellation, or fail-fast on a task error.
			summary.Aborted = true
			summary.Duration = time.Since(started)
			return summary, err
		}
	}

	summary.Duration = time.Since(started)
	return summary, nil
}

// runTask executes one section.
//
// It returns the section's result plus, separately, an error that should abandon
// the whole run. Splitting the two lets the caller distinguish "this report
// failed" from "stop everything".
func (r *Runner) runTask(ctx context.Context, spec config.TaskSpec) (TaskResult, error) {
	log := r.log.With(
		slog.String("section", spec.Section),
		slog.String("task", string(spec.ID)),
	)

	result := TaskResult{
		Section: spec.Section,
		Sheet:   spec.Sheet,
		Task:    spec.ID,
	}

	task, ok := r.lookup(spec.ID)
	if !ok {
		// Configuration validation accepts any name in the vocabulary, so reaching
		// here means a task is declared but not implemented — a build-time mistake
		// that the tasks package tests are designed to catch.
		result.Err = fmt.Errorf("task %q is declared but not implemented", spec.ID)
		log.Error("cannot execute section", slog.String("error", result.Err.Error()))
		return result, nil
	}

	run := &tasks.Run{
		Spec:      spec,
		Cfg:       r.cfg,
		Directory: r.services.Directory,
		Reports:   r.services.Reports,
		Book:      r.book,
		Log:       log,
	}

	log.Info("section started", slog.String("sheet", spec.Sheet))
	started := time.Now()
	execErr := task.Execute(ctx, run)

	// Finalise regardless of the outcome: rows already written stay useful, and a
	// sheet without its auto-filter looks broken to the reader.
	if err := run.Finalize(); err != nil && execErr == nil {
		execErr = err
	}

	result.Duration = time.Since(started)
	result.Rows = run.RowsWritten()
	result.Truncated = run.Truncated()
	result.Err = execErr

	if execErr != nil {
		log.Error("section failed",
			slog.Int64("duration_ms", result.Duration.Milliseconds()),
			slog.Int("rows", result.Rows),
			slog.String("error", execErr.Error()))

		// A dead context makes every remaining section pointless, so it always
		// abandons the run whether or not -fail-fast is set.
		if errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) {
			return result, execErr
		}
		if r.cfg.Run.FailFast {
			return result, fmt.Errorf("section %q failed and -fail-fast is set: %w", spec.Section, execErr)
		}
		return result, nil
	}

	log.Info("section completed",
		slog.String("sheet", spec.Sheet),
		slog.Int("rows", result.Rows),
		slog.Int64("duration_ms", result.Duration.Milliseconds()))
	return result, nil
}
