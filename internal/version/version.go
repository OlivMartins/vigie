// Package version reports the build version of Vigie.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at build time with -ldflags "-X github.com/OlivMartins/vigie/internal/version.version=..."
var (
	version = ""
	commit  = ""
)

// Version returns the release version, or the module version for builds made
// with go install, or "dev".
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// Commit returns the source revision, if known.
func Commit() string {
	if commit != "" {
		return commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return s.Value[:12]
			}
		}
	}
	return ""
}

// String describes the build in one line.
func String() string {
	s := "vigie " + Version()
	if c := Commit(); c != "" {
		s += " (" + c + ")"
	}
	return fmt.Sprintf("%s %s %s/%s", s, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
