package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// TaskID is the canonical identifier of a report task.
//
// The identifiers are stable API: they appear in INI files, in --only/--skip
// filters and in log output. The legacy Go function names used by earlier
// versions of this tool (GetUsersWithout2Fa and friends) are retained as aliases
// so that existing configuration files keep working unchanged.
type TaskID string

// The canonical task identifiers.
const (
	TaskUsersWithout2FA TaskID = "users-without-2fa"
	TaskSuspendedUsers  TaskID = "suspended-users"
	TaskAdmins          TaskID = "admins"
	// #nosec G101 -- a report identifier that happens to contain "password", not a credential
	TaskAppPasswords     TaskID = "app-specific-passwords"
	TaskGroups           TaskID = "groups"
	TaskGroupMembers     TaskID = "group-members"
	TaskThirdPartyTokens TaskID = "third-party-tokens"
	TaskAuditActivities  TaskID = "audit-activities"
)

// TaskDefinition describes a task for documentation and validation purposes. The
// executable behaviour lives in package tasks; keeping the vocabulary here lets
// configuration be validated before any credential is touched or any API call is
// made.
type TaskDefinition struct {
	ID      TaskID
	Summary string
	// Aliases are additional accepted spellings, including the legacy names.
	Aliases []string
	// Keys lists the INI keys this task understands, beyond the common ones
	// (task, enabled, sheet). Used to warn about typos.
	Keys []string
}

// taskCatalog is the single source of truth for which tasks exist.
var taskCatalog = []TaskDefinition{
	{
		ID:      TaskUsersWithout2FA,
		Summary: "Users that have not enrolled in 2-step verification.",
		Aliases: []string{"GetUsersWithout2Fa", "users-without-2sv"},
		Keys:    []string{"whitelist"},
	},
	{
		ID:      TaskSuspendedUsers,
		Summary: "Suspended user accounts and the reason for suspension.",
		Aliases: []string{"GetSuspendedUsers"},
	},
	{
		ID:      TaskAdmins,
		Summary: "Super administrators and delegated administrators, de-duplicated.",
		Aliases: []string{"GetAdmins"},
	},
	{
		ID:      TaskAppPasswords,
		Summary: "Application specific passwords (ASPs) issued per user.",
		Aliases: []string{"GetAsps", "asps"},
		Keys:    []string{"userkey"},
	},
	{
		ID:      TaskGroups,
		Summary: "Groups in the domain with membership counts.",
		Aliases: []string{"GetGroups"},
	},
	{
		ID:      TaskGroupMembers,
		Summary: "Direct members of every group, with role and status.",
		Aliases: []string{"GetGroupMembers"},
	},
	{
		ID:      TaskThirdPartyTokens,
		Summary: "OAuth tokens granted to third party applications, with scopes.",
		Aliases: []string{"GetTokensThirdPartyApps", "tokens"},
		Keys:    []string{"userkey"},
	},
	{
		ID:      TaskAuditActivities,
		Summary: "Admin SDK Reports audit activities for one application.",
		Aliases: []string{"GetAuditLogins", "audit", "audit-logins"},
		Keys:    []string{"userkey", "appname", "eventname", "starttime", "endtime", "lookbackdays"},
	},
}

// taskIndex maps every accepted spelling, in normalised form, onto its task.
var taskIndex = buildTaskIndex()

func buildTaskIndex() map[string]TaskDefinition {
	index := make(map[string]TaskDefinition, len(taskCatalog)*3)
	for _, def := range taskCatalog {
		index[normaliseTaskName(string(def.ID))] = def
		for _, alias := range def.Aliases {
			index[normaliseTaskName(alias)] = def
		}
	}
	return index
}

// normaliseTaskName reduces a task spelling to a comparison key by lowercasing
// and dropping separators. This makes "GetUsersWithout2Fa", "users-without-2fa"
// and "users_without_2fa" equivalent, which matters because the same identifier
// is typed into INI files by hand.
func normaliseTaskName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	// "get" prefixes come from the legacy function names and carry no meaning.
	return strings.TrimPrefix(b.String(), "get")
}

// ParseTaskID resolves any accepted spelling of a task name.
func ParseTaskID(raw string) (TaskID, error) {
	def, ok := taskIndex[normaliseTaskName(raw)]
	if !ok {
		return "", fmt.Errorf("unknown task %q (known tasks: %s)", raw, strings.Join(TaskNames(), ", "))
	}
	return def.ID, nil
}

// LookupTask returns the definition of a canonical task ID.
func LookupTask(id TaskID) (TaskDefinition, bool) {
	def, ok := taskIndex[normaliseTaskName(string(id))]
	return def, ok
}

