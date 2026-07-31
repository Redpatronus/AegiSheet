// Package logging builds the structured logger used across AegiSheet and
// enforces the project's data-handling rules on everything that is logged.
//
// Two concerns are solved centrally here instead of at every call site:
//
//  1. Sink and format selection. Operators choose text (human readable, for
//     interactive runs) or JSON (for log shippers such as Cloud Logging, Splunk
//     or Loki) and route output to stderr, stdout or a file — without any
//     application code changing.
//
//  2. Redaction. AegiSheet reads Google Workspace directory data, which is
//     personally identifiable information (PII), and OAuth credentials, which
//     are secrets. Neither belongs in a log aggregator by default. Redaction is
//     implemented as a slog.Handler decorator, which means a call site can write
//     the natural thing — logger.Info("user scanned", "email", u.PrimaryEmail) —
//     and the handler guarantees the value is masked unless PII logging was
//     explicitly enabled for a debugging session.
//
// Secrets are redacted unconditionally: there is no configuration flag that
// causes an OAuth token or client secret to be written to a log.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// RedactedSecret replaces credential-bearing values. It is distinctive on purpose
// so that a grep over a log archive shows where sensitive data was withheld.
const RedactedSecret = "[REDACTED]"

// secretKeys are attribute names whose values are credentials. Values under
// these keys are destroyed before reaching any handler, regardless of options.
var secretKeys = map[string]struct{}{
	"access_token":  {},
	"authorization": {},
	"auth_code":     {},
	"client_secret": {},
	"code":          {},
	"credentials":   {},
	"id_token":      {},
	"password":      {},
	"private_key":   {},
	"refresh_token": {},
	"secret":        {},
	"token":         {},
}

// piiKeys are attribute names whose values identify a natural person. They are
// masked unless Options.IncludePII is set, which an operator may do temporarily
// while debugging a specific account.
//
// Note that file *paths* to credentials are not secrets themselves and stay
// readable — an operator needs them to diagnose configuration problems.
var piiKeys = map[string]struct{}{
	"actor":        {},
	"email":        {},
	"group_email":  {},
	"impersonate":  {},
	"ip":           {},
	"ip_address":   {},
	"member":       {},
	"member_email": {},
	"owner":        {},
	"subject":      {},
	"user":         {},
	"user_email":   {},
	"user_key":     {},
	"user_name":    {},
}

// Options is the operator-facing logging configuration. It is populated from
// defaults, the INI file, environment variables and command line flags; see
// package config for the precedence rules.
type Options struct {
	// Level is one of debug, info, warn, error (case-insensitive).
	Level string
	// Format is either "text" or "json".
	Format string
	// Output is "stderr", "stdout", or a filesystem path to append to.
	Output string
	// AddSource attaches source file and line to every record. Useful when
	// diagnosing the tool itself; adds noise otherwise.
	AddSource bool
	// IncludePII disables masking of person-identifying attributes. Off by
	// default; enabling it makes logs subject to the same handling rules as the
	// generated report.
	IncludePII bool
}

// DefaultOptions returns the safe baseline: human readable, on stderr, no PII.
func DefaultOptions() Options {
	return Options{
		Level:      "info",
		Format:     "text",
		Output:     "stderr",
		AddSource:  false,
		IncludePII: false,
	}
}

// LogFileMode is applied to a log file created by New. Logs may contain host and
// account metadata, so they are not world readable.
const LogFileMode = 0o640

// New builds a logger from opts.
//
// The returned io.Closer owns the log file when Options.Output points at one; it
// is a no-op for the standard streams. Callers must close it on shutdown, and
// must not close it before the last log call.
func New(opts Options) (*slog.Logger, io.Closer, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, nil, err
	}

	sink, closer, err := openSink(opts.Output)
	if err != nil {
		return nil, nil, err
	}

	handlerOpts := &slog.HandlerOptions{
		Level:     level,
		AddSource: opts.AddSource,
	}

	var base slog.Handler
	switch strings.ToLower(strings.TrimSpace(opts.Format)) {
	case "", "text", "logfmt":
		base = slog.NewTextHandler(sink, handlerOpts)
	case "json":
		base = slog.NewJSONHandler(sink, handlerOpts)
	default:
		_ = closer.Close()
		return nil, nil, fmt.Errorf("unsupported log format %q (want text or json)", opts.Format)
	}

	// The redacting decorator wraps the concrete handler so that no attribute can
	// reach the sink without passing through the masking rules.
	logger := slog.New(&redactingHandler{next: base, includePII: opts.IncludePII})
	return logger, closer, nil
}

// ParseLevel maps an operator-supplied level name onto a slog.Level.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q (want debug, info, warn or error)", name)
	}
}

