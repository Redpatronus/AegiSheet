package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// progressInterval is how often a fan-out reports how far it has got. Reports
// over a large tenant take minutes; silence for minutes is indistinguishable from
// a hang.
const progressInterval = 250

// maxReportedErrors bounds how many individual failures appear in the error
// returned by fanOut. Thousands of joined errors are unreadable and can dwarf the
// rest of the log.
const maxReportedErrors = 5

// fanOut applies fetch to every item with bounded concurrency, returning the
// results in input order.
//
// Several reports are per-user calls repeated across the whole directory
// (application specific passwords, third-party tokens) or per-group calls repeated
// across every group. Performing them one at a time is dominated by round-trip
// latency: on a tenant with ten thousand users, a 150 ms call means twenty-five
// minutes of waiting. Bounding the concurrency keeps that in check without
// tripping the per-second API quota, and the operator can tune it with
// -concurrency.
//
// Results keep their input order so that the generated workbook is byte-stable
// across runs over unchanged data — a property that makes diffing two reports
// meaningful.
//
// A failure on one item does not abandon the rest: partial output plus a clear
// error beats no output at all, particularly for the per-user calls, which fail
// individually for accounts the caller cannot see. The returned error summarises
// the failures; results for the items that succeeded are still populated.
func fanOut[T any, R any](
	ctx context.Context,
	items []T,
	workers int,
	log *slog.Logger,
	fetch func(context.Context, T) ([]R, error),
) ([][]R, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}

	results := make([][]R, len(items))
	failures := make([]error, len(items))

	// A buffered channel is the semaphore: acquiring a slot before spawning bounds
	// the number of in-flight requests.
	slots := make(chan struct{}, workers)
	var (
		wg        sync.WaitGroup
		completed atomic.Int64
	)

	for index, item := range items {
		// Stop dispatching once the run is cancelled; already-started work finishes
		// and reports its own context error.
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		slots <- struct{}{}
		go func(index int, item T) {
			defer wg.Done()
			defer func() { <-slots }()

			result, err := fetch(ctx, item)
			results[index] = result
			failures[index] = err

			if done := completed.Add(1); done%progressInterval == 0 {
				log.Info("fan-out progress",
					slog.Int64("completed", done), slog.Int("total", len(items)))
			}
		}(index, item)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, summarizeErrors(failures)
}

// summarizeErrors condenses per-item failures into one reportable error.
func summarizeErrors(errs []error) error {
	reported := make([]error, 0, maxReportedErrors)
	total := 0
	for _, err := range errs {
		if err == nil {
			continue
		}
		total++
		if len(reported) < maxReportedErrors {
			reported = append(reported, err)
		}
	}
	if total == 0 {
		return nil
	}
	if hidden := total - len(reported); hidden > 0 {
		reported = append(reported, fmt.Errorf("and %d further failures", hidden))
	}
	return fmt.Errorf("%d of the per-item calls failed: %w", total, errors.Join(reported...))
}
