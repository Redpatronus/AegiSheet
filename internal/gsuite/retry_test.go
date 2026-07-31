package gsuite

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/Redpatronus/AegiSheet/internal/logging"
)

// timeoutError is a net.Error that reports a timeout, as a stalled connection does.
type timeoutError struct{}

func (timeoutError) Error() string   { return "simulated network timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error", err: nil, want: false},
		{
			name: "429 quota exceeded",
			err:  &googleapi.Error{Code: http.StatusTooManyRequests},
			want: true,
		},
		{
			name: "503 backend unavailable",
			err:  &googleapi.Error{Code: http.StatusServiceUnavailable},
			want: true,
		},
		{
			name: "500 internal error",
			err:  &googleapi.Error{Code: http.StatusInternalServerError},
			want: true,
		},
		{
			// The Admin SDK signals rate limiting with 403 as well as 429, which is the
			// case a naive status check gets wrong.
			name: "403 rate limit",
			err: &googleapi.Error{
				Code:   http.StatusForbidden,
				Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}},
			},
			want: true,
		},
		{
			name: "403 missing scope",
			err: &googleapi.Error{
				Code:   http.StatusForbidden,
				Errors: []googleapi.ErrorItem{{Reason: "insufficientPermissions"}},
			},
			want: false,
		},
		{
			name: "401 invalid credentials",
			err:  &googleapi.Error{Code: http.StatusUnauthorized},
			want: false,
		},
		{
			name: "404 unknown user",
			err:  &googleapi.Error{Code: http.StatusNotFound},
			want: false,
		},
		{
			name: "400 bad query",
			err:  &googleapi.Error{Code: http.StatusBadRequest},
			want: false,
		},
		{
			name: "wrapped retryable error",
			err:  fmt.Errorf("list users: %w", &googleapi.Error{Code: http.StatusServiceUnavailable}),
			want: true,
		},
		{
			name: "network timeout",
			err:  &net.OpError{Op: "read", Err: timeoutError{}},
			want: true,
		},
		{
			name: "connection reset",
			err:  fmt.Errorf("read tcp: %w", syscall.ECONNRESET),
			want: true,
		},
		{
			// The run is over; retrying can only delay the exit.
			name: "context cancelled",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "deadline exceeded",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "unclassified error",
			err:  errors.New("something unexpected"),
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRetryable(test.err); got != test.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestRetryAfterHeader(t *testing.T) {
	err := &googleapi.Error{
		Code:   http.StatusTooManyRequests,
		Header: http.Header{"Retry-After": []string{"7"}},
	}
	delay, ok := retryAfter(err)
	if !ok {
		t.Fatal("a Retry-After header was ignored")
	}
	if delay != 7*time.Second {
		t.Errorf("delay = %v, want 7s", delay)
	}

	if _, ok := retryAfter(&googleapi.Error{Code: http.StatusTooManyRequests}); ok {
		t.Error("a delay was reported without a Retry-After header")
	}
	if _, ok := retryAfter(errors.New("plain error")); ok {
		t.Error("a delay was reported for a non-API error")
	}
}

func TestRetrierRetriesUntilSuccess(t *testing.T) {
	retrier := NewRetrier(4, time.Millisecond, 5*time.Millisecond, logging.Discard())

	attempts := 0
	err := retrier.Do(context.Background(), "test.op", func(context.Context) error {
		attempts++
		if attempts < 3 {
			return &googleapi.Error{Code: http.StatusServiceUnavailable}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestRetrierStopsAtTheAttemptBudget(t *testing.T) {
	// maxRetries=2 means three attempts in total.
	retrier := NewRetrier(2, time.Millisecond, 2*time.Millisecond, logging.Discard())

	attempts := 0
	err := retrier.Do(context.Background(), "test.op", func(context.Context) error {
		attempts++
		return &googleapi.Error{Code: http.StatusServiceUnavailable}
	})
	if err == nil {
		t.Fatal("Do reported success after exhausting its budget")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}

	// The operation name must survive into the error so a quota problem is
	// attributable to a specific call.
	if !strings.Contains(err.Error(), "test.op") {
		t.Errorf("the error does not name the operation: %v", err)
	}
}

func TestRetrierDoesNotRetryPermanentFailures(t *testing.T) {
	retrier := NewRetrier(5, time.Millisecond, time.Millisecond, logging.Discard())

	attempts := 0
	permanent := &googleapi.Error{Code: http.StatusUnauthorized, Message: "invalid credentials"}
	err := retrier.Do(context.Background(), "test.op", func(context.Context) error {
		attempts++
		return permanent
	})

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	// A permanent error is returned unchanged so callers can inspect it.
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusUnauthorized {
		t.Errorf("error = %v, want the original 401", err)
	}
}

func TestRetrierHonoursCancellation(t *testing.T) {
	retrier := NewRetrier(10, 50*time.Millisecond, time.Second, logging.Discard())

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	err := retrier.Do(ctx, "test.op", func(context.Context) error {
		attempts++
		// Cancel during the first attempt: the wait before the second must abort.
		cancel()
		return &googleapi.Error{Code: http.StatusServiceUnavailable}
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

func TestRetrierDelayGrowsAndIsCapped(t *testing.T) {
	retrier := NewRetrier(6, 100*time.Millisecond, 400*time.Millisecond, logging.Discard())
	transient := &googleapi.Error{Code: http.StatusServiceUnavailable}

	// Jitter spreads each delay over the upper half of the interval, so the bound
	// checked here is the interval itself.
	for attempt, upper := range map[int]time.Duration{
		1: 100 * time.Millisecond,
		2: 200 * time.Millisecond,
		3: 400 * time.Millisecond,
		9: 400 * time.Millisecond, // capped
	} {
		delay := retrier.nextDelay(attempt, transient)
		if delay > upper {
			t.Errorf("attempt %d delay = %v, want at most %v", attempt, delay, upper)
		}
		if delay <= 0 {
			t.Errorf("attempt %d delay = %v, want a positive delay", attempt, delay)
		}
	}
}

func TestClampPageSize(t *testing.T) {
	// The operator sets one page size; each API has its own ceiling.
	if got := clampPageSize(1000, maxUserPageSize); got != maxUserPageSize {
		t.Errorf("clampPageSize(1000, %d) = %d, want %d", maxUserPageSize, got, maxUserPageSize)
	}
	if got := clampPageSize(50, maxUserPageSize); got != 50 {
		t.Errorf("clampPageSize(50, %d) = %d, want 50", maxUserPageSize, got)
	}
	if got := clampPageSize(0, maxGroupPageSize); got != maxGroupPageSize {
		t.Errorf("clampPageSize(0, %d) = %d, want the ceiling", maxGroupPageSize, got)
	}
}
