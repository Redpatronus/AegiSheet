package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfigFile writes an INI file into a temporary directory and returns its
// path. The [global] section it prepends is the minimum needed to pass validation,
// so each test only has to express the part it is about.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ini")
	content := "[global]\n" +
		"credentialsFile=credentials.json\n" +
		"tokenFile=token.json\n" +
		"output=report.xlsx\n" +
		body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// noEnv is a getenv that reports every variable as unset.
func noEnv(string) string { return "" }

// envFrom builds a getenv over a fixed map.
func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadPrecedence(t *testing.T) {
	// The output path is set in all three layers; each case removes the layers
	// above it to prove the ordering.
	path := writeConfigFile(t, "\n[Groups]\ntask=GetGroups\n")

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "ini only",
			args: []string{"-config", path},
			want: "report.xlsx",
		},
		{
			name: "environment overrides ini",
			args: []string{"-config", path},
			env:  map[string]string{"AEGISHEET_OUTPUT": "from-env.xlsx"},
			want: "from-env.xlsx",
		},
		{
			name: "flag overrides environment",
			args: []string{"-config", path, "-output", "from-flag.xlsx"},
			env:  map[string]string{"AEGISHEET_OUTPUT": "from-env.xlsx"},
			want: "from-flag.xlsx",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Load(test.args, envFrom(test.env), io.Discard)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Output.Path != test.want {
				t.Errorf("output path = %q, want %q", cfg.Output.Path, test.want)
			}
		})
	}
}

func TestLoadDefaultsSurviveEmptyLayers(t *testing.T) {
	path := writeConfigFile(t, "\n[Groups]\ntask=GetGroups\n")

	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	defaults := Default()
	if cfg.API.Customer != defaults.API.Customer {
		t.Errorf("customer = %q, want the default %q", cfg.API.Customer, defaults.API.Customer)
	}
	if cfg.API.PageSize != defaults.API.PageSize {
		t.Errorf("page size = %d, want the default %d", cfg.API.PageSize, defaults.API.PageSize)
	}
	if cfg.Output.FileMode != DefaultOutputMode {
		t.Errorf("output mode = %v, want the default %v", cfg.Output.FileMode, DefaultOutputMode)
	}
	// An unset boolean flag must not overwrite the default.
	if cfg.Log.IncludePII {
		t.Error("PII logging is enabled by default, which would leak personal data")
	}
}

func TestLoadAcceptsLegacyTaskNames(t *testing.T) {
	// Configuration files written for earlier versions must keep working.
	path := writeConfigFile(t, `
[2nd Factor Users]
task=GetUsersWithout2Fa

[Group Members]
task=GetGroupMembers

[Admin Activity]
task=GetAuditLogins
appname="drive"

[Tokens]
task=GetTokensThirdPartyApps
`)

	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := map[string]TaskID{
		"2nd Factor Users": TaskUsersWithout2FA,
		"Group Members":    TaskGroupMembers,
		"Admin Activity":   TaskAuditActivities,
		"Tokens":           TaskThirdPartyTokens,
	}
	if len(cfg.Tasks) != len(want) {
		t.Fatalf("got %d sections, want %d", len(cfg.Tasks), len(want))
	}
	for _, task := range cfg.Tasks {
		if want[task.Section] != task.ID {
			t.Errorf("section %q resolved to %q, want %q", task.Section, task.ID, want[task.Section])
		}
		if task.Sheet != task.Section {
			t.Errorf("section %q writes to sheet %q, want the section name", task.Section, task.Sheet)
		}
	}
}

func TestParseTaskIDSpellings(t *testing.T) {
	// The same task must resolve from every spelling an operator might type.
	for _, spelling := range []string{
		"users-without-2fa",
		"users_without_2fa",
		"GetUsersWithout2Fa",
		"getuserswithout2fa",
		"USERS-WITHOUT-2FA",
	} {
		got, err := ParseTaskID(spelling)
		if err != nil {
			t.Errorf("ParseTaskID(%q): %v", spelling, err)
			continue
		}
		if got != TaskUsersWithout2FA {
			t.Errorf("ParseTaskID(%q) = %q, want %q", spelling, got, TaskUsersWithout2FA)
		}
	}

	if _, err := ParseTaskID("GetEverything"); err == nil {
		t.Error("an unknown task name was accepted")
	}
}

