// Package config resolves the complete runtime configuration of AegiSheet from
// four layers, in increasing order of precedence:
//
//  1. built-in defaults (Default)
//  2. the [global] section of the INI file
//  3. environment variables prefixed with AEGISHEET_
//  4. command line flags
//
// Layering matters operationally: the INI file is version controlled and
// describes *what* to report on, environment variables carry per-deployment
// values such as credential paths in a container, and flags let an operator
// override anything for a single run without editing either.
//
// The report sections themselves (which sheets to produce, and with what
// parameters) come only from the INI file, since they are the description of the
// work rather than of the environment. Flags -only and -skip select subsets of
// them.
//
// Everything an operator can set is declared once in settings(); see that table
// for the full surface.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Redpatronus/AegiSheet/internal/buildinfo"
	"github.com/Redpatronus/AegiSheet/internal/logging"
)

// Built-in defaults that are also documented in README.md and conf/.
const (
	// DefaultConfigPath points at the example configuration shipped in the repo.
	DefaultConfigPath = "conf/xls_report.ini"
	// DefaultTokenFile is where the installed-application OAuth token is cached.
	DefaultTokenFile = "token.json"
	// DefaultCustomer is the Admin SDK alias for "the caller's own tenant".
	DefaultCustomer = "my_customer"
	// DefaultOutputMode keeps the generated workbook readable only by its owner:
	// it contains directory data about real people.
	DefaultOutputMode fs.FileMode = 0o600
	// GlobalSection is the INI section holding process-wide settings.
	GlobalSection = "global"
	// legacyUsersSection was used by earlier versions to hold the 2FA whitelist.
	// It is still honoured as a fallback, and never treated as a report section.
	legacyUsersSection = "users"
)

// Sentinel errors returned by Load when the operator asked for information
// rather than for a run. Load has already written the requested output; the
// caller only has to exit successfully.
var (
	ErrVersionRequested = errors.New("config: version requested")
	ErrTasksRequested   = errors.New("config: task list requested")
	ErrConfigRequested  = errors.New("config: effective configuration requested")
)

// Config is the fully resolved configuration of a single run.
type Config struct {
	// ConfigPath is the INI file this configuration was read from.
	ConfigPath string

	Auth   AuthConfig
	API    APIConfig
	Output OutputConfig
	Log    logging.Options
	Run    RunConfig

	// Tasks are the report sections to execute, in file order, after -only/-skip
	// filtering.
	Tasks []TaskSpec

	// Warnings collects non-fatal problems found while loading. They are returned
	// rather than printed because the logger does not exist yet at load time; the
	// caller emits them once logging is configured.
	Warnings []string
}

// AuthConfig selects and parameterises the credential source.
type AuthConfig struct {
	// CredentialsFile is an OAuth client secret ("installed application") JSON.
	CredentialsFile string
	// TokenFile caches the token obtained with CredentialsFile.
	TokenFile string
	// ServiceAccountFile is a service account key JSON. Preferred for unattended
	// runs, and requires domain-wide delegation plus ImpersonateSubject.
	ServiceAccountFile string
	// ImpersonateSubject is the administrator the service account acts as.
	ImpersonateSubject string
	// UseADC selects Application Default Credentials.
	UseADC bool
	// NonInteractive forbids prompting on the terminal, so that a scheduled run
	// fails loudly instead of hanging forever on a stdin read.
	NonInteractive bool
	// Scopes overrides the built-in least-privilege scope set when non-empty.
	Scopes []string
}

// AuthMode names a credential source.
type AuthMode string

const (
	AuthModeServiceAccount AuthMode = "service_account"
	AuthModeInstalledApp   AuthMode = "installed_app"
	AuthModeADC            AuthMode = "application_default"
)

// Mode reports which credential source the settings select, or an explanatory
// error when they select none.
//
// A service account key wins over an OAuth client secret, because a deployment
// that has been given a key file intends to run unattended.
func (a AuthConfig) Mode() (AuthMode, error) {
	switch {
	case a.ServiceAccountFile != "":
		return AuthModeServiceAccount, nil
	case a.UseADC:
		return AuthModeADC, nil
	case a.CredentialsFile != "":
		return AuthModeInstalledApp, nil
	default:
		return "", errors.New("no credentials configured: set -service-account (with -impersonate), " +
			"-credentials, or -use-adc")
	}
}

