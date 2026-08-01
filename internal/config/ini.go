package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

// Common INI keys understood in every report section.
const (
	keyTask      = "task"
	keyEnabled   = "enabled"
	keySheet     = "sheet"
	keyMaxRows   = "maxrows"
	keyWhitelist = "whitelist"
)

// dateOnlyLayout is accepted in addition to RFC 3339 for audit window bounds,
// because operators usually think in whole days.
const dateOnlyLayout = "2006-01-02"

// loadINIFile reads and parses the configuration file.
//
// Keys are parsed case-insensitively so that userKey, userkey and USERKEY all
// resolve; earlier versions of this tool read the same value under two different
// spellings in two different files, and one of them always lost. Section names
// keep their case, because a section name is the worksheet title.
func loadINIFile(path string) (*ini.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("no configuration file configured: pass -config")
	}

	// Stat first: go-ini's own error for a missing file is less actionable than a
	// message that names the flag used to fix it.
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("configuration file %s does not exist (pass -config to point at it)", path)
		}
		return nil, fmt.Errorf("configuration file %s is not readable: %w", path, err)
	}

	file, err := ini.LoadSources(ini.LoadOptions{InsensitiveKeys: true}, path)
	if err != nil {
		return nil, fmt.Errorf("parse configuration file %s: %w", path, err)
	}
	return file, nil
}

// applyINIGlobals layers the [global] section over the defaults.
func applyINIGlobals(cfg *Config, file *ini.File, table []setting) error {
	section := file.Section(GlobalSection)

	var errs []error
	known := map[string]struct{}{
		// Consumed by report sections rather than by a global setting.
		keyWhitelist: {},
	}

	for _, s := range table {
		if s.INI == "" {
			continue
		}
		key := strings.ToLower(s.INI)
		known[key] = struct{}{}

		if !section.HasKey(key) {
			continue
		}
		raw := strings.TrimSpace(section.Key(key).String())
		if raw == "" {
			continue
		}
		if isPlaceholder(raw) {
			// The shipped example uses <path_to_credentials> style placeholders.
			// Treating them as unset produces "no credentials configured", which
			// tells the operator what to do, instead of "open <path_to_...>: no
			// such file".
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[%s] %s is still set to the example "+
				"placeholder %s and was ignored", GlobalSection, s.INI, raw))
			continue
		}
		if err := s.Set(cfg, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: [%s] %s: %w", cfg.ConfigPath, GlobalSection, s.INI, err))
		}
	}

	// Unknown keys are almost always typos; naming them saves a debugging round.
	for _, key := range section.KeyStrings() {
		if _, ok := known[strings.ToLower(key)]; !ok {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[%s] key %q is not recognised (typo?)",
				GlobalSection, key))
		}
	}

	return errors.Join(errs...)
}

// parseTaskSections turns every non-reserved INI section into a TaskSpec.
//
// Behaviour differs deliberately from earlier versions in two ways:
//
//   - A section without a task= key is skipped with a warning. Previously it
//     silently defaulted to the 2FA report, so a typo in the key name produced a
//     plausible-looking sheet containing the wrong data.
//   - An unrecognised task name is a hard error listing the valid names, for the
//     same reason.
func parseTaskSections(file *ini.File) ([]TaskSpec, []string, error) {
	var (
		specs    []TaskSpec
		warnings []string
		errs     []error
	)

	// Whitelist fallbacks: the current location is [global], the legacy one is the
	// [users] section used by the original tool.
	globalWhitelist := readList(file.Section(GlobalSection), keyWhitelist)
	legacyWhitelist := readList(file.Section(legacyUsersSection), keyWhitelist)
	if len(legacyWhitelist) > 0 {
		warnings = append(warnings, fmt.Sprintf("[%s] whitelist is deprecated; move it into [%s] "+
			"or into the report section that needs it", legacyUsersSection, GlobalSection))
	}

	for _, section := range file.Sections() {
		name := section.Name()

		// Reserved sections never describe a report.
		if name == ini.DefaultSection ||
			strings.EqualFold(name, GlobalSection) ||
			strings.EqualFold(name, legacyUsersSection) {
			continue
		}

		if !section.HasKey(keyTask) {
			warnings = append(warnings, fmt.Sprintf("section %q has no %s= key and was skipped", name, keyTask))
			continue
		}
		rawTask := strings.TrimSpace(section.Key(keyTask).String())
		id, err := ParseTaskID(rawTask)
		if err != nil {
			errs = append(errs, fmt.Errorf("section %q: %w", name, err))
			continue
		}

		spec, specErrs := buildTaskSpec(section, name, id, rawTask, globalWhitelist, legacyWhitelist)
		if len(specErrs) > 0 {
			errs = append(errs, specErrs...)
			continue
		}
		specs = append(specs, spec)
	}

	if len(errs) > 0 {
		return nil, warnings, errors.Join(errs...)
	}
	return specs, warnings, nil
}

