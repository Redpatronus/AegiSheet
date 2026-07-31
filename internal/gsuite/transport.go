package gsuite

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Redpatronus/AegiSheet/internal/logging"
)

// instrumentedTransport adds a stable User-Agent to every outbound request and
// emits one debug record per round trip.
//
// The request path is sanitised before logging. Admin SDK routes embed identity
// in the URL — /admin/directory/v1/users/alice@example.com/asps — so logging the
// raw path would leak personal data through a channel that carries no attribute
// key for the redaction rules to act on.
type instrumentedTransport struct {
	base      http.RoundTripper
	userAgent string
	log       *slog.Logger
}

func (t *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrippers must not modify the request they are given.
	clone := req.Clone(req.Context())
	if t.userAgent != "" {
		clone.Header.Set("User-Agent", t.userAgent)
	}

	start := time.Now()
	resp, err := t.base.RoundTrip(clone)
	elapsed := time.Since(start)

	// Only build the attribute list when it will actually be emitted: these logs
	// are per-request and the report issues thousands of them.
	if t.log.Enabled(req.Context(), slog.LevelDebug) {
		attrs := []slog.Attr{
			slog.String("method", clone.Method),
			slog.String("host", clone.URL.Host),
			slog.String("path", logging.SanitizePath(clone.URL.Path)),
			slog.Int64("elapsed_ms", elapsed.Milliseconds()),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		} else {
			attrs = append(attrs, slog.Int("status", resp.StatusCode))
		}
		t.log.LogAttrs(req.Context(), slog.LevelDebug, "http request", attrs...)
	}

	return resp, err
}

// newBaseHTTPClient builds the transport used for both API calls and OAuth token
// exchanges, so that timeouts, the User-Agent and request logging apply uniformly.
func newBaseHTTPClient(timeout time.Duration, userAgent string, log *slog.Logger) *http.Client {
	return &http.Client{
		// Timeout covers the whole exchange including the body. The Admin SDK
		// responses are small, so a single bound is appropriate and prevents a
		// stalled connection from hanging the run.
		Timeout: timeout,
		Transport: &instrumentedTransport{
			base:      http.DefaultTransport,
			userAgent: userAgent,
			log:       log,
		},
	}
}
