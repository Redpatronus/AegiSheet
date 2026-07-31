package xlsx

import (
	"strconv"
	"strings"
)

// Worksheet naming limits imposed by the Excel file format. Violating any of
// them makes the workbook unopenable, so names taken from configuration (where
// they are free-form section titles) must be normalised before use.
const (
	// MaxSheetNameLen is the hard limit on worksheet title length.
	MaxSheetNameLen = 31
	// invalidSheetChars cannot appear in a worksheet title.
	invalidSheetChars = `:\/?*[]`
	// fallbackSheetName is used when a name normalises to nothing at all.
	fallbackSheetName = "Sheet"
	// reservedSheetName is reserved by Excel for the change-history sheet.
	reservedSheetName = "History"
	// replacementChar substitutes for a forbidden character.
	replacementChar = '_'
)

// SanitizeSheetName converts an arbitrary configuration section name into a legal
// worksheet title.
//
// Earlier versions passed section names straight through, so a section such as
// [Admin Activity: 2024/Q1] produced a workbook that Excel refused to open, and
// a long section name silently exceeded the 31 character limit. The
// transformation applied here is deliberately lossy but predictable:
//
//	"Admin Activity: 2024/Q1"      -> "Admin Activity_ 2024_Q1"
//	"a very long section name ..."  -> truncated to 31 characters
//	"History"                       -> "History_" (reserved by Excel)
func SanitizeSheetName(name string) string {
	trimmed := strings.TrimSpace(name)

	var b strings.Builder
	b.Grow(len(trimmed))
	for _, r := range trimmed {
		if strings.ContainsRune(invalidSheetChars, r) {
			b.WriteRune(replacementChar)
			continue
		}
		b.WriteRune(r)
	}
	cleaned := b.String()

	// Excel rejects titles that begin or end with an apostrophe.
	cleaned = strings.Trim(cleaned, "'")

	// Truncate on rune boundaries: the limit is 31 characters, not bytes, and
	// cutting a multi-byte rune in half would corrupt the XML.
	if runes := []rune(cleaned); len(runes) > MaxSheetNameLen {
		cleaned = strings.TrimSpace(string(runes[:MaxSheetNameLen]))
	}

	if cleaned == "" {
		return fallbackSheetName
	}
	if strings.EqualFold(cleaned, reservedSheetName) {
		return reservedSheetName + string(replacementChar)
	}
	return cleaned
}

// uniqueSheetName returns a name that is not yet present in used, appending a
// "~N" discriminator when necessary and keeping the result within the length
// limit.
//
// Duplicate names arise legitimately: two sections whose titles differ only past
// character 31, or only in characters that sanitisation replaces, collapse onto
// the same title. Excel would reject the second sheet outright.
func uniqueSheetName(used map[string]struct{}, name string) string {
	if _, taken := used[strings.ToLower(name)]; !taken {
		return name
	}
	for n := 2; ; n++ {
		suffix := "~" + strconv.Itoa(n)
		base := name
		if runes := []rune(base); len(runes)+len(suffix) > MaxSheetNameLen {
			base = string(runes[:MaxSheetNameLen-len(suffix)])
		}
		candidate := base + suffix
		if _, taken := used[strings.ToLower(candidate)]; !taken {
			return candidate
		}
	}
}