func TestLoadRejectsUnknownTask(t *testing.T) {
	// Earlier versions silently fell back to the 2FA report here, producing a
	// plausible sheet with the wrong contents.
	path := writeConfigFile(t, "\n[Mystery]\ntask=GetSomethingElse\n")

	_, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err == nil {
		t.Fatal("an unknown task name was accepted")
	}
	if !strings.Contains(err.Error(), "Mystery") {
		t.Errorf("the error does not name the offending section: %v", err)
	}
}

func TestLoadSkipsSectionWithoutTaskKey(t *testing.T) {
	path := writeConfigFile(t, `
[Groups]
task=GetGroups

[Leftover Notes]
comment=this section configures nothing
`)

	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tasks) != 1 {
		t.Fatalf("got %d sections, want only the one with a task key", len(cfg.Tasks))
	}

	var warned bool
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "Leftover Notes") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("skipping a section without a task key produced no warning: %v", cfg.Warnings)
	}
}

func TestLoadTreatsExamplePlaceholdersAsUnset(t *testing.T) {
	// The shipped example uses <path_to_credentials> placeholders. Running it
	// unedited must produce a configuration error, not a file-not-found error from
	// deep inside the auth code.
	path := filepath.Join(t.TempDir(), "example.ini")
	content := "[global]\n" +
		"credentialsFile=<path_to_credentials>\n" +
		"tokenFile=<path_to_token>\n" +
		"output=<path_to_output>\n" +
		"\n[Groups]\ntask=GetGroups\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err == nil {
		t.Fatal("the unedited example configuration was accepted")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("the error does not point at the missing credentials: %v", err)
	}
}

func TestLoadMissingConfigFile(t *testing.T) {
	_, err := Load([]string{"-config", filepath.Join(t.TempDir(), "absent.ini")}, noEnv, io.Discard)
	if err == nil {
		t.Fatal("a missing configuration file was accepted")
	}
	if !strings.Contains(err.Error(), "-config") {
		t.Errorf("the error does not mention how to fix it: %v", err)
	}
}

func TestWhitelistResolution(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "section level wins",
			body: "\n[global]\nwhitelist=global@example.com\n" +
				"\n[2FA]\ntask=GetUsersWithout2Fa\nwhitelist=section@example.com\n",
			want: "section@example.com",
		},
		{
			name: "global fallback",
			body: "whitelist=global@example.com\n\n[2FA]\ntask=GetUsersWithout2Fa\n",
			want: "global@example.com",
		},
		{
			name: "legacy users section",
			body: "\n[users]\nwhitelist=legacy@example.com\n\n[2FA]\ntask=GetUsersWithout2Fa\n",
			want: "legacy@example.com",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfigFile(t, test.body)
			cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Tasks) != 1 {
				t.Fatalf("got %d sections, want 1: %v", len(cfg.Tasks), cfg.SectionNames())
			}
			if !cfg.Tasks[0].Whitelist.Contains(test.want) {
				t.Errorf("whitelist %v does not contain %q", cfg.Tasks[0].Whitelist.Sorted(), test.want)
			}
		})
	}
}

func TestWhitelistIgnoresCase(t *testing.T) {
	// The binary search this replaces was case-sensitive, so a whitelist entry
	// spelled in lower case did not match an address the API returned capitalised.
	set := NewStringSet([]string{" Alice@Example.com ", "", "bob@example.com"})
	if set.Len() != 2 {
		t.Fatalf("set has %d entries, want 2 (blank entries must be dropped)", set.Len())
	}
	for _, probe := range []string{"alice@example.com", "ALICE@EXAMPLE.COM", " alice@example.com "} {
		if !set.Contains(probe) {
			t.Errorf("Contains(%q) = false, want true", probe)
		}
	}
	if set.Contains("carol@example.com") {
		t.Error("Contains reported an absent address as present")
	}
}

