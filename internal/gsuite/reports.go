package gsuite

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	reports "google.golang.org/api/admin/reports/v1"

	"github.com/Redpatronus/AegiSheet/internal/config"
)

// maxActivityPageSize is the ceiling the Reports API enforces on page size.
const maxActivityPageSize = 1000

// Reports reads the Admin SDK Reports (audit) API.
type Reports struct {
	svc     *reports.Service
	api     config.APIConfig
	retrier *Retrier
	log     *slog.Logger
}

func newReports(svc *reports.Service, api config.APIConfig, retrier *Retrier, log *slog.Logger) *Reports {
	return &Reports{svc: svc, api: api, retrier: retrier, log: log}
}

// ActivityQuery describes an audit log query.
type ActivityQuery struct {
	// UserKey is an e-mail address, a user ID, or "all" for the whole tenant.
	UserKey string
	// AppName is the Reports application, e.g. "login", "admin" or "drive".
	AppName string
	// EventName optionally narrows the result to a single event type.
	EventName string
	// StartTime and EndTime bound the window. Zero values leave the bound open,
	// in which case the API applies its own retention window.
	StartTime time.Time
	EndTime   time.Time
}

// ForEachActivity visits every audit activity matching the query, following the
// page chain.
//
// Audit logs are the largest data source this tool reads — a busy tenant produces
// millions of events — so the caller streams them into the worksheet rather than
// accumulating them, and can end iteration by returning ErrStop.
func (r *Reports) ForEachActivity(
	ctx context.Context,
	query ActivityQuery,
	visit func(*reports.Activity) error,
) error {
	if query.UserKey == "" {
		// The path parameter is mandatory; the previous implementation left it empty
		// whenever the configuration omitted it, which the API rejected.
		query.UserKey = config.AllUsers
	}
	if query.AppName == "" {
		return errors.New("an application name is required to query audit activities")
	}

	pageSize := clampPageSize(r.api.PageSize, maxActivityPageSize)

	var (
		pageToken string
		pages     int
		visited   int
	)
	for {
		var page *reports.Activities
		err := r.retrier.Do(ctx, "reports.activities.list", func(ctx context.Context) error {
			call := r.svc.Activities.
				List(query.UserKey, query.AppName).
				MaxResults(pageSize).
				Context(ctx)

			if query.EventName != "" {
				call = call.EventName(query.EventName)
			}
			// The API expects RFC 3339 timestamps.
			if !query.StartTime.IsZero() {
				call = call.StartTime(query.StartTime.UTC().Format(time.RFC3339))
			}
			if !query.EndTime.IsZero() {
				call = call.EndTime(query.EndTime.UTC().Format(time.RFC3339))
			}
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}

			var callErr error
			page, callErr = call.Do()
			return callErr
		})
		if err != nil {
			return fmt.Errorf("list %s activities (page %d): %w", query.AppName, pages+1, err)
		}
		pages++

		for _, activity := range page.Items {
			visited++
			if err := visit(activity); err != nil {
				if errors.Is(err, ErrStop) {
					r.log.Debug("activity iteration stopped early",
						slog.String("application", query.AppName), slog.Int("activities", visited))
					return nil
				}
				return err
			}
		}

		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	r.log.Debug("activities listed",
		slog.String("application", query.AppName),
		slog.Int("pages", pages),
		slog.Int("activities", visited))
	return nil
}
