package tasks

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/gsuite"
	"github.com/Redpatronus/AegiSheet/internal/xlsx"
)

// resolveSubjects determines which users a per-user task should query.
//
// A section may name a single userKey, or use "all" (the default) to sweep the
// whole directory. Enumerating first and then fanning out — rather than nesting
// the per-user call inside the user pagination — keeps the concurrency bounded and
// the output order stable.
func resolveSubjects(ctx context.Context, run *Run) ([]gsuite.UserRef, error) {
	userKey, allUsers := run.Spec.EffectiveUserKey()

	if !allUsers {
		// Resolve the single subject so the report can show a display name, and so a
		// mistyped address fails immediately with a clear message instead of
		// producing an empty sheet.
		user, err := run.Directory.GetUser(ctx, userKey)
		if err != nil {
			return nil, fmt.Errorf("resolve configured userKey: %w", err)
		}
		return []gsuite.UserRef{{
			Email:     user.PrimaryEmail,
			FullName:  gsuite.FullName(user),
			Suspended: user.Suspended,
			Archived:  user.Archived,
		}}, nil
	}

	refs, err := run.Directory.ListUserRefs(ctx, "")
	if err != nil {
		return nil, err
	}
	run.Log.Info("resolved report subjects", slog.Int("users", len(refs)))
	return refs, nil
}

// aspRow is one application specific password.
type aspRow struct {
	userEmail    string
	userName     string
	name         string
	codeID       int64
	creationTime int64
	lastTimeUsed int64
}

// appPasswordsTask reports application specific passwords.
//
// These are long-lived credentials that bypass 2-step verification, so an
// inventory of them belongs in a security review. Earlier versions computed this
// data and only wrote it to the log, producing an empty worksheet; here it lands
// in the workbook like every other report.
var appPasswordsTask = Task{
	ID:      config.TaskAppPasswords,
	Summary: "Application specific passwords (ASPs) issued per user.",
	Execute: func(ctx context.Context, run *Run) error {
		sheet, err := run.Sheet([]xlsx.Column{
			xlsx.Col("USER E-MAIL", 34),
			xlsx.Col("FULL NAME", 26),
			xlsx.Col("APPLICATION", 30),
			xlsx.Col("CODE ID", 12),
			xlsx.Col("CREATED (UTC)", 20),
			xlsx.Col("LAST USED (UTC)", 20),
		})
		if err != nil {
			return err
		}

		subjects, err := resolveSubjects(ctx, run)
		if err != nil {
			return err
		}

		perUser, fanErr := fanOut(ctx, subjects, run.concurrency(), run.Log,
			func(ctx context.Context, subject gsuite.UserRef) ([]aspRow, error) {
				passwords, err := run.Directory.ListASPs(ctx, subject.Email)
				if err != nil {
					return nil, fmt.Errorf("user %s: %w", subject.Email, err)
				}
				rows := make([]aspRow, 0, len(passwords))
				for _, password := range passwords {
					rows = append(rows, aspRow{
						userEmail:    subject.Email,
						userName:     subject.FullName,
						name:         password.Name,
						codeID:       password.CodeId,
						creationTime: password.CreationTime,
						lastTimeUsed: password.LastTimeUsed,
					})
				}
				return rows, nil
			})

		written := 0
		for _, rows := range perUser {
			for _, row := range rows {
				if sheet.Full() {
					break
				}
				if err := sheet.AppendRow(
					row.userEmail,
					row.userName,
					row.name,
					row.codeID,
					// These two fields are Unix milliseconds, not RFC 3339 strings.
					formatEpochMillis(row.creationTime),
					formatEpochMillis(row.lastTimeUsed),
				); err != nil {
					return err
				}
				written++
			}
		}
		run.Log.Info("application specific passwords collected",
			slog.Int("users", len(subjects)), slog.Int("passwords", written))

		return fanErr
	},
}

// tokenRow is one third-party OAuth grant.
type tokenRow struct {
	userEmail   string
	userName    string
	displayText string
	clientID    string
	anonymous   bool
	nativeApp   bool
	scopes      []string
}

// thirdPartyTokensTask reports OAuth grants to third-party applications.
//
// Each grant is a standing delegation of the user's data to an external party, so
// the scope list is the substance of the report and is written out in full.
//
// Earlier versions required a userKey and reported on that one account, resolving
// each token's owner with an extra API call. Here the default is to sweep every
// user, and the owner is already known from the enumeration, so no lookup is
// needed.
var thirdPartyTokensTask = Task{
	ID:      config.TaskThirdPartyTokens,
	Summary: "OAuth tokens granted to third party applications, with scopes.",
	Execute: func(ctx context.Context, run *Run) error {
		sheet, err := run.Sheet([]xlsx.Column{
			xlsx.Col("USER E-MAIL", 34),
			xlsx.Col("FULL NAME", 26),
			xlsx.Col("APPLICATION", 32),
			xlsx.Col("CLIENT ID", 44),
			xlsx.Col("ANONYMOUS", 11),
			xlsx.Col("NATIVE APP", 11),
			xlsx.Col("SCOPES", 14),
			xlsx.Col("SCOPE LIST", 80),
		})
		if err != nil {
			return err
		}

		subjects, err := resolveSubjects(ctx, run)
		if err != nil {
			return err
		}

		perUser, fanErr := fanOut(ctx, subjects, run.concurrency(), run.Log,
			func(ctx context.Context, subject gsuite.UserRef) ([]tokenRow, error) {
				tokens, err := run.Directory.ListTokens(ctx, subject.Email)
				if err != nil {
					return nil, fmt.Errorf("user %s: %w", subject.Email, err)
				}
				rows := make([]tokenRow, 0, len(tokens))
				for _, token := range tokens {
					rows = append(rows, tokenRow{
						userEmail:   subject.Email,
						userName:    subject.FullName,
						displayText: token.DisplayText,
						clientID:    token.ClientId,
						anonymous:   token.Anonymous,
						nativeApp:   token.NativeApp,
						scopes:      token.Scopes,
					})
				}
				return rows, nil
			})

		written := 0
		for _, rows := range perUser {
			for _, row := range rows {
				if sheet.Full() {
					break
				}
				if err := sheet.AppendRow(
					row.userEmail,
					row.userName,
					row.displayText,
					row.clientID,
					row.anonymous,
					row.nativeApp,
					len(row.scopes),
					joinValues(row.scopes),
				); err != nil {
					return err
				}
				written++
			}
		}
		run.Log.Info("third party tokens collected",
			slog.Int("users", len(subjects)), slog.Int("tokens", written))

		return fanErr
	},
}
