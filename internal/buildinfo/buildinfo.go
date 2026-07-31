// Package buildinfo exposes the identity of the running binary.
//
// Every log line and every generated report is stamped with this information so
// that an operator holding only a spreadsheet or a log excerpt can tell exactly
// which build produced it. That traceability is the reason this tiny package
// exists as a package rather than as a handful of variables in main.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// AppName is the canonical program name. It is used for the flag set name, the
// environment variable prefix and the outbound HTTP User-Agent.
const AppName = "aegisheet"

// The following values are intended to be injected at link time, e.g.
//
//	go build -ldflags "-X github.com/Redpatronus/AegiSheet/internal/buildinfo.version=1.4.0 \
//	                   -X github.com/Redpatronus/AegiSheet/internal/buildinfo.commit=$(git rev-parse --short HEAD) \
//	                   -X github.com/Redpatronus/AegiSheet/internal/buildinfo.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// When they are absent (a plain `go build`, or `go install module@version`) the
// values are recovered from the Go module metadata embedded by the toolchain, so
// the binary is never completely anonymous.
var (
	version = ""
	commit  = ""
	date    = ""
)

// Info is a snapshot of the build identity, resolved once per call.
type Info struct {
	Name      string // program name
	Version   string // semantic version, module version, or "dev"
	Commit    string // VCS revision, short form where available
	BuildDate string // RFC3339 build timestamp, or "unknown"
	GoVersion string // toolchain that compiled the binary
	Platform  string // GOOS/GOARCH
}

// Get resolves the build identity, preferring link-time values and falling back
// to the metadata that the Go toolchain stamps into the binary.
func Get() Info {
	info := Info{
		Name:      AppName,
		Version:   version,
		Commit:    commit,
		BuildDate: date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	// debug.ReadBuildInfo fails only for binaries built without module support,
	// which the project no longer supports; treat it as best-effort enrichment.
	if bi, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = shortCommit(setting.Value)
				}
			case "vcs.time":
				if info.BuildDate == "" {
					info.BuildDate = setting.Value
				}
			case "vcs.modified":
				// A dirty working tree must be visible in audit output: a report
				// produced from uncommitted code is not reproducible.
				if setting.Value == "true" && info.Commit != "" {
					info.Commit += "-dirty"
				}
			}
		}
	}

	// Normalise the remaining gaps so downstream formatting never prints "".
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.BuildDate == "" {
		info.BuildDate = "unknown"
	}
	return info
}

// String renders a single-line human readable build banner.
func (i Info) String() string {
	return fmt.Sprintf("%s %s (commit %s, built %s, %s, %s)",
		i.Name, i.Version, i.Commit, i.BuildDate, i.GoVersion, i.Platform)
}

// UserAgent renders the value sent in the HTTP User-Agent header. Google Cloud
// support can correlate server-side quota events with a specific client build
// from this string, so it intentionally carries the version.
func (i Info) UserAgent() string {
	return fmt.Sprintf("%s/%s (+https://github.com/Redpatronus/AegiSheet)", i.Name, i.Version)
}

// shortCommit trims a full git SHA to the conventional 12-character form.
func shortCommit(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