func TestOnlyAndSkipFilters(t *testing.T) {
	body := `
[Groups]
task=GetGroups

[Admins]
task=GetAdmins

[2FA]
task=GetUsersWithout2Fa
`
	path := writeConfigFile(t, body)

	t.Run("only selects by section name", func(t *testing.T) {
		cfg, err := Load([]string{"-config", path, "-only", "Groups,Admins"}, noEnv, io.Discard)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.SectionNames(); len(got) != 2 {
			t.Errorf("sections = %v, want Groups and Admins", got)
		}
	})

	t.Run("only selects by task name", func(t *testing.T) {
		cfg, err := Load([]string{"-config", path, "-only", string(TaskAdmins)}, noEnv, io.Discard)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.SectionNames(); len(got) != 1 || got[0] != "Admins" {
			t.Errorf("sections = %v, want only Admins", got)
		}
	})

	t.Run("skip removes a section", func(t *testing.T) {
		cfg, err := Load([]string{"-config", path, "-skip", "Groups"}, noEnv, io.Discard)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		for _, name := range cfg.SectionNames() {
			if name == "Groups" {
				t.Error("the skipped section was executed anyway")
			}
		}
	})

	t.Run("a filter matching nothing warns", func(t *testing.T) {
		cfg, err := Load([]string{"-config", path, "-skip", "Typo"}, noEnv, io.Discard)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		var warned bool
		for _, warning := range cfg.Warnings {
			if strings.Contains(warning, "Typo") {
				warned = true
			}
		}
		if !warned {
			t.Errorf("a filter that matched nothing produced no warning: %v", cfg.Warnings)
		}
	})

	t.Run("filtering everything out is an error", func(t *testing.T) {
		if _, err := Load([]string{"-config", path, "-only", "Nothing"}, noEnv, io.Discard); err == nil {
			t.Error("a run with no sections was accepted")
		}
	})
}

func TestSectionEnabledFalseIsSkipped(t *testing.T) {
	path := writeConfigFile(t, `
[Groups]
task=GetGroups

[Admins]
task=GetAdmins
enabled=false
`)
	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.SectionNames(); len(got) != 1 || got[0] != "Groups" {
		t.Errorf("sections = %v, want only Groups", got)
	}
}

func TestAuditSectionDefaults(t *testing.T) {
	path := writeConfigFile(t, "\n[Audit]\ntask=GetAuditLogins\n")
	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	spec := cfg.Tasks[0]

	if spec.AppName != DefaultAuditApplication {
		t.Errorf("appName = %q, want the default %q", spec.AppName, DefaultAuditApplication)
	}
	// The previous implementation passed an empty userKey, which the API rejects.
	userKey, allUsers := spec.EffectiveUserKey()
	if userKey != AllUsers || !allUsers {
		t.Errorf("EffectiveUserKey() = (%q, %v), want (%q, true)", userKey, allUsers, AllUsers)
	}
}

func TestAuditKeysAreCaseInsensitive(t *testing.T) {
	// The original code read "userkey" in one file and "userKey" in another; both
	// spellings must resolve to the same value.
	path := writeConfigFile(t, "\n[Audit]\ntask=audit-activities\nUserKey=admin@example.com\nAPPNAME=admin\n")
	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	spec := cfg.Tasks[0]
	if spec.UserKey != "admin@example.com" {
		t.Errorf("userKey = %q, want admin@example.com", spec.UserKey)
	}
	if spec.AppName != "admin" {
		t.Errorf("appName = %q, want admin", spec.AppName)
	}
}

func TestAuditWindowResolution(t *testing.T) {
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	t.Run("lookback derives the start", func(t *testing.T) {
		spec := TaskSpec{LookbackDays: 7}
		start, end := spec.ResolveWindow(now)
		if want := now.AddDate(0, 0, -7); !start.Equal(want) {
			t.Errorf("start = %v, want %v", start, want)
		}
		if !end.IsZero() {
			t.Errorf("end = %v, want the zero time (open bound)", end)
		}
	})

	t.Run("an explicit start wins over lookback", func(t *testing.T) {
		explicit := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		spec := TaskSpec{StartTime: explicit, LookbackDays: 7}
		start, _ := spec.ResolveWindow(now)
		if !start.Equal(explicit) {
			t.Errorf("start = %v, want the configured %v", start, explicit)
		}
	})
}

func TestTimestampParsing(t *testing.T) {
	path := writeConfigFile(t, "\n[Audit]\ntask=audit-activities\nstartTime=2026-01-31\nendTime=2026-02-01T12:30:00Z\n")
	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	spec := cfg.Tasks[0]

	if want := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC); !spec.StartTime.Equal(want) {
		t.Errorf("startTime = %v, want %v", spec.StartTime, want)
	}
	if want := time.Date(2026, 2, 1, 12, 30, 0, 0, time.UTC); !spec.EndTime.Equal(want) {
		t.Errorf("endTime = %v, want %v", spec.EndTime, want)
	}
}

func TestTimestampParsingRejectsGarbage(t *testing.T) {
	path := writeConfigFile(t, "\n[Audit]\ntask=audit-activities\nstartTime=last tuesday\n")
	if _, err := Load([]string{"-config", path}, noEnv, io.Discard); err == nil {
		t.Error("an unparseable timestamp was accepted")
	}
}

