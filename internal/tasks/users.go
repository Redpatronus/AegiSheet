package tasks

import (
	"context"
	"log/slog"
	"sort"

	admin "google.golang.org/api/admin/directory/v1"

	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/gsuite"
	"github.com/Redpatronus/AegiSheet/internal/xlsx"
)

// Directory API search expressions. Kept as named constants because a typo in one
// of them produces a plausible but wrong report rather than an error.
const (
	queryNotEnrolledIn2SV = "isEnrolledIn2Sv=false"
	querySuspended        = "isSuspended=true"
	querySuperAdmin       = "isAdmin=true"
	queryDelegatedAdmin   = "isDelegatedAdmin=true"
)

// usersWithout2FATask reports accounts that can still authenticate with a password
// alone, which is normally the highest-value finding in this workbook.
var usersWithout2FATask = Task{
	ID:      config.TaskUsersWithout2FA,
	Summary: "Users that have not enrolled in 2-step verification.",
	Execute: func(ctx context.Context, run *Run) error {
		sheet, err := run.Sheet([]xlsx.Column{
			xlsx.Col("E-MAIL", 34),
			xlsx.Col("FULL NAME", 26),
			xlsx.Col("ORG UNIT", 24),
			xlsx.Col("2SV ENROLLED", 14),
			xlsx.Col("2SV ENFORCED", 14),
			xlsx.Col("SUSPENDED", 11),
			xlsx.Col("ARCHIVED", 10),
			xlsx.Col("LAST LOGIN (UTC)", 20),
			xlsx.Col("CREATED (UTC)", 20),
		})
		if err != nil {
			return err
		}

		var excluded int
		err = run.Directory.ForEachUser(ctx, queryNotEnrolledIn2SV, func(user *admin.User) error {
			if sheet.Full() {
				return gsuite.ErrStop
			}
			// The whitelist exists for accounts that legitimately cannot enrol, such
			// as service identities. Matching is case-insensitive, unlike the sorted
			// binary search this replaces.
			if run.Spec.Whitelist.Contains(user.PrimaryEmail) {
				excluded++
				run.Log.Debug("user excluded by whitelist", slog.String("email", user.PrimaryEmail))
				return nil
			}
			return sheet.AppendRow(
				user.PrimaryEmail,
				gsuite.FullName(user),
				user.OrgUnitPath,
				user.IsEnrolledIn2Sv,
				user.IsEnforcedIn2Sv,
				user.Suspended,
				user.Archived,
				formatAPITime(user.LastLoginTime),
				formatAPITime(user.CreationTime),
			)
		})
		if err != nil {
			return err
		}

		if excluded > 0 {
			run.Log.Info("users omitted by the whitelist",
				slog.Int("omitted", excluded),
				slog.Int("whitelist_entries", run.Spec.Whitelist.Len()))
		}
		return nil
	},
}

// suspendedUsersTask reports accounts that are suspended, and why.
var suspendedUsersTask = Task{
	ID:      config.TaskSuspendedUsers,
	Summary: "Suspended user accounts and the reason for suspension.",
	Execute: func(ctx context.Context, run *Run) error {
		sheet, err := run.Sheet([]xlsx.Column{
			xlsx.Col("E-MAIL", 34),
			xlsx.Col("FULL NAME", 26),
			xlsx.Col("ORG UNIT", 24),
			xlsx.Col("SUSPENDED", 11),
			xlsx.Col("SUSPENSION REASON", 22),
			xlsx.Col("ARCHIVED", 10),
			xlsx.Col("LAST LOGIN (UTC)", 20),
			xlsx.Col("CREATED (UTC)", 20),
		})
		if err != nil {
			return err
		}

		return run.Directory.ForEachUser(ctx, querySuspended, func(user *admin.User) error {
			if sheet.Full() {
				return gsuite.ErrStop
			}
			if run.Spec.Whitelist.Contains(user.PrimaryEmail) {
				return nil
			}
			return sheet.AppendRow(
				user.PrimaryEmail,
				gsuite.FullName(user),
				user.OrgUnitPath,
				user.Suspended,
				user.SuspensionReason,
				user.Archived,
				formatAPITime(user.LastLoginTime),
				formatAPITime(user.CreationTime),
			)
		})
	},
}

// adminRecord accumulates the privileges of one administrator across both queries.
type adminRecord struct {
	user      *admin.User
	super     bool
	delegated bool
}

// roles renders the privilege combination for the report.
func (a adminRecord) roles() string {
	switch {
	case a.super && a.delegated:
		return "super admin, delegated admin"
	case a.super:
		return "super admin"
	case a.delegated:
		return "delegated admin"
	default:
		return ""
	}
}

// adminsTask reports privileged accounts.
//
// The Directory API has no single "is an administrator" predicate, so two queries
// are needed. Earlier versions appended both result sets to the same sheet, which
// listed every super administrator twice, since a super administrator also matches
// isDelegatedAdmin. Here the results are merged by address and the privileges are
// expressed in a ROLES column, so each person appears exactly once.
var adminsTask = Task{
	ID:      config.TaskAdmins,
	Summary: "Super administrators and delegated administrators, de-duplicated.",
	Execute: func(ctx context.Context, run *Run) error {
		sheet, err := run.Sheet([]xlsx.Column{
			xlsx.Col("E-MAIL", 34),
			xlsx.Col("FULL NAME", 26),
			xlsx.Col("ORG UNIT", 24),
			xlsx.Col("ROLES", 28),
			xlsx.Col("SUPER ADMIN", 13),
			xlsx.Col("DELEGATED ADMIN", 16),
			xlsx.Col("2SV ENROLLED", 14),
			xlsx.Col("SUSPENDED", 11),
			xlsx.Col("LAST LOGIN (UTC)", 20),
		})
		if err != nil {
			return err
		}

		records := make(map[string]*adminRecord)

		// collect merges one query's results into the record set.
		collect := func(query string, mark func(*adminRecord)) error {
			return run.Directory.ForEachUser(ctx, query, func(user *admin.User) error {
				if run.Spec.Whitelist.Contains(user.PrimaryEmail) {
					return nil
				}
				record, seen := records[user.PrimaryEmail]
				if !seen {
					record = &adminRecord{user: user}
					records[user.PrimaryEmail] = record
				}
				mark(record)
				return nil
			})
		}

		if err := collect(querySuperAdmin, func(r *adminRecord) { r.super = true }); err != nil {
			return err
		}
		if err := collect(queryDelegatedAdmin, func(r *adminRecord) { r.delegated = true }); err != nil {
			return err
		}

		// Sort by address: map iteration order is deliberately random in Go, and an
		// audit report that reorders itself between runs cannot be diffed.
		addresses := make([]string, 0, len(records))
		for address := range records {
			addresses = append(addresses, address)
		}
		sort.Strings(addresses)

		for _, address := range addresses {
			if sheet.Full() {
				break
			}
			record := records[address]
			if err := sheet.AppendRow(
				record.user.PrimaryEmail,
				gsuite.FullName(record.user),
				record.user.OrgUnitPath,
				record.roles(),
				record.super,
				record.delegated,
				record.user.IsEnrolledIn2Sv,
				record.user.Suspended,
				formatAPITime(record.user.LastLoginTime),
			); err != nil {
				return err
			}
		}

		run.Log.Info("administrators collected", slog.Int("administrators", len(records)))
		return nil
	},
}
