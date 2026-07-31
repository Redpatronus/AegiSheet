package tasks

import (
	"context"
	"fmt"
	"log/slog"

	admin "google.golang.org/api/admin/directory/v1"

	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/gsuite"
	"github.com/Redpatronus/AegiSheet/internal/xlsx"
)

// groupsTask reports the groups in scope.
var groupsTask = Task{
	ID:      config.TaskGroups,
	Summary: "Groups in the domain with membership counts.",
	Execute: func(ctx context.Context, run *Run) error {
		sheet, err := run.Sheet([]xlsx.Column{
			xlsx.Col("GROUP NAME", 32),
			xlsx.Col("E-MAIL", 34),
			xlsx.Col("DESCRIPTION", 40),
			xlsx.Col("DIRECT MEMBERS", 15),
			xlsx.Col("ADMIN CREATED", 14),
			xlsx.Col("ALIASES", 40),
		})
		if err != nil {
			return err
		}

		return run.Directory.ForEachGroup(ctx, func(group *admin.Group) error {
			if sheet.Full() {
				return gsuite.ErrStop
			}
			return sheet.AppendRow(
				group.Name,
				group.Email,
				group.Description,
				group.DirectMembersCount,
				group.AdminCreated,
				joinValues(group.Aliases),
			)
		})
	},
}

// groupMemberRow is one row of the group membership report, resolved before being
// written so that the fan-out below can keep results in a stable order.
type groupMemberRow struct {
	groupName  string
	groupEmail string
	email      string
	memberID   string
	role       string
	memberType string
	status     string
}

// groupMembersTask reports the direct members of every group.
//
// Listing members is one API call per group, so the calls are issued with bounded
// concurrency; on a tenant with hundreds of groups this turns minutes of
// sequential round trips into seconds. Group order and member order are preserved
// so the output stays reproducible.
//
// Unlike earlier versions, members without an e-mail address are included rather
// than skipped. Such members are real — an entire customer added to a group, or a
// service identity — and omitting them understated the membership of a group in a
// report whose purpose is to show exactly who has access.
var groupMembersTask = Task{
	ID:      config.TaskGroupMembers,
	Summary: "Direct members of every group, with role and status.",
	Execute: func(ctx context.Context, run *Run) error {
		sheet, err := run.Sheet([]xlsx.Column{
			xlsx.Col("GROUP NAME", 32),
			xlsx.Col("GROUP E-MAIL", 34),
			xlsx.Col("MEMBER E-MAIL", 34),
			xlsx.Col("MEMBER ID", 24),
			xlsx.Col("ROLE", 10),
			xlsx.Col("TYPE", 10),
			xlsx.Col("STATUS", 12),
		})
		if err != nil {
			return err
		}

		// Collect the groups first: the member calls fan out over this list, and the
		// group objects are small enough to hold even for a large tenant.
		var groups []*admin.Group
		if err := run.Directory.ForEachGroup(ctx, func(group *admin.Group) error {
			groups = append(groups, group)
			return nil
		}); err != nil {
			return err
		}
		run.Log.Info("listing group members", slog.Int("groups", len(groups)))

		perGroup, fanErr := fanOut(ctx, groups, run.concurrency(), run.Log,
			func(ctx context.Context, group *admin.Group) ([]groupMemberRow, error) {
				var rows []groupMemberRow
				err := run.Directory.ForEachMember(ctx, group.Id, func(member *admin.Member) error {
					rows = append(rows, groupMemberRow{
						groupName:  group.Name,
						groupEmail: group.Email,
						email:      member.Email,
						memberID:   member.Id,
						role:       member.Role,
						memberType: member.Type,
						status:     member.Status,
					})
					return nil
				})
				if err != nil {
					// Name the group in the error: with concurrency in play the log order
					// alone does not identify which call failed.
					return rows, fmt.Errorf("group %q: %w", group.Email, err)
				}
				return rows, nil
			})

		// Write whatever was collected even if some groups failed, then report the
		// failure. A partial membership report with a loud error is more useful than
		// nothing.
		var members int
		for _, rows := range perGroup {
			for _, row := range rows {
				if sheet.Full() {
					break
				}
				if err := sheet.AppendRow(
					row.groupName,
					row.groupEmail,
					row.email,
					row.memberID,
					row.role,
					row.memberType,
					row.status,
				); err != nil {
					return err
				}
				members++
			}
		}
		run.Log.Info("group members collected",
			slog.Int("groups", len(groups)), slog.Int("members", members))

		return fanErr
	},
}