func TestServiceAccountRequiresImpersonation(t *testing.T) {
	path := writeConfigFile(t, "\n[Groups]\ntask=GetGroups\n")

	_, err := Load([]string{"-config", path, "-service-account", "key.json"}, noEnv, io.Discard)
	if err == nil {
		t.Fatal("a service account without an impersonation subject was accepted")
	}
	if !strings.Contains(err.Error(), "-impersonate") {
		t.Errorf("the error does not name the missing flag: %v", err)
	}

	if _, err := Load([]string{
		"-config", path,
		"-service-account", "key.json",
		"-impersonate", "admin@example.com",
	}, noEnv, io.Discard); err != nil {
		t.Errorf("a complete service account configuration was rejected: %v", err)
	}
}

func TestAuthModeSelection(t *testing.T) {
	tests := []struct {
		name string
		auth AuthConfig
		want AuthMode
	}{
		{
			name: "a service account key wins over a client secret",
			auth: AuthConfig{ServiceAccountFile: "key.json", CredentialsFile: "credentials.json"},
			want: AuthModeServiceAccount,
		},
		{
			name: "application default credentials",
			auth: AuthConfig{UseADC: true},
			want: AuthModeADC,
		},
		{
			name: "installed application",
			auth: AuthConfig{CredentialsFile: "credentials.json"},
			want: AuthModeInstalledApp,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.auth.Mode()
			if err != nil {
				t.Fatalf("Mode: %v", err)
			}
			if got != test.want {
				t.Errorf("mode = %q, want %q", got, test.want)
			}
		})
	}

	if _, err := (AuthConfig{}).Mode(); err == nil {
		t.Error("a configuration with no credentials reported a usable mode")
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	// An operator fixing a configuration file should not have to iterate one error
	// per run.
	cfg := Default()
	cfg.API.HTTPTimeout = 0
	cfg.API.PageSize = 0
	cfg.API.Concurrency = 0
	cfg.Log.Level = "chatty"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("an invalid configuration passed validation")
	}
	for _, want := range []string{"-http-timeout", "-page-size", "-concurrency", "chatty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestInvalidFlagValuesAreRejected(t *testing.T) {
	path := writeConfigFile(t, "\n[Groups]\ntask=GetGroups\n")

	for _, args := range [][]string{
		{"-config", path, "-http-timeout", "soon"},
		{"-config", path, "-output-mode", "999"},
		{"-config", path, "-page-size", "many"},
	} {
		if _, err := Load(args, noEnv, io.Discard); err == nil {
			t.Errorf("Load(%v) accepted an invalid value", args)
		}
	}
}

func TestDurationAcceptsBareSeconds(t *testing.T) {
	// Operators writing INI files do not necessarily know Go's duration syntax.
	path := writeConfigFile(t, "httpTimeout=90\n\n[Groups]\ntask=GetGroups\n")
	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.HTTPTimeout != 90*time.Second {
		t.Errorf("httpTimeout = %v, want 90s", cfg.API.HTTPTimeout)
	}
}

func TestInformationalModesReturnSentinels(t *testing.T) {
	path := writeConfigFile(t, "\n[Groups]\ntask=GetGroups\n")

	tests := []struct {
		flag string
		want error
	}{
		{flag: "-version", want: ErrVersionRequested},
		{flag: "-list-tasks", want: ErrTasksRequested},
		{flag: "-print-config", want: ErrConfigRequested},
	}
	for _, test := range tests {
		buf := &strings.Builder{}
		_, err := Load([]string{"-config", path, test.flag}, noEnv, buf)
		if !errors.Is(err, test.want) {
			t.Errorf("Load(%s) error = %v, want %v", test.flag, err, test.want)
		}
		if buf.Len() == 0 {
			t.Errorf("Load(%s) printed nothing", test.flag)
		}
	}
}

func TestUnknownGlobalKeyWarns(t *testing.T) {
	path := writeConfigFile(t, "credentialsFilee=typo.json\n\n[Groups]\ntask=GetGroups\n")
	cfg, err := Load([]string{"-config", path}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Keys are parsed case-insensitively, so the warning echoes the key in lower
	// case; the comparison here does the same.
	var warned bool
	for _, warning := range cfg.Warnings {
		if strings.Contains(strings.ToLower(warning), "credentialsfilee") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a misspelled global key produced no warning: %v", cfg.Warnings)
	}
}

func TestPositionalArgumentsRejected(t *testing.T) {
	path := writeConfigFile(t, "\n[Groups]\ntask=GetGroups\n")
	if _, err := Load([]string{"-config", path, "unexpected"}, noEnv, io.Discard); err == nil {
		t.Error("a positional argument was silently ignored")
	}
}