// APIConfig tunes how AegiSheet talks to the Google Admin SDK.
type APIConfig struct {
	// Customer is the customer ID or the my_customer alias.
	Customer string
	// Domain narrows directory queries to one domain. The Admin SDK rejects
	// requests that set both customer and domain, so Domain wins when both are
	// present.
	Domain string
	// HTTPTimeout bounds a single HTTP round trip.
	HTTPTimeout time.Duration
	// MaxRetries is the number of additional attempts after a retryable failure.
	MaxRetries int
	// RetryBaseDelay and RetryMaxDelay bound the exponential backoff.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	// PageSize is the requested list page size, clamped per API.
	PageSize int64
	// Concurrency bounds parallel per-user calls in fan-out tasks.
	Concurrency int
	// UserAgent identifies this build to Google. Not operator-settable: it is
	// derived from the build metadata so that quota events stay attributable.
	UserAgent string
}

// OutputConfig describes the generated workbook.
type OutputConfig struct {
	Path      string
	Overwrite bool
	FileMode  fs.FileMode
	DryRun    bool
}

// RunConfig controls orchestration of the report tasks.
type RunConfig struct {
	// Timeout is the wall-clock budget for the whole run. Zero disables it.
	Timeout time.Duration
	// FailFast aborts on the first task error. By default a failing task is
	// recorded and the remaining tasks still run, so that one missing API scope
	// does not cost an operator the entire report.
	FailFast bool
	// Only and Skip filter sections by section name or task name.
	Only []string
	Skip []string
}

// Default returns the built-in configuration, before any external input.
func Default() *Config {
	return &Config{
		ConfigPath: DefaultConfigPath,
		Auth: AuthConfig{
			TokenFile: DefaultTokenFile,
		},
		API: APIConfig{
			Customer:       DefaultCustomer,
			HTTPTimeout:    60 * time.Second,
			MaxRetries:     4,
			RetryBaseDelay: 500 * time.Millisecond,
			RetryMaxDelay:  30 * time.Second,
			PageSize:       200,
			Concurrency:    4,
			UserAgent:      buildinfo.Get().UserAgent(),
		},
		Output: OutputConfig{
			Overwrite: true,
			FileMode:  DefaultOutputMode,
		},
		Log: logging.DefaultOptions(),
		Run: RunConfig{
			Timeout: 15 * time.Minute,
		},
	}
}

// flagBinding ties a parsed flag value back to the setter that consumes it.
type flagBinding struct {
	read func() string
	set  func(*Config, string) error
}

