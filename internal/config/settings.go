package config

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is prepended to every environment variable name recognised by
// AegiSheet, e.g. AEGISHEET_LOG_LEVEL.
const EnvPrefix = "AEGISHEET_"

// setting describes one operator-visible knob in every spelling it accepts.
//
// A single table drives command line flags, environment variables, the [global]
// INI section, the --print-config dump and the -h usage text. Adding a knob in
// one place therefore makes it available through all channels, which is what
// keeps the three configuration surfaces from drifting apart.
type setting struct {
	// Flag is the command line flag name, without the leading dash.
	Flag string
	// Env is the environment variable name after EnvPrefix. Empty disables the
	// environment binding.
	Env string
	// INI is the key name inside the [global] section. Empty means the knob is
	// not settable from the configuration file (true only for the path of that
	// file itself). Keys are compared case-insensitively.
	INI string
	// Usage is the help text shown by -h.
	Usage string
	// Kind selects the flag type used to register this setting.
	Kind kind
	// Get renders the current value, for defaults in -h and for --print-config.
	Get func(*Config) string
	// Set parses a textual value into the configuration.
	Set func(*Config, string) error
}

// kind selects how a setting is registered as a command line flag.
//
// Every setting is ultimately parsed from a string, whichever layer it came from,
// so the kind affects only two things: the type hint shown by -h, and the syntax
// the flag package itself accepts before the shared parsers run.
type kind int

const (
	kindString kind = iota // the zero value
	kindBool
	kindInt
	kindInt64
	kindDuration
)

