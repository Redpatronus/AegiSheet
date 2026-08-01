package logging

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestLogger builds a JSON logger over a buffer so that records can be decoded
// and inspected field by field.
func newTestLogger(t *testing.T, includePII bool) (*slog.Logger, *strings.Builder) {
	t.Helper()
	buf := &strings.Builder{}
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(&redactingHandler{next: handler, includePII: includePII}), buf
}

// decodeRecords parses the emitted JSON lines.
func decodeRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		record := map[string]any{}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestSecretsAreAlwaysRedacted(t *testing.T) {
	// Secrets must not be loggable, even when PII logging has been switched on.
	for _, includePII := range []bool{false, true} {
		logger, buf := newTestLogger(t, includePII)
		logger.Info("token exchanged",
			"refresh_token", "1//super-secret-value",
			"client_secret", "GOCSPX-abcdef",
			"authorization", "Bearer ya29.a0Af",
			"password", "hunter2")

		raw := buf.String()
		for _, secret := range []string{"super-secret-value", "GOCSPX-abcdef", "ya29.a0Af", "hunter2"} {
			if strings.Contains(raw, secret) {
				t.Errorf("includePII=%v: the log contains the secret %q: %s", includePII, secret, raw)
			}
		}
		if !strings.Contains(raw, RedactedSecret) {
			t.Errorf("includePII=%v: no redaction marker in %s", includePII, raw)
		}
	}
}

func TestPIIIsMaskedByDefault(t *testing.T) {
	logger, buf := newTestLogger(t, false)
	logger.Info("user scanned", "email", "alice@example.com", "ip_address", "203.0.113.42")

	records := decodeRecords(t, buf.String())
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	// The domain survives because it identifies the tenant, not the person; the
	// local part does not.
	if got := records[0]["email"]; got != "a***@example.com" {
		t.Errorf("email = %v, want a***@example.com", got)
	}
	if got := records[0]["ip_address"]; got != "20***" {
		t.Errorf("ip_address = %v, want 20***", got)
	}
}

func TestPIIPassesThroughWhenExplicitlyEnabled(t *testing.T) {
	logger, buf := newTestLogger(t, true)
	logger.Info("user scanned", "email", "alice@example.com")

	if !strings.Contains(buf.String(), "alice@example.com") {
		t.Errorf("PII logging was enabled but the address was still masked: %s", buf.String())
	}
}

func TestRedactionAppliesToBoundAndGroupedAttributes(t *testing.T) {
	// Attributes bound with With() bypass Handle on the wrapped handler, and grouped
	// attributes are nested; both paths need their own redaction.
	logger, buf := newTestLogger(t, false)
	logger.With("email", "bound@example.com").Info("bound attribute")
	logger.Info("grouped attribute", slog.Group("auth",
		slog.String("subject", "admin@example.com"),
		slog.String("token", "secret-token-value")))

	raw := buf.String()
	for _, leaked := range []string{"bound@example.com", "admin@example.com", "secret-token-value"} {
		if strings.Contains(raw, leaked) {
			t.Errorf("the log contains %q: %s", leaked, raw)
		}
	}
	if !strings.Contains(raw, "b***@example.com") || !strings.Contains(raw, "a***@example.com") {
		t.Errorf("expected masked addresses in %s", raw)
	}
}

func TestKeyNamingVariantsAreClassified(t *testing.T) {
	// The same concept is spelled several ways across call sites; all of them must
	// classify as PII.
	logger, buf := newTestLogger(t, false)
	logger.Info("variants",
		"userEmail", "camel@example.com",
		"user-email", "kebab@example.com",
		"user_email", "snake@example.com")

	raw := buf.String()
	for _, leaked := range []string{"camel@example.com", "kebab@example.com", "snake@example.com"} {
		if strings.Contains(raw, leaked) {
			t.Errorf("the log contains %q: %s", leaked, raw)
		}
	}
}

func TestNonSensitiveAttributesAreUntouched(t *testing.T) {
	logger, buf := newTestLogger(t, false)
	logger.Info("section completed", "sheet", "Group Members", "rows", 421)

	records := decodeRecords(t, buf.String())
	if got := records[0]["sheet"]; got != "Group Members" {
		t.Errorf("sheet = %v, want Group Members", got)
	}
	if got := records[0]["rows"]; got != float64(421) {
		t.Errorf("rows = %v, want 421", got)
	}
}

func TestMaskPII(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "alice@example.com", want: "a***@example.com"},
		{in: "a@example.com", want: "a***@example.com"},
		{in: "203.0.113.42", want: "20***"},
		{in: "ab", want: "***"},
		// An absent value has nothing to hide, so it stays absent.
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "no-at-sign", want: "no***"},
	}
	for _, test := range tests {
		if got := MaskPII(test.in); got != test.want {
			t.Errorf("MaskPII(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestSanitizePath(t *testing.T) {
	// Admin SDK routes embed identity in the path, which is logged without any
	// attribute key to classify it.
	tests := []struct {
		in   string
		want string
	}{
		{
			in:   "/admin/directory/v1/users/alice@example.com/asps",
			want: "/admin/directory/v1/users/{email}/asps",
		},
		{
			in:   "/admin/directory/v1/groups/03x8mnp12345678/members",
			want: "/admin/directory/v1/groups/{id}/members",
		},
		{
			in:   "/admin/directory/v1/users",
			want: "/admin/directory/v1/users",
		},
		{in: "", want: ""},
	}
	for _, test := range tests {
		if got := SanitizePath(test.in); got != test.want {
			t.Errorf("SanitizePath(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	// Levels arrive from an INI file or an environment variable, so surrounding
	// whitespace and arbitrary casing have to be tolerated.
	tests := []struct {
		in   string
		want slog.Level
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "INFO", want: slog.LevelInfo},
		{in: "", want: slog.LevelInfo},
		{in: " warn ", want: slog.LevelWarn},
		{in: "warning", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
	}
	for _, test := range tests {
		got, err := ParseLevel(test.in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", test.in, err)
			continue
		}
		if got != test.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", test.in, got, test.want)
		}
	}

	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("an unsupported level was accepted")
	}
}

func TestNewWritesToAFileAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "aegisheet.log")

	// Two successive runs must accumulate rather than truncate: the log file is the
	// audit trail of scheduled runs.
	for _, message := range []string{"first run", "second run"} {
		logger, closer, err := New(Options{Level: "info", Format: "json", Output: path})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		logger.Info(message)
		if err := closer.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	for _, message := range []string{"first run", "second run"} {
		if !strings.Contains(string(data), message) {
			t.Errorf("the log file is missing %q: %s", message, data)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != LogFileMode {
		t.Errorf("log file mode = 0%o, want 0%o", perm, LogFileMode)
	}
}

func TestNewRejectsUnsupportedFormat(t *testing.T) {
	if _, _, err := New(Options{Level: "info", Format: "xml"}); err == nil {
		t.Error("an unsupported log format was accepted")
	}
}