// Load resolves the configuration for a run.
//
// args must exclude the program name (i.e. pass os.Args[1:]). getenv may be nil,
// in which case os.Getenv is used; injecting it keeps the precedence rules
// testable. Informational output (usage, -version, -list-tasks, -print-config)
// is written to out.
func Load(args []string, getenv func(string) string, out io.Writer) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg := Default()
	table := settings()

	flagSet := flag.NewFlagSet(buildinfo.AppName, flag.ContinueOnError)
	flagSet.SetOutput(out)

	// Register one flag per declared setting. Defaults come from cfg so that -h
	// documents the real built-in values, and each flag is registered with its
	// declared type so that -h advertises the right one ("-concurrency int", not
	// "-concurrency string").
	//
	// Whatever the flag type, the value is handed back to the setting's own parser
	// as text. That keeps one parser per setting across all four configuration
	// layers instead of one per layer.
	bindings := make(map[string]flagBinding, len(table))
	for _, s := range table {
		setter := s.Set
		switch s.Kind {
		case kindBool:
			// Registered as a boolean so that -dry-run works without "=true".
			def, _ := parseBool(s.Get(cfg))
			p := flagSet.Bool(s.Flag, def, s.Usage)
			bindings[s.Flag] = flagBinding{read: func() string { return boolString(*p) }, set: setter}
		case kindInt:
			def, _ := strconv.Atoi(s.Get(cfg))
			p := flagSet.Int(s.Flag, def, s.Usage)
			bindings[s.Flag] = flagBinding{read: func() string { return strconv.Itoa(*p) }, set: setter}
		case kindInt64:
			def, _ := strconv.ParseInt(s.Get(cfg), 10, 64)
			p := flagSet.Int64(s.Flag, def, s.Usage)
			bindings[s.Flag] = flagBinding{read: func() string { return strconv.FormatInt(*p, 10) }, set: setter}
		case kindDuration:
			def, _ := time.ParseDuration(s.Get(cfg))
			p := flagSet.Duration(s.Flag, def, s.Usage)
			bindings[s.Flag] = flagBinding{read: func() string { return p.String() }, set: setter}
		default:
			p := flagSet.String(s.Flag, s.Get(cfg), s.Usage)
			bindings[s.Flag] = flagBinding{read: func() string { return *p }, set: setter}
		}
	}

	// Informational modes. They are not part of the settings table because they
	// select an action rather than configure one.
	showVersion := flagSet.Bool("version", false, "print build information and exit")
	listTasks := flagSet.Bool("list-tasks", false, "print the available report tasks and exit")
	printConfig := flagSet.Bool("print-config", false, "print the effective configuration and exit")

	flagSet.Usage = func() { writeUsage(out, flagSet) }

	if err := flagSet.Parse(args); err != nil {
		// flag.ErrHelp reaches the caller unchanged so that -h exits successfully.
		return nil, err
	}
	if flagSet.NArg() > 0 {
		return nil, fmt.Errorf("unexpected positional argument %q (all input is passed via flags)", flagSet.Arg(0))
	}

	if *showVersion {
		fmt.Fprintln(out, buildinfo.Get())
		return nil, ErrVersionRequested
	}
	if *listTasks {
		writeTaskCatalog(out)
		return nil, ErrTasksRequested
	}

	// Snapshot the flags the operator actually set. Unvisited flags must not
	// participate: their value is a default, and applying it would silently
	// outrank the INI file and the environment.
	explicit := make(map[string]string)
	flagSet.Visit(func(f *flag.Flag) {
		if binding, ok := bindings[f.Name]; ok {
			explicit[f.Name] = binding.read()
		}
	})

	// The configuration file location follows the same precedence as everything
	// else, and has to be resolved before the file can be read.
	if value, ok := lookupEnv(getenv, "CONFIG"); ok {
		cfg.ConfigPath = value
	}
	if value, ok := explicit["config"]; ok {
		cfg.ConfigPath = value
	}

	file, err := loadINIFile(cfg.ConfigPath)
	if err != nil {
		return nil, err
	}

	// Layer 2: the [global] INI section.
	if err := applyINIGlobals(cfg, file, table); err != nil {
		return nil, err
	}
	// Layer 3: the environment.
	if err := applyEnv(cfg, getenv, table); err != nil {
		return nil, err
	}
	// Layer 4: explicit flags.
	var flagErrs []error
	for name, raw := range explicit {
		if err := bindings[name].set(cfg, raw); err != nil {
			flagErrs = append(flagErrs, fmt.Errorf("flag -%s: %w", name, err))
		}
	}
	if len(flagErrs) > 0 {
		return nil, errors.Join(flagErrs...)
	}

	// The User-Agent is derived, never configured.
	cfg.API.UserAgent = buildinfo.Get().UserAgent()

	// Report sections come from the file only.
	specs, warnings, err := parseTaskSections(file, cfg)
	if err != nil {
		return nil, err
	}
	cfg.Warnings = append(cfg.Warnings, warnings...)
	cfg.Tasks = filterTasks(specs, cfg.Run, &cfg.Warnings)

	if *printConfig {
		writeEffectiveConfig(out, cfg, table)
		return nil, ErrConfigRequested
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the resolved configuration for internal consistency and for
// values the program cannot act on. It returns every problem it finds at once,
// so an operator fixing a configuration file does not have to iterate one error
// at a time.
//
// Observations that are suspicious but workable are appended to Warnings instead.
func (c *Config) Validate() error {
	var errs []error

	// --- logging -----------------------------------------------------------
	if _, err := logging.ParseLevel(c.Log.Level); err != nil {
		errs = append(errs, err)
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json", "logfmt":
	default:
		errs = append(errs, fmt.Errorf("log format %q is not supported (want text or json)", c.Log.Format))
	}
	if c.Log.IncludePII {
		c.Warnings = append(c.Warnings, "PII logging is enabled: log output contains personal data "+
			"and must be handled like the report itself")
	}

	// --- credentials -------------------------------------------------------
	mode, err := c.Auth.Mode()
	if err != nil {
		errs = append(errs, err)
	}
	switch mode {
	case AuthModeServiceAccount:
		if c.Auth.ImpersonateSubject == "" {
			errs = append(errs, errors.New("-impersonate is required with -service-account: "+
				"the Admin SDK rejects service account calls that do not impersonate an administrator"))
		}
		if c.Auth.CredentialsFile != "" {
			c.Warnings = append(c.Warnings, "both a service account key and an OAuth client secret are "+
				"configured; the service account key takes precedence")
		}
	case AuthModeInstalledApp:
		if c.Auth.TokenFile == "" {
			errs = append(errs, errors.New("-token is required with -credentials: the OAuth token needs a cache location"))
		}
	}

	// --- API ---------------------------------------------------------------
	if c.API.Customer == "" && c.API.Domain == "" {
		errs = append(errs, errors.New("either -customer or -domain must be set"))
	}
	if c.API.Domain != "" && c.API.Customer != "" && c.API.Customer != DefaultCustomer {
		c.Warnings = append(c.Warnings, fmt.Sprintf("both -customer=%s and -domain=%s are set; "+
			"the Admin SDK accepts only one, so the domain will be used", c.API.Customer, c.API.Domain))
	}
	if c.API.HTTPTimeout <= 0 {
		errs = append(errs, errors.New("-http-timeout must be greater than zero"))
	}
	if c.API.MaxRetries < 0 {
		errs = append(errs, errors.New("-max-retries cannot be negative"))
	}
	if c.API.RetryBaseDelay <= 0 {
		errs = append(errs, errors.New("-retry-base-delay must be greater than zero"))
	}
	if c.API.RetryMaxDelay < c.API.RetryBaseDelay {
		errs = append(errs, errors.New("-retry-max-delay cannot be smaller than -retry-base-delay"))
	}
	if c.API.PageSize < 1 {
		errs = append(errs, errors.New("-page-size must be at least 1"))
	}
	const maxConcurrency = 64
	if c.API.Concurrency < 1 || c.API.Concurrency > maxConcurrency {
		errs = append(errs, fmt.Errorf("-concurrency must be between 1 and %d", maxConcurrency))
	}
	for _, scope := range c.Auth.Scopes {
		if !strings.HasPrefix(scope, "https://") {
			errs = append(errs, fmt.Errorf("scope %q does not look like an OAuth scope URL", scope))
		}
	}

	// --- output ------------------------------------------------------------
	if c.Output.Path == "" && !c.Output.DryRun {
		errs = append(errs, errors.New("no output path configured: set -output (or -dry-run to validate without writing)"))
	}
	if c.Output.FileMode.Perm()&0o077 != 0 && c.Output.Path != "" {
		c.Warnings = append(c.Warnings, fmt.Sprintf("output mode 0%o allows access beyond the owner; "+
			"the workbook contains personal data", c.Output.FileMode.Perm()))
	}

	// --- execution ---------------------------------------------------------
	if c.Run.Timeout < 0 {
		errs = append(errs, errors.New("-timeout cannot be negative"))
	}
	if len(c.Tasks) == 0 {
		errs = append(errs, fmt.Errorf("no report sections to run: %s defines none, or -only/-skip excluded them all",
			c.ConfigPath))
	}

	// --- per-task ----------------------------------------------------------
	for _, task := range c.Tasks {
		if err := c.validateTask(task); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// validateTask checks one report section, appending advisory notes to Warnings.
func (c *Config) validateTask(task TaskSpec) error {
	var errs []error

	if task.LookbackDays < 0 {
		errs = append(errs, fmt.Errorf("section %q: lookbackDays cannot be negative", task.Section))
	}
	if !task.StartTime.IsZero() && !task.EndTime.IsZero() && task.EndTime.Before(task.StartTime) {
		errs = append(errs, fmt.Errorf("section %q: endTime is before startTime", task.Section))
	}
	if task.MaxRows < 0 {
		errs = append(errs, fmt.Errorf("section %q: maxRows cannot be negative", task.Section))
	}

	if task.ID == TaskAuditActivities && task.AppName != "" &&
		!slices.Contains(AuditApplications, strings.ToLower(task.AppName)) {
		// Not an error: Google publishes new applications faster than this list is
		// updated, and reporting on one of them is a legitimate use.
		c.Warnings = append(c.Warnings, fmt.Sprintf("section %q: appName %q is not in the list of known "+
			"Reports applications; sending it to the API unchanged", task.Section, task.AppName))
	}

	for key := range task.Extra {
		c.Warnings = append(c.Warnings, fmt.Sprintf("section %q: key %q is not used by task %s (typo?)",
			task.Section, key, task.ID))
	}

	return errors.Join(errs...)
}

// LogAttrs renders the effective configuration for a single startup log record.
// Credential *paths* are included because they are needed to diagnose setup
// problems; credential *contents* never leave the auth package.
func (c *Config) LogAttrs() []slog.Attr {
	mode, _ := c.Auth.Mode()
	sections := make([]string, 0, len(c.Tasks))
	for _, task := range c.Tasks {
		sections = append(sections, fmt.Sprintf("%s=%s", task.Section, task.ID))
	}

	return []slog.Attr{
		slog.String("config_file", c.ConfigPath),
		slog.Group("auth",
			slog.String("mode", string(mode)),
			slog.String("credentials_file", c.Auth.CredentialsFile),
			slog.String("service_account_file", c.Auth.ServiceAccountFile),
			slog.String("token_file", c.Auth.TokenFile),
			// The impersonated administrator is a person: the logging handler masks
			// this key unless PII logging was explicitly enabled.
			slog.String("subject", c.Auth.ImpersonateSubject),
			slog.Bool("non_interactive", c.Auth.NonInteractive),
		),
		slog.Group("api",
			slog.String("customer", c.API.Customer),
			slog.String("domain", c.API.Domain),
			// Durations are rendered as strings: a JSON handler would otherwise emit
			// raw nanoseconds, and these values exist to be read by an operator.
			slog.String("http_timeout", c.API.HTTPTimeout.String()),
			slog.Int("max_retries", c.API.MaxRetries),
			slog.Int64("page_size", c.API.PageSize),
			slog.Int("concurrency", c.API.Concurrency),
		),
		slog.Group("output",
			slog.String("path", c.Output.Path),
			slog.String("mode", fmt.Sprintf("0%o", c.Output.FileMode.Perm())),
			slog.Bool("overwrite", c.Output.Overwrite),
			slog.Bool("dry_run", c.Output.DryRun),
		),
		slog.Group("run",
			slog.String("timeout", c.Run.Timeout.String()),
			slog.Bool("fail_fast", c.Run.FailFast),
			slog.Int("tasks", len(c.Tasks)),
			slog.String("sections", strings.Join(sections, ",")),
		),
	}
}

// filterTasks applies -only/-skip and the per-section enabled flag, recording a
// warning for every filter term that matched nothing (a silent typo in -only
// would otherwise produce an empty report).
func filterTasks(specs []TaskSpec, run RunConfig, warnings *[]string) []TaskSpec {
	only := NewStringSet(run.Only)
	skip := NewStringSet(run.Skip)
	matched := make(StringSet, len(only)+len(skip))

	out := make([]TaskSpec, 0, len(specs))
	for _, spec := range specs {
		// A filter term may name either the section or the task.
		names := []string{spec.Section, string(spec.ID), spec.RawName}

		if only.Len() > 0 {
			hit := false
			for _, name := range names {
				if only.Contains(name) {
					hit = true
					matched[strings.ToLower(name)] = struct{}{}
				}
			}
			if !hit {
				continue
			}
		}
		if skip.Len() > 0 {
			skipped := false
			for _, name := range names {
				if skip.Contains(name) {
					skipped = true
					matched[strings.ToLower(name)] = struct{}{}
				}
			}
			if skipped {
				continue
			}
		}
		if !spec.Enabled {
			continue
		}
		out = append(out, spec)
	}

	for _, term := range append(append([]string{}, run.Only...), run.Skip...) {
		if !matched.Contains(term) {
			*warnings = append(*warnings, fmt.Sprintf("filter %q matched no section or task", term))
		}
	}
	return out
}

// lookupEnv reads EnvPrefix+suffix, reporting whether it was set to a non-empty
// value. An empty variable is treated as unset so that `FOO= program` does not
// blank out a configured value by accident.
func lookupEnv(getenv func(string) string, suffix string) (string, bool) {
	value := strings.TrimSpace(getenv(EnvPrefix + suffix))
	if value == "" {
		return "", false
	}
	return value, true
}

// applyEnv layers environment variables over the configuration.
func applyEnv(cfg *Config, getenv func(string) string, table []setting) error {
	var errs []error
	for _, s := range table {
		if s.Env == "" {
			continue
		}
		value, ok := lookupEnv(getenv, s.Env)
		if !ok {
			continue
		}
		if err := s.Set(cfg, value); err != nil {
			errs = append(errs, fmt.Errorf("environment variable %s%s: %w", EnvPrefix, s.Env, err))
		}
	}
	return errors.Join(errs...)
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// writeUsage prints the help screen: what the program does, how it is invoked,
// every flag, and where else the same settings can come from.
func writeUsage(out io.Writer, flagSet *flag.FlagSet) {
	fmt.Fprintf(out, "%s\n\n", buildinfo.Get())
	fmt.Fprintf(out, "AegiSheet collects Google Workspace administrative data and writes it to an\n"+
		"Excel workbook, one worksheet per report section.\n\n")
	fmt.Fprintf(out, "Usage:\n  %s [flags]\n\n", buildinfo.AppName)
	fmt.Fprintf(out, "Flags:\n")
	flagSet.PrintDefaults()
	fmt.Fprintf(out, "\nConfiguration precedence (lowest to highest):\n"+
		"  built-in defaults, [%s] section of the INI file, %s* environment\n"+
		"  variables, command line flags.\n", GlobalSection, EnvPrefix)
	fmt.Fprintf(out, "\nReport sections are defined only in the INI file (%s by default).\n",
		DefaultConfigPath)
	fmt.Fprintf(out, "Run with -list-tasks for the available tasks, or -print-config to see how the\n"+
		"layers resolved.\n")
}

// writeTaskCatalog prints the available tasks with their accepted aliases.
func writeTaskCatalog(out io.Writer) {
	fmt.Fprintf(out, "Available report tasks (use as `task=` inside an INI section):\n\n")
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "TASK\tDESCRIPTION\tALIASES\n")
	for _, def := range TaskCatalog() {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", def.ID, def.Summary, strings.Join(def.Aliases, ", "))
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\nTask names are matched ignoring case, dashes and underscores, so the legacy\n"+
		"names from earlier versions keep working.\n")
}

// writeEffectiveConfig prints every setting with its resolved value, which is the
// quickest way to answer "why did it use that credential file?".
func writeEffectiveConfig(out io.Writer, cfg *Config, table []setting) {
	fmt.Fprintf(out, "# effective configuration for %s\n", cfg.ConfigPath)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "FLAG\tENV\tINI KEY\tVALUE\n")
	for _, s := range table {
		env, iniKey := "-", "-"
		if s.Env != "" {
			env = EnvPrefix + s.Env
		}
		if s.INI != "" {
			iniKey = s.INI
		}
		fmt.Fprintf(tw, "-%s\t%s\t%s\t%s\n", s.Flag, env, iniKey, s.Get(cfg))
	}
	_ = tw.Flush()

	fmt.Fprintf(out, "\n# report sections after -only/-skip filtering\n")
	tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "SECTION\tSHEET\tTASK\tPARAMETERS\n")
	for _, task := range cfg.Tasks {
		params := make([]string, 0, 4)
		if task.UserKey != "" {
			params = append(params, "userKey="+task.UserKey)
		}
		if task.AppName != "" {
			params = append(params, "appName="+task.AppName)
		}
		if task.EventName != "" {
			params = append(params, "eventName="+task.EventName)
		}
		if task.LookbackDays > 0 {
			params = append(params, fmt.Sprintf("lookbackDays=%d", task.LookbackDays))
		}
		if task.Whitelist.Len() > 0 {
			params = append(params, fmt.Sprintf("whitelist=%d entries", task.Whitelist.Len()))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", task.Section, task.Sheet, task.ID, strings.Join(params, " "))
	}
	_ = tw.Flush()

	for _, warning := range cfg.Warnings {
		fmt.Fprintf(out, "\n# warning: %s\n", warning)
	}
}
