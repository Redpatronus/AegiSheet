package tasks

import (
	"strconv"
	"strings"
	"time"

	reports "google.golang.org/api/admin/reports/v1"
)

// timestampLayout is the format used for every timestamp written into a report.
//
// Timestamps are written as text in UTC rather than as native Excel dates on
// purpose: an Excel serial date carries no time zone, so a reader in another
// office would silently see different values, and audit evidence has to mean the
// same thing everywhere. The layout sorts correctly as a string.
const timestampLayout = "2006-01-02 15:04:05"

// epochSentinelYear marks "never happened". The Admin SDK reports the Unix epoch
// for accounts that have never signed in, and printing 1970 as a login date has
// misled more than one reader.
const epochSentinelYear = 1971

// formatAPITime renders an RFC 3339 timestamp string from the API.
//
// Unparseable input yields an empty cell rather than an error: one malformed
// timestamp must not cost the entire report, and an empty cell is unambiguous.
func formatAPITime(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		// Some fields carry sub-second precision.
		parsed, err = time.Parse(time.RFC3339Nano, trimmed)
		if err != nil {
			return ""
		}
	}
	return formatTime(parsed)
}

// formatEpochMillis renders a Unix millisecond timestamp, as used by the
// application specific password endpoints.
func formatEpochMillis(millis int64) string {
	if millis <= 0 {
		return ""
	}
	return formatTime(time.UnixMilli(millis).UTC())
}

// formatTime renders a parsed timestamp, mapping epoch sentinels to an empty cell.
func formatTime(t time.Time) string {
	if t.IsZero() || t.UTC().Year() < epochSentinelYear {
		return ""
	}
	return t.UTC().Format(timestampLayout)
}

// joinValues renders a string slice into one cell.
func joinValues(values []string) string {
	return strings.Join(values, ", ")
}

// formatParameter renders an audit event parameter value.
//
// A parameter carries its value in exactly one of several typed fields, and the
// JSON encoding omits zero values. An absent integer and an integer that is
// genuinely zero are therefore indistinguishable on the wire, as are an absent
// boolean and false. Rather than inventing a value, the ambiguous cases render as
// an empty cell; every field that can be detected reliably is preferred first.
func formatParameter(parameter *reports.ActivityEventsParameters) string {
	if parameter == nil {
		return ""
	}

	switch {
	case parameter.Value != "":
		return parameter.Value
	case len(parameter.MultiValue) > 0:
		return joinValues(parameter.MultiValue)
	case len(parameter.MultiIntValue) > 0:
		parts := make([]string, 0, len(parameter.MultiIntValue))
		for _, value := range parameter.MultiIntValue {
			parts = append(parts, strconv.FormatInt(value, 10))
		}
		return joinValues(parts)
	case parameter.MessageValue != nil && len(parameter.MessageValue.Parameter) > 0:
		return formatNestedParameters(parameter.MessageValue.Parameter)
	case len(parameter.MultiMessageValue) > 0:
		parts := make([]string, 0, len(parameter.MultiMessageValue))
		for _, message := range parameter.MultiMessageValue {
			if message == nil || len(message.Parameter) == 0 {
				continue
			}
			parts = append(parts, "{"+formatNestedParameters(message.Parameter)+"}")
		}
		return joinValues(parts)
	case parameter.IntValue != 0:
		return strconv.FormatInt(parameter.IntValue, 10)
	case parameter.BoolValue:
		return "true"
	default:
		return ""
	}
}

// formatNestedParameters flattens a nested parameter list into "name=value" pairs.
func formatNestedParameters(nested []*reports.NestedParameter) string {
	parts := make([]string, 0, len(nested))
	for _, parameter := range nested {
		if parameter == nil {
			continue
		}
		parts = append(parts, parameter.Name+"="+formatNestedValue(parameter))
	}
	return strings.Join(parts, "; ")
}

// formatNestedValue applies the same precedence rules one level down.
func formatNestedValue(parameter *reports.NestedParameter) string {
	switch {
	case parameter.Value != "":
		return parameter.Value
	case len(parameter.MultiValue) > 0:
		return joinValues(parameter.MultiValue)
	case len(parameter.MultiIntValue) > 0:
		parts := make([]string, 0, len(parameter.MultiIntValue))
		for _, value := range parameter.MultiIntValue {
			parts = append(parts, strconv.FormatInt(value, 10))
		}
		return joinValues(parts)
	case parameter.IntValue != 0:
		return strconv.FormatInt(parameter.IntValue, 10)
	case parameter.BoolValue:
		return "true"
	default:
		return ""
	}
}
