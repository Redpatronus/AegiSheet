// Package gsuite is the Google Workspace access layer: authentication, transport
// instrumentation, retry policy and paginated iteration over the Admin SDK.
//
// Report tasks consume this package and never touch the generated Google clients
// directly. That boundary is what lets pagination, retrying and PII-safe logging
// be implemented once instead of in every report.
//
// The most consequential difference from earlier versions is pagination. Every
// list call used to fetch a single page — MaxResults(500) for users,
// MaxResults(100) for groups and activities — and ignore NextPageToken. Any
// tenant larger than one page produced a report that looked complete and was
// silently truncated, which for a security report is worse than an error. Every
// iterator here follows the page chain to the end.
package gsuite

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	admin "google.golang.org/api/admin/directory/v1"
	reports "google.golang.org/api/admin/reports/v1"
	"google.golang.org/api/option"

	"github.com/Redpatronus/AegiSheet/internal/config"
)

// Services bundles the Admin SDK endpoints AegiSheet uses.
type Services struct {
	Directory *Directory
	Reports   *Reports
}

// NewServices constructs the Admin SDK clients on top of an already
// authenticated HTTP client.
//
// Passing the HTTP client explicitly (rather than letting the generated clients
// discover credentials) keeps a single transport in play, so the User-Agent,
// timeout, request logging and token refresh behaviour are identical for every
// endpoint.
func NewServices(
	ctx context.Context,
	client *http.Client,
	api config.APIConfig,
	log *slog.Logger,
) (*Services, error) {
	if client == nil {
		return nil, fmt.Errorf("an authenticated HTTP client is required")
	}

	// One retry policy is shared by both endpoints: they draw on the same
	// per-project quota, so they should back off with the same parameters.
	retrier := NewRetrier(api.MaxRetries, api.RetryBaseDelay, api.RetryMaxDelay, log)

	directoryService, err := admin.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("initialise Admin SDK Directory service: %w", err)
	}
	reportsService, err := reports.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("initialise Admin SDK Reports service: %w", err)
	}

	return &Services{
		Directory: newDirectory(directoryService, api, retrier, log.With(slog.String("api", "directory"))),
		Reports:   newReports(reportsService, api, retrier, log.With(slog.String("api", "reports"))),
	}, nil
}

// clampPageSize keeps a requested page size within what an API accepts.
//
// The Admin SDK rejects oversized page sizes outright, and each resource has its
// own ceiling, so the single operator-facing -page-size is adjusted per call site
// instead of being trusted.
func clampPageSize(requested, ceiling int64) int64 {
	if requested < 1 {
		return ceiling
	}
	return min(requested, ceiling)
}