// TaskCatalog returns every task definition, ordered by identifier.
func TaskCatalog() []TaskDefinition {
	out := append([]TaskDefinition(nil), taskCatalog...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// TaskNames returns every canonical task identifier, sorted.
func TaskNames() []string {
	names := make([]string, 0, len(taskCatalog))
	for _, def := range taskCatalog {
		names = append(names, string(def.ID))
	}
	sort.Strings(names)
	return names
}

// AuditApplications lists the Admin SDK Reports application names known at the
// time of writing.
//
// Google adds applications regularly, so an unrecognised value produces a
// warning rather than an error: refusing to run against a newly published
// application would be worse than reporting on it without validation.
var AuditApplications = []string{
	"access_transparency", "admin", "calendar", "chat", "chrome", "context_aware_access",
	"data_studio", "drive", "gcp", "gplus", "groups", "groups_enterprise", "jamboard",
	"keep", "login", "meet", "mobile", "rules", "saml", "token", "user_accounts", "vault",
}

// DefaultAuditApplication matches the historical behaviour of the tool, whose
// audit task was named "GetAuditLogins".
const DefaultAuditApplication = "login"

// AllUsers is the Reports API sentinel for "every user in the customer". It is
// also used by AegiSheet's per-user Directory tasks to mean "enumerate users".
const AllUsers = "all"

// TaskSpec is one fully resolved unit of work: an INI section plus its
// parameters, pointing at the sheet it will populate.
type TaskSpec struct {
	// Section is the INI section name the spec came from. It is also the default
	// worksheet name, preserving the original tool's behaviour.
	Section string
	// Sheet is the worksheet to write into; may be overridden with `sheet=`.
	Sheet string
	// ID is the canonical task to execute.
	ID TaskID
	// RawName is the task spelling as written in the configuration file, kept for
	// diagnostics.
	RawName string
	// Enabled allows a section to be kept in the file but skipped.
	Enabled bool

	// UserKey selects the subject for per-user tasks. Empty or "all" means every
	// user in the customer.
	UserKey string
	// AppName is the Reports API application for audit tasks.
	AppName string
	// EventName optionally narrows audit activities to a single event.
	EventName string
	// StartTime and EndTime bound audit activities. Zero means unbounded.
	StartTime time.Time
	EndTime   time.Time
	// LookbackDays derives StartTime relative to the run time when StartTime is
	// not set explicitly.
	LookbackDays int

	// MaxRows caps the rows written by this task. Zero means unlimited. This is a
	// safety valve for interactive runs against very large tenants.
	MaxRows int

	// Whitelist holds addresses to exclude from a task's output.
	Whitelist StringSet

	// Extra carries INI keys that the task vocabulary does not recognise. They
	// are reported as warnings rather than silently dropped.
	Extra map[string]string
}

// EffectiveUserKey reports the subject to query and whether the task should
// enumerate every user instead of a single one.
func (t TaskSpec) EffectiveUserKey() (userKey string, allUsers bool) {
	key := strings.TrimSpace(t.UserKey)
	if key == "" || strings.EqualFold(key, AllUsers) {
		return AllUsers, true
	}
	return key, false
}

// ResolveWindow computes the audit time window relative to now, applying
// LookbackDays when no explicit start time was configured.
func (t TaskSpec) ResolveWindow(now time.Time) (start, end time.Time) {
	start, end = t.StartTime, t.EndTime
	if start.IsZero() && t.LookbackDays > 0 {
		start = now.Add(-time.Duration(t.LookbackDays) * 24 * time.Hour)
	}
	return start, end
}

// StringSet is a case-insensitive set of identifiers, used for whitelists.
//
// It replaces the binary search over a caller-sorted slice used by earlier
// versions: that approach silently returned false whenever the caller forgot to
// sort, and it was case-sensitive, so "Alice@example.com" bypassed a whitelist
// entry spelled "alice@example.com".
type StringSet map[string]struct{}

// NewStringSet builds a set, trimming whitespace, lowercasing and dropping empty
// entries.
func NewStringSet(values []string) StringSet {
	set := make(StringSet, len(values))
	for _, v := range values {
		if key := strings.ToLower(strings.TrimSpace(v)); key != "" {
			set[key] = struct{}{}
		}
	}
	return set
}

// Contains reports whether value is in the set, ignoring case and surrounding
// whitespace.
func (s StringSet) Contains(value string) bool {
	if len(s) == 0 {
		return false
	}
	_, ok := s[strings.ToLower(strings.TrimSpace(value))]
	return ok
}

// Len returns the number of entries.
func (s StringSet) Len() int { return len(s) }

// Sorted returns the entries in a deterministic order, for logging.
func (s StringSet) Sorted() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
