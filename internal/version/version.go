// Package version is the version of this build, set by the linker:
//
//	-ldflags "-X github.com/Teamtem-dev/kuben/internal/version.Version=2.0.0-alpha.1"
package version

import "runtime/debug"

// Version is the release version; "dev" for local builds.
var Version = "dev" //nolint:gochecknoglobals // set by the linker, never written at run time

// Commit is the VCS revision the binary was built from, when Go recorded it.
func Commit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
