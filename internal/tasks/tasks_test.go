package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	reports "google.golang.org/api/admin/reports/v1"

	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/logging"
)

// TestEveryConfiguredTaskIsImplemented guards the seam between the configuration
// vocabulary and the implementations. Configuration validation accepts any name in
// the catalog, so a task declared there without an implementation would only fail
// at run time, after credentials have been used and API calls made.
func TestEveryConfiguredTaskIsImplemented(t *testing.T) {
	implemented := Implemented()

	for _, definition := range config.TaskCatalog() {
		if !slices.Contains(implemented, definition.ID) {
			t.Errorf("task %q is in the configuration catalog but has no implementation", definition.ID)
		}
		task, ok := Lookup(definition.ID)
		if !ok {
			continue
		}
		if task.Execute == nil {
			t.Errorf("task %q has no Execute function", definition.ID)
		}
		if task.Summary == "" {
			t.Errorf("task %q has no summary", definition.ID)
		}
	}

	// And the converse: an implementation that configuration cannot name is dead
	// code.
	for _, id := range implemented {
		if _, ok := config.LookupTask(id); !ok {
			t.Errorf("task %q is implemented but not in the configuration catalog", id)
		}
	}
}

func TestFormatAPITime(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "RFC 3339", in: "2026-03-15T12:34:56Z", want: "2026-03-15 12:34:56"},
		{name: "with fractional seconds", in: "2026-03-15T12:34:56.789Z", want: "2026-03-15 12:34:56"},
		{name: "converted to UTC", in: "2026-03-15T14:34:56+02:00", want: "2026-03-15 12:34:56"},
		{name: "empty", in: "", want: ""},
		{name: "unparseable", in: "not a timestamp", want: ""},
		{
			// The API reports the epoch for accounts that have never signed in;
			// printing 1970 as a login date has misled readers before.
			name: "epoch sentinel",
			in:   "1970-01-01T00:00:00.000Z",
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatAPITime(test.in); got != test.want {
				t.Errorf("formatAPITime(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestFormatEpochMillis(t *testing.T) {
	// Application specific passwords carry Unix milliseconds, not RFC 3339 strings.
	when := time.Date(2026, 3, 15, 12, 34, 56, 0, time.UTC)
	if got, want := formatEpochMillis(when.UnixMilli()), "2026-03-15 12:34:56"; got != want {
		t.Errorf("formatEpochMillis = %q, want %q", got, want)
	}
	for _, zero := range []int64{0, -1} {
		if got := formatEpochMillis(zero); got != "" {
			t.Errorf("formatEpochMillis(%d) = %q, want an empty cell", zero, got)
		}
	}
}

func TestFormatParameterPrecedence(t *testing.T) {
	tests := []struct {
		name string
		in   *reports.ActivityEventsParameters
		want string
	}{
		{name: "nil", in: nil, want: ""},
		{
			name: "string value",
			in:   &reports.ActivityEventsParameters{Name: "login_type", Value: "google_password"},
			want: "google_password",
		},
		{
			name: "multi value",
			in:   &reports.ActivityEventsParameters{Name: "scopes", MultiValue: []string{"drive", "gmail"}},
			want: "drive, gmail",
		},
		{
			name: "multi int value",
			in:   &reports.ActivityEventsParameters{Name: "codes", MultiIntValue: []int64{1, 2, 3}},
			want: "1, 2, 3",
		},
		{
			name: "int value",
			in:   &reports.ActivityEventsParameters{Name: "count", IntValue: 42},
			want: "42",
		},
		{
			name: "bool value",
			in:   &reports.ActivityEventsParameters{Name: "is_suspicious", BoolValue: true},
			want: "true",
		},
		{
			name: "nested message",
			in: &reports.ActivityEventsParameters{
				Name: "target",
				MessageValue: &reports.ActivityEventsParametersMessageValue{
					Parameter: []*reports.NestedParameter{
						{Name: "id", Value: "abc"},
						{Name: "size", IntValue: 7},
					},
				},
			},
			want: "id=abc; size=7",
		},
		{
			// A zero integer, a false boolean and an absent field are indistinguishable
			// once the JSON has dropped the zero value, so nothing is invented.
			name: "empty parameter",
			in:   &reports.ActivityEventsParameters{Name: "unset"},
			want: "",
		},
		{
			// A string value takes precedence over the typed alternatives.
			name: "value wins over int",
			in:   &reports.ActivityEventsParameters{Name: "mixed", Value: "text", IntValue: 9},
			want: "text",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatParameter(test.in); got != test.want {
				t.Errorf("formatParameter = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "all"); got != "all" {
		t.Errorf("firstNonEmpty = %q, want all", got)
	}
	if got := firstNonEmpty("admin@example.com", "all"); got != "admin@example.com" {
		t.Errorf("firstNonEmpty = %q, want admin@example.com", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("firstNonEmpty() = %q, want an empty string", got)
	}
}

func TestFanOutPreservesInputOrder(t *testing.T) {
	// A reproducible workbook depends on this: concurrent completion must not
	// reorder the rows.
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}

	results, err := fanOut(context.Background(), items, 8, logging.Discard(),
		func(_ context.Context, item int) ([]string, error) {
			// Make the later items finish first, which is what would scramble the
			// output if ordering depended on completion.
			time.Sleep(time.Duration(len(items)-item) * time.Millisecond / 10)
			return []string{fmt.Sprintf("item-%d", item)}, nil
		})
	if err != nil {
		t.Fatalf("fanOut: %v", err)
	}
	if len(results) != len(items) {
		t.Fatalf("got %d result slots, want %d", len(results), len(items))
	}
	for i, result := range results {
		want := fmt.Sprintf("item-%d", i)
		if len(result) != 1 || result[0] != want {
			t.Errorf("slot %d = %v, want [%s]", i, result, want)
		}
	}
}

func TestFanOutKeepsPartialResultsOnFailure(t *testing.T) {
	// Per-user calls fail individually for accounts the caller cannot see. Losing
	// the whole report over one of them would be a poor trade.
	items := []int{0, 1, 2, 3}

	results, err := fanOut(context.Background(), items, 2, logging.Discard(),
		func(_ context.Context, item int) ([]string, error) {
			if item%2 == 1 {
				return nil, fmt.Errorf("item %d is not accessible", item)
			}
			return []string{fmt.Sprintf("item-%d", item)}, nil
		})

	if err == nil {
		t.Fatal("fanOut reported success despite failures")
	}
	if !strings.Contains(err.Error(), "2 of the per-item calls failed") {
		t.Errorf("the error does not summarise the failure count: %v", err)
	}
	for _, index := range []int{0, 2} {
		if len(results[index]) != 1 {
			t.Errorf("slot %d lost its successful result: %v", index, results[index])
		}
	}
}

func TestFanOutSummarisesLargeFailureCounts(t *testing.T) {
	// Thousands of joined errors would swamp the log.
	items := make([]int, 40)
	_, err := fanOut(context.Background(), items, 4, logging.Discard(),
		func(_ context.Context, item int) ([]string, error) {
			return nil, errors.New("unavailable")
		})
	if err == nil {
		t.Fatal("fanOut reported success despite failures")
	}
	if !strings.Contains(err.Error(), "further failures") {
		t.Errorf("the error was not condensed: %v", err)
	}
	if lines := strings.Count(err.Error(), "unavailable"); lines > maxReportedErrors {
		t.Errorf("the error lists %d individual failures, want at most %d", lines, maxReportedErrors)
	}
}

func TestFanOutStopsOnCancellation(t *testing.T) {
	items := make([]int, 100)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fanOut(ctx, items, 4, logging.Discard(),
		func(context.Context, int) ([]string, error) { return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestFanOutHandlesEmptyInput(t *testing.T) {
	results, err := fanOut(context.Background(), []int(nil), 4, logging.Discard(),
		func(context.Context, int) ([]string, error) { return nil, errors.New("must not be called") })
	if err != nil {
		t.Errorf("fanOut over an empty list: %v", err)
	}
	if results != nil {
		t.Errorf("results = %v, want nil", results)
	}
}
