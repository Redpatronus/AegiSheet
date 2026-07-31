package tasks

import (
	"context"
	"log/slog"
	"time"

	reports "google.golang.org/api/admin/reports/v1"

	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/gsuite"
	"github.com/Redpatronus/AegiSheet/internal/xlsx"
)

// auditActivitiesTask reports Admin SDK audit activities for one application.
//
// Output shape: one row per event *parameter*, with the activity context (time,
// actor, address, event) repeated on each row. That keeps every row independently
// filterable, which is how these sheets are read — filter on PARAMETER=login_type
// and the column of values is immediately meaningful.
//
// Two defects in the earlier implementation are fixed here:
//
//   - The row cursor advanced only inside the parameter loop, so an event with no
//     parameters left the cursor where it was and the next event overwrote it.
//     Events without parameters are common (many admin events carry none), so
//     activity was silently lost from the middle of the report.
//   - The context columns were written once per event, before the parameter loop,
//     so the second and subsequent parameters of an event produced rows whose
//     time, actor and event columns were empty.
//
// Both follow from the sheet owning its cursor and from a row being written as a
// complete unit.
var auditActivitiesTask = Task{
	ID:      config.TaskAuditActivities,
	Summary: "Admin SDK Reports audit activities for one application.",
	Execute: func(ctx context.Context, run *Run) error {
		sheet, err := run.Sheet([]xlsx.Column{
			xlsx.Col("TIME (UTC)", 20),
			xlsx.Col("ACTOR E-MAIL", 34),
			xlsx.Col("ACTOR TYPE", 14),
			xlsx.Col("IP ADDRESS", 18),
			xlsx.Col("APPLICATION", 14),
			xlsx.Col("EVENT TYPE", 24),
			xlsx.Col("EVENT NAME", 30),
			xlsx.Col("PARAMETER", 28),
			xlsx.Col("VALUE", 50),
		})
		if err != nil {
			return err
		}

		// The window is resolved against a single "now" so that every page of the
		// query uses identical bounds; recomputing it per page would shift the start
		// time under a long-running report.
		start, end := run.Spec.ResolveWindow(time.Now())

		query := gsuite.ActivityQuery{
			UserKey:   firstNonEmpty(run.Spec.UserKey, config.AllUsers),
			AppName:   run.Spec.AppName,
			EventName: run.Spec.EventName,
			StartTime: start,
			EndTime:   end,
		}
		run.Log.Info("querying audit activities",
			slog.String("application", query.AppName),
			slog.String("user_key", query.UserKey),
			slog.String("event_name", query.EventName),
			slog.String("start_time", formatTime(start)),
			slog.String("end_time", formatTime(end)))

		var activities, events int
		err = run.Reports.ForEachActivity(ctx, query, func(activity *reports.Activity) error {
			if sheet.Full() {
				return gsuite.ErrStop
			}
			activities++

			// Every nested object here is a pointer that the API may omit, so each is
			// read defensively; a single sparse activity must not end the report.
			timestamp, application := "", query.AppName
			if activity.Id != nil {
				timestamp = formatAPITime(activity.Id.Time)
				if activity.Id.ApplicationName != "" {
					application = activity.Id.ApplicationName
				}
			}
			actorEmail, actorType := "", ""
			if activity.Actor != nil {
				actorEmail = activity.Actor.Email
				actorType = activity.Actor.CallerType
			}

			for _, event := range activity.Events {
				if event == nil {
					continue
				}
				events++

				// An event with no parameters still describes something that happened
				// and therefore still gets a row.
				if len(event.Parameters) == 0 {
					if err := sheet.AppendRow(
						timestamp, actorEmail, actorType, activity.IpAddress,
						application, event.Type, event.Name, "", "",
					); err != nil {
						return err
					}
					continue
				}

				for _, parameter := range event.Parameters {
					if parameter == nil {
						continue
					}
					if sheet.Full() {
						return gsuite.ErrStop
					}
					if err := sheet.AppendRow(
						timestamp, actorEmail, actorType, activity.IpAddress,
						application, event.Type, event.Name,
						parameter.Name, formatParameter(parameter),
					); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		run.Log.Info("audit activities collected",
			slog.String("application", query.AppName),
			slog.Int("activities", activities),
			slog.Int("events", events),
			slog.Int("rows", sheet.Rows()))
		return nil
	},
}

// firstNonEmpty returns the first non-empty value, which keeps the defaulting of
// optional section parameters compact at the call site.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