// Discard returns a logger that drops everything. Intended for tests and for the
// narrow window before configuration has been parsed.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// openSink resolves the Output setting into a writer plus its owner.
func openSink(output string) (io.Writer, io.Closer, error) {
	switch strings.ToLower(strings.TrimSpace(output)) {
	case "", "stderr":
		return os.Stderr, nopCloser{}, nil
	case "stdout":
		// stdout is deliberately supported but not the default: keeping logs on
		// stderr leaves stdout free for future machine-readable summaries.
		return os.Stdout, nopCloser{}, nil
	}

	// Anything else is a path. Append rather than truncate so that repeated runs
	// (typically from cron) accumulate an audit trail.
	if dir := filepath.Dir(output); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, nil, fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, LogFileMode)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", output, err)
	}
	return f, f, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// redactingHandler masks sensitive attribute values before delegating to the
// wrapped handler. It implements slog.Handler.
type redactingHandler struct {
	next       slog.Handler
	includePII bool
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	// Records are immutable by contract, so build a redacted copy. Attribute
	// counts here are small (single digits), making the copy irrelevant to cost.
	out := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	rec.Attrs(func(attr slog.Attr) bool {
		out.AddAttrs(h.redact(attr))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Attributes bound via With() bypass Handle on the wrapped handler, so they
	// must be redacted at bind time.
	redacted := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		redacted[i] = h.redact(attr)
	}
	return &redactingHandler{next: h.next.WithAttrs(redacted), includePII: h.includePII}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name), includePII: h.includePII}
}

// redact applies the masking rules to a single attribute, recursing into groups.
func (h *redactingHandler) redact(attr slog.Attr) slog.Attr {
	// Resolve LogValuer implementations first: the classification below must see
	// the value that would actually be written.
	attr.Value = attr.Value.Resolve()

	if attr.Value.Kind() == slog.KindGroup {
		group := attr.Value.Group()
		redacted := make([]slog.Attr, len(group))
		for i, member := range group {
			redacted[i] = h.redact(member)
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(redacted...)}
	}

	key := normaliseKey(attr.Key)
	if _, isSecret := secretKeys[key]; isSecret {
		return slog.String(attr.Key, RedactedSecret)
	}
	if _, isPII := piiKeys[key]; isPII && !h.includePII {
		return slog.String(attr.Key, MaskPII(attr.Value.String()))
	}
	return attr
}

// normaliseKey lowercases an attribute key and normalises separators so that
// "userEmail", "user-email" and "user_email" all classify identically.
func normaliseKey(key string) string {
	var b strings.Builder
	b.Grow(len(key) + 4)
	for i, r := range key {
		switch {
		case r == '-' || r == '.' || r == ' ':
			b.WriteByte('_')
		case r >= 'A' && r <= 'Z':
			// Insert a separator at lowerUpper boundaries so camelCase keys map
			// onto the snake_case names in the classification tables.
			if i > 0 && isLower(rune(key[i-1])) {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isLower(r rune) bool { return r >= 'a' && r <= 'z' }

// MaskPII reduces a person-identifying string to a non-identifying shape that is
// still useful for correlating log lines.
//
// Email addresses keep their domain, because the domain is an organisational
// attribute needed to tell tenants apart, and keep the first character of the
// local part so that two different masked users remain distinguishable in most
// cases:
//
//	alice@example.com -> a***@example.com
//
// Anything else keeps at most its first two characters.
func MaskPII(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		// Nothing was there to hide; a placeholder here would only suggest that
		// something was withheld.
		return ""
	}

	if at := strings.LastIndex(value, "@"); at > 0 && at < len(value)-1 {
		local, domain := value[:at], value[at+1:]
		return maskLocalPart(local) + "@" + domain
	}

	// Bare identifiers (IP addresses, member IDs, display names).
	runes := []rune(value)
	if len(runes) <= 2 {
		return "***"
	}
	return string(runes[:2]) + "***"
}

func maskLocalPart(local string) string {
	runes := []rune(local)
	if len(runes) == 0 {
		return "***"
	}
	return string(runes[:1]) + "***"
}

// SanitizePath makes an HTTP request path safe to log.
//
// Google Admin SDK paths embed identity in the URL — for example
// /admin/directory/v1/users/alice@example.com/asps. Logging the raw path would
// leak PII through a channel that has no attribute key to classify, so each
// identifying segment is replaced by a placeholder while the route shape, which
// is what makes the log line useful, is preserved.
func SanitizePath(path string) string {
	if path == "" {
		return ""
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		switch {
		case segment == "":
			// Leading/duplicate separators: nothing to hide.
		case strings.Contains(segment, "@"):
			segments[i] = "{email}"
		case isOpaqueID(segment):
			segments[i] = "{id}"
		}
	}
	return strings.Join(segments, "/")
}

// isOpaqueID reports whether a path segment looks like a generated identifier
// (Workspace group and member IDs, numeric customer IDs) rather than a fixed
// part of the API route.
func isOpaqueID(segment string) bool {
	const minIDLen = 8
	if len(segment) < minIDLen {
		return false
	}
	digits := 0
	for _, r := range segment {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	// Route segments ("directory", "activities") contain no digits; identifiers
	// are majority digits in every Workspace form observed.
	return digits*2 >= len(segment)
}
