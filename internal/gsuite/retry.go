package gsuite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/api/googleapi"
)

// Retrier re-attempts transient Admin SDK failures with capped exponential
// backoff and full jitter.
//
// Retrying is not optional for this workload. A report over a large tenant issues
// thousands of requests, and the Admin SDK enforces per-second quotas by
// answering 403 rateLimitExceeded or 429; the previous implementation treated any
// error as fatal and terminated the process, discarding all work completed so
// far. Jitter matters because several report tasks hit the same quota bucket, and
// un-jittered backoff makes them retry in lockstep.
type Retrier struct {
	// MaxAttempts is the total number of attempts, including the first one.
	MaxAttempts int
	// BaseDelay is the delay after the first failure.
	BaseDelay time.Duration
	// MaxDelay caps the computed delay.
	MaxDelay time.Duration

	log *slog.Logger
}

// NewRetrier builds a Retrier from the operator-facing settings, where the
// configured value is the number of *retries* rather than of attempts.
func NewRetrier(maxRetries int, base, maxDelay time.Duration, log *slog.Logger) *Retrier {
	if maxRetries < 0 {
		maxRetries = 0
	}
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	if maxDelay < base {
		maxDelay = base
	}
	return &Retrier{
		MaxAttempts: maxRetries + 1,
		BaseDelay:   base,
		MaxDelay:    maxDelay,
		log:         log,
	}
}

// Do runs fn until it succeeds, fails with a non-retryable error, exhausts the
// attempt budget, or the context ends.
//
// operation is a short stable identifier such as "directory.users.list"; it
// appears in logs and in the final error, which is what makes a quota problem
// attributable to a specific call.
func (r *Retrier) Do(ctx context.Context, operation string, fn func(context.Context) error) error {
	var lastErr error

	for attempt := 1; attempt <= r.MaxAttempts; attempt++ {
		// Check cancellation before spending an attempt: a shutdown signal should
		// stop work promptly rather than after one more round trip.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", operation, err)
		}

		err := fn(ctx)
		if err == nil {
			if attempt > 1 {
				r.log.Info("API call succeeded after retrying",
					slog.String("operation", operation), slog.Int("attempt", attempt))
			}
			return nil
		}
		lastErr = err

		// A permanent error (a bad request, a missing scope, a revoked token) will
		// fail identically on every retry; surfacing it immediately keeps the run
		// short and the diagnosis obvious.
		if !IsRetryable(err) {
			return err
		}
		if attempt == r.MaxAttempts {
			break
		}

		delay := r.nextDelay(attempt, err)
		r.log.Warn("retryable API failure, backing off",
			slog.String("operation", operation),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", r.MaxAttempts),
			slog.String("delay", delay.String()),
			slog.String("error", err.Error()))

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			// Both errors are wrapped: a caller needs errors.Is to recognise the
			// cancellation, and the underlying API failure is what explains why the
			// call was still being retried when the run ended.
			return fmt.Errorf("%s: %w (last failure: %w)", operation, ctx.Err(), lastErr)
		case <-timer.C:
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", operation, r.MaxAttempts, lastErr)
}

// nextDelay computes the wait before the next attempt.
//
// A Retry-After header from Google is authoritative and is honoured up to
// MaxDelay. Otherwise the delay doubles per attempt and is then jittered across
// the lower half of the interval, which spreads concurrent retries out instead of
// synchronising them.
func (r *Retrier) nextDelay(attempt int, err error) time.Duration {
	if server, ok := retryAfter(err); ok {
		return min(server, r.MaxDelay)
	}

	delay := r.BaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= r.MaxDelay {
			delay = r.MaxDelay
			break
		}
	}

	half := delay / 2
	if half <= 0 {
		return delay
	}
	// math/rand is the correct generator here and crypto/rand would be the wrong
	// one: the value is a timing nudge that desynchronises concurrent retries, not
	// a secret, and nothing about the run's security depends on it being
	// unpredictable. It is also drawn on every retry of every call.
	// #nosec G404 -- backoff jitter is a scheduling concern, not a security one
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// IsRetryable classifies an error as transient.
//
// The Admin SDK signals rate limiting with 403 plus a reason code as well as with
// 429, so the HTTP status alone is not sufficient. Network-level failures are
// retried because a report is a long-lived sequence of requests and a single
// reset connection should not discard it.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Context errors mean the deadline or the operator ended the run; retrying
	// cannot succeed.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case http.StatusTooManyRequests, // 429 quota exceeded
			http.StatusInternalServerError, // 500 transient backend fault
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503 backend error / backoff requested
			http.StatusGatewayTimeout:      // 504
			return true
		case http.StatusForbidden:
			// 403 is overloaded: it covers both "you lack the scope" (permanent)
			// and "you are going too fast" (transient). Only the rate limit
			// reasons are retryable.
			for _, item := range apiErr.Errors {
				switch item.Reason {
				case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded", "backendError":
					return true
				}
			}
			return false
		default:
			return false
		}
	}

	// Timeouts and temporary DNS failures.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// Connections dropped mid-response.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	return false
}

// retryAfter extracts a server-specified backoff from an API error, accepting
// both the delta-seconds and the HTTP-date forms of the header.
func retryAfter(err error) (time.Duration, bool) {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Header == nil {
		return 0, false
	}
	value := apiErr.Header.Get("Retry-After")
	if value == "" {
		return 0, false
	}

	if seconds, convErr := strconv.Atoi(value); convErr == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, convErr := http.ParseTime(value); convErr == nil {
		if delay := time.Until(when); delay > 0 {
			return delay, true
		}
	}
	return 0, false
}