// buildTaskSpec reads one report section into a TaskSpec.
func buildTaskSpec(
	section *ini.Section,
	name string,
	id TaskID,
	rawTask string,
	globalWhitelist, legacyWhitelist []string,
) (TaskSpec, []error) {
	var errs []error

	spec := TaskSpec{
		Section: name,
		Sheet:   name, // the section name is the sheet title unless overridden
		ID:      id,
		RawName: rawTask,
		Enabled: true,
	}

	if value := readString(section, keySheet); value != "" {
		spec.Sheet = value
	}
	if section.HasKey(keyEnabled) {
		if err := setBool(section.Key(keyEnabled).String(), &spec.Enabled); err != nil {
			errs = append(errs, fmt.Errorf("section %q: %s: %w", name, keyEnabled, err))
		}
	}
	if value := readString(section, keyMaxRows); value != "" {
		if err := setInt(value, &spec.MaxRows); err != nil {
			errs = append(errs, fmt.Errorf("section %q: %s: %w", name, keyMaxRows, err))
		}
	}

	// Task parameters. Reading them for every task keeps the code simple; keys
	// that the selected task does not use are reported as warnings via Extra.
	spec.UserKey = readString(section, "userkey")
	spec.AppName = readString(section, "appname")
	spec.EventName = readString(section, "eventname")

	if value := readString(section, "starttime"); value != "" {
		parsed, err := parseTimestamp(value)
		if err != nil {
			errs = append(errs, fmt.Errorf("section %q: startTime: %w", name, err))
		} else {
			spec.StartTime = parsed
		}
	}
	if value := readString(section, "endtime"); value != "" {
		parsed, err := parseTimestamp(value)
		if err != nil {
			errs = append(errs, fmt.Errorf("section %q: endTime: %w", name, err))
		} else {
			spec.EndTime = parsed
		}
	}
	if value := readString(section, "lookbackdays"); value != "" {
		if err := setInt(value, &spec.LookbackDays); err != nil {
			errs = append(errs, fmt.Errorf("section %q: lookbackDays: %w", name, err))
		}
	}

	// The audit task must name an application; default to the one the legacy
	// task name ("GetAuditLogins") implied.
	if spec.ID == TaskAuditActivities && spec.AppName == "" {
		spec.AppName = DefaultAuditApplication
	}

	// Whitelist resolution: most specific wins.
	switch {
	case section.HasKey(keyWhitelist):
		spec.Whitelist = NewStringSet(readList(section, keyWhitelist))
	case len(globalWhitelist) > 0:
		spec.Whitelist = NewStringSet(globalWhitelist)
	default:
		spec.Whitelist = NewStringSet(legacyWhitelist)
	}

	spec.Extra = unusedKeys(section, spec.ID)

	return spec, errs
}

// unusedKeys returns the section's keys that the selected task does not consume.
func unusedKeys(section *ini.Section, id TaskID) map[string]string {
	known := map[string]struct{}{
		keyTask:      {},
		keyEnabled:   {},
		keySheet:     {},
		keyMaxRows:   {},
		keyWhitelist: {},
	}
	if def, ok := LookupTask(id); ok {
		for _, key := range def.Keys {
			known[strings.ToLower(key)] = struct{}{}
		}
	}

	var extra map[string]string
	for _, key := range section.KeyStrings() {
		if _, ok := known[strings.ToLower(key)]; ok {
			continue
		}
		if extra == nil {
			extra = make(map[string]string)
		}
		extra[key] = section.Key(key).String()
	}
	return extra
}

// readString returns a trimmed key value, treating example placeholders as unset.
func readString(section *ini.Section, key string) string {
	if section == nil || !section.HasKey(key) {
		return ""
	}
	value := strings.TrimSpace(section.Key(key).String())
	if isPlaceholder(value) {
		return ""
	}
	return value
}

// readList splits a comma separated key value.
func readList(section *ini.Section, key string) []string {
	value := readString(section, key)
	if value == "" {
		return nil
	}
	return splitList(value)
}

// isPlaceholder recognises the <angle_bracket> placeholders used in the example
// configuration, so that an unedited example produces a configuration error
// rather than a filesystem error.
func isPlaceholder(value string) bool {
	return strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">")
}

// parseTimestamp accepts RFC 3339 ("2024-01-31T00:00:00Z") or a plain date
// ("2024-01-31", interpreted as midnight UTC).
func parseTimestamp(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	if parsed, err := time.Parse(dateOnlyLayout, value); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q (want 2006-01-02 or RFC 3339)", value)
}

// SectionNames returns the report section names in file order. Used by tests and
// diagnostics.
func (c *Config) SectionNames() []string {
	names := make([]string, 0, len(c.Tasks))
	for _, task := range c.Tasks {
		names = append(names, task.Section)
	}
	return names
}

// SortedWarnings returns the warnings in a stable order for deterministic output.
func (c *Config) SortedWarnings() []string {
	out := append([]string(nil), c.Warnings...)
	sort.Strings(out)
	return out
}
