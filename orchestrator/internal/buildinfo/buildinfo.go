// Package buildinfo exposes version information injected at build time.
package buildinfo

import "runtime/debug"

// Set with -ldflags "-X github.com/0xyu/aco/internal/buildinfo.Version=..." etc.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info returns version, commit, and build date, filling commit/date from the
// Go VCS stamp when they were not injected.
func Info() (version, commit, date string) {
	version, commit, date = Version, Commit, Date
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "" {
					commit = s.Value
					if len(commit) > 12 {
						commit = commit[:12]
					}
				}
			case "vcs.time":
				if date == "" {
					date = s.Value
				}
			}
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	if date == "" {
		date = "unknown"
	}
	return version, commit, date
}