// settings returns the complete configuration surface of the program.
func settings() []setting {
	return []setting{
		// ---------------------------------------------------------------- input
		{
			Flag:  "config",
			Env:   "CONFIG",
			Usage: "path to the INI file describing the report sections",
			Get:   func(c *Config) string { return c.ConfigPath },
			Set:   func(c *Config, v string) error { c.ConfigPath = v; return nil },
		},

		// --------------------------------------------------------------- output
		{
			Flag:  "output",
			Env:   "OUTPUT",
			INI:   "output",
			Usage: "path of the generated .xlsx workbook",
			Get:   func(c *Config) string { return c.Output.Path },
			Set:   func(c *Config, v string) error { c.Output.Path = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "output-mode",
			Env:   "OUTPUT_MODE",
			INI:   "outputMode",
			Usage: "octal permissions for the generated workbook (it contains personal data)",
			Get:   func(c *Config) string { return "0" + strconv.FormatUint(uint64(c.Output.FileMode.Perm()), 8) },
			Set: func(c *Config, v string) error {
				mode, err := parseFileMode(v)
				if err != nil {
					return err
				}
				c.Output.FileMode = mode
				return nil
			},
		},
		{
			Flag:  "overwrite",
			Env:   "OVERWRITE",
			INI:   "overwrite",
			Usage: "replace the output file when it already exists",
			Kind:  kindBool,
			Get:   func(c *Config) string { return strconv.FormatBool(c.Output.Overwrite) },
			Set:   func(c *Config, v string) error { return setBool(v, &c.Output.Overwrite) },
		},
		{
			Flag:  "dry-run",
			Env:   "DRY_RUN",
			INI:   "dryRun",
			Usage: "run every task but do not write the workbook to disk",
			Kind:  kindBool,
			Get:   func(c *Config) string { return strconv.FormatBool(c.Output.DryRun) },
			Set:   func(c *Config, v string) error { return setBool(v, &c.Output.DryRun) },
		},

		// ------------------------------------------------------- authentication
		{
			Flag:  "credentials",
			Env:   "CREDENTIALS_FILE",
			INI:   "credentialsFile",
			Usage: "OAuth client secret JSON for the installed-application flow",
			Get:   func(c *Config) string { return c.Auth.CredentialsFile },
			Set:   func(c *Config, v string) error { c.Auth.CredentialsFile = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "token",
			Env:   "TOKEN_FILE",
			INI:   "tokenFile",
			Usage: "cache file for the OAuth token obtained by the installed-application flow",
			Get:   func(c *Config) string { return c.Auth.TokenFile },
			Set:   func(c *Config, v string) error { c.Auth.TokenFile = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "service-account",
			Env:   "SERVICE_ACCOUNT_FILE",
			INI:   "serviceAccountFile",
			Usage: "service account key JSON; requires -impersonate (recommended for unattended runs)",
			Get:   func(c *Config) string { return c.Auth.ServiceAccountFile },
			Set:   func(c *Config, v string) error { c.Auth.ServiceAccountFile = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "impersonate",
			Env:   "IMPERSONATE_SUBJECT",
			INI:   "impersonateSubject",
			Usage: "administrator address the service account impersonates via domain-wide delegation",
			Get:   func(c *Config) string { return c.Auth.ImpersonateSubject },
			Set:   func(c *Config, v string) error { c.Auth.ImpersonateSubject = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "use-adc",
			Env:   "USE_ADC",
			INI:   "useADC",
			Usage: "authenticate with Application Default Credentials instead of a key file",
			Kind:  kindBool,
			Get:   func(c *Config) string { return strconv.FormatBool(c.Auth.UseADC) },
			Set:   func(c *Config, v string) error { return setBool(v, &c.Auth.UseADC) },
		},
		{
			Flag:  "non-interactive",
			Env:   "NON_INTERACTIVE",
			INI:   "nonInteractive",
			Usage: "fail instead of prompting on the terminal for an OAuth authorisation code",
			Kind:  kindBool,
			Get:   func(c *Config) string { return strconv.FormatBool(c.Auth.NonInteractive) },
			Set:   func(c *Config, v string) error { return setBool(v, &c.Auth.NonInteractive) },
		},
		{
			Flag:  "scopes",
			Env:   "SCOPES",
			INI:   "scopes",
			Usage: "comma separated OAuth scopes; empty uses the built-in least-privilege set",
			Get:   func(c *Config) string { return strings.Join(c.Auth.Scopes, ",") },
			Set:   func(c *Config, v string) error { c.Auth.Scopes = splitList(v); return nil },
		},

		// ------------------------------------------------------------------ API
		{
			Flag:  "customer",
			Env:   "CUSTOMER_ID",
			INI:   "customer",
			Usage: "customer to query; the literal my_customer means the caller's own tenant",
			Get:   func(c *Config) string { return c.API.Customer },
			Set:   func(c *Config, v string) error { c.API.Customer = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "domain",
			Env:   "DOMAIN",
			INI:   "domain",
			Usage: "restrict directory queries to a single domain (mutually exclusive with -customer)",
			Get:   func(c *Config) string { return c.API.Domain },
			Set:   func(c *Config, v string) error { c.API.Domain = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "http-timeout",
			Kind:  kindDuration,
			Env:   "HTTP_TIMEOUT",
			INI:   "httpTimeout",
			Usage: "timeout for a single HTTP request to Google",
			Get:   func(c *Config) string { return c.API.HTTPTimeout.String() },
			Set:   func(c *Config, v string) error { return setDuration(v, &c.API.HTTPTimeout) },
		},
		{
			Flag:  "max-retries",
			Kind:  kindInt,
			Env:   "MAX_RETRIES",
			INI:   "maxRetries",
			Usage: "additional attempts after a retryable API failure (0 disables retrying)",
			Get:   func(c *Config) string { return strconv.Itoa(c.API.MaxRetries) },
			Set:   func(c *Config, v string) error { return setInt(v, &c.API.MaxRetries) },
		},
		{
			Flag:  "retry-base-delay",
			Kind:  kindDuration,
			Env:   "RETRY_BASE_DELAY",
			INI:   "retryBaseDelay",
			Usage: "initial backoff delay between retries",
			Get:   func(c *Config) string { return c.API.RetryBaseDelay.String() },
			Set:   func(c *Config, v string) error { return setDuration(v, &c.API.RetryBaseDelay) },
		},
		{
			Flag:  "retry-max-delay",
			Kind:  kindDuration,
			Env:   "RETRY_MAX_DELAY",
			INI:   "retryMaxDelay",
			Usage: "upper bound on the exponential backoff delay",
			Get:   func(c *Config) string { return c.API.RetryMaxDelay.String() },
			Set:   func(c *Config, v string) error { return setDuration(v, &c.API.RetryMaxDelay) },
		},
		{
			Flag:  "page-size",
			Kind:  kindInt64,
			Env:   "PAGE_SIZE",
			INI:   "pageSize",
			Usage: "requested page size for list calls; clamped to each API's own maximum",
			Get:   func(c *Config) string { return strconv.FormatInt(c.API.PageSize, 10) },
			Set:   func(c *Config, v string) error { return setInt64(v, &c.API.PageSize) },
		},
		{
			Flag:  "concurrency",
			Kind:  kindInt,
			Env:   "CONCURRENCY",
			INI:   "concurrency",
			Usage: "parallel per-user API calls for tasks that fan out over the directory",
			Get:   func(c *Config) string { return strconv.Itoa(c.API.Concurrency) },
			Set:   func(c *Config, v string) error { return setInt(v, &c.API.Concurrency) },
		},

		// ------------------------------------------------------------ execution
		{
			Flag:  "timeout",
			Kind:  kindDuration,
			Env:   "TIMEOUT",
			INI:   "timeout",
			Usage: "wall-clock budget for the whole run (0 disables the budget)",
			Get:   func(c *Config) string { return c.Run.Timeout.String() },
			Set:   func(c *Config, v string) error { return setDuration(v, &c.Run.Timeout) },
		},
		{
			Flag:  "fail-fast",
			Env:   "FAIL_FAST",
			INI:   "failFast",
			Usage: "abort the run on the first failing task instead of continuing",
			Kind:  kindBool,
			Get:   func(c *Config) string { return strconv.FormatBool(c.Run.FailFast) },
			Set:   func(c *Config, v string) error { return setBool(v, &c.Run.FailFast) },
		},
		{
			Flag:  "only",
			Env:   "ONLY",
			INI:   "only",
			Usage: "comma separated section or task names to run, excluding all others",
			Get:   func(c *Config) string { return strings.Join(c.Run.Only, ",") },
			Set:   func(c *Config, v string) error { c.Run.Only = splitList(v); return nil },
		},
		{
			Flag:  "skip",
			Env:   "SKIP",
			INI:   "skip",
			Usage: "comma separated section or task names to exclude from the run",
			Get:   func(c *Config) string { return strings.Join(c.Run.Skip, ",") },
			Set:   func(c *Config, v string) error { c.Run.Skip = splitList(v); return nil },
		},

		// -------------------------------------------------------------- logging
		{
			Flag:  "log-level",
			Env:   "LOG_LEVEL",
			INI:   "logLevel",
			Usage: "log verbosity: debug, info, warn or error",
			Get:   func(c *Config) string { return c.Log.Level },
			Set:   func(c *Config, v string) error { c.Log.Level = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "log-format",
			Env:   "LOG_FORMAT",
			INI:   "logFormat",
			Usage: "log encoding: text for humans, json for log collectors",
			Get:   func(c *Config) string { return c.Log.Format },
			Set:   func(c *Config, v string) error { c.Log.Format = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "log-file",
			Env:   "LOG_FILE",
			INI:   "logFile",
			Usage: "log destination: stderr, stdout or a file path to append to",
			Get:   func(c *Config) string { return c.Log.Output },
			Set:   func(c *Config, v string) error { c.Log.Output = strings.TrimSpace(v); return nil },
		},
		{
			Flag:  "log-source",
			Env:   "LOG_SOURCE",
			INI:   "logSource",
			Usage: "annotate log records with the emitting source file and line",
			Kind:  kindBool,
			Get:   func(c *Config) string { return strconv.FormatBool(c.Log.AddSource) },
			Set:   func(c *Config, v string) error { return setBool(v, &c.Log.AddSource) },
		},
		{
			Flag:  "log-pii",
			Env:   "LOG_PII",
			INI:   "logPII",
			Usage: "log personal data unmasked; only for short debugging sessions",
			Kind:  kindBool,
			Get:   func(c *Config) string { return strconv.FormatBool(c.Log.IncludePII) },
			Set:   func(c *Config, v string) error { return setBool(v, &c.Log.IncludePII) },
		},
	}
}

// ---------------------------------------------------------------------------
// Parsing helpers shared by the flag, environment and INI paths. Every knob is
// ultimately set from a string, so a single set of converters gives all three
// surfaces identical accepted syntax and identical error messages.
// ---------------------------------------------------------------------------

func setBool(raw string, dst *bool) error {
	value, err := parseBool(raw)
	if err != nil {
		return err
	}
	*dst = value
	return nil
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", raw)
	}
}

func setDuration(raw string, dst *time.Duration) error {
	trimmed := strings.TrimSpace(raw)
	value, err := time.ParseDuration(trimmed)
	if err != nil {
		// Accept a bare number of seconds as a convenience for INI files written
		// by operators who do not know Go's duration syntax.
		if seconds, secErr := strconv.Atoi(trimmed); secErr == nil {
			*dst = time.Duration(seconds) * time.Second
			return nil
		}
		return fmt.Errorf("invalid duration %q (want e.g. 90s, 5m, 1h)", raw)
	}
	*dst = value
	return nil
}

func setInt(raw string, dst *int) error {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid integer %q", raw)
	}
	*dst = value
	return nil
}

func setInt64(raw string, dst *int64) error {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid integer %q", raw)
	}
	*dst = value
	return nil
}

// parseFileMode accepts octal permissions with or without a leading zero.
func parseFileMode(raw string) (fs.FileMode, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "0o")
	value, err := strconv.ParseUint(trimmed, 8, 32)
	if err != nil || value > 0o777 {
		return 0, fmt.Errorf("invalid file mode %q (want octal such as 0600)", raw)
	}
	return fs.FileMode(value), nil
}

// splitList parses a comma separated list, trimming blanks and dropping empties.
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
