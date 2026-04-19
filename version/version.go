// Package version exposes build / VCS / runtime metadata.
//
// Two sources are honoured, in order of priority:
//
//  1. Explicit ldflags injection (typical for tagged binaries built
//     by goreleaser / Makefile):
//
//     go build -ldflags "
//       -X github.com/zynthara/chok/version.version=v1.0.0
//       -X github.com/zynthara/chok/version.gitHash=abc1234
//       -X github.com/zynthara/chok/version.buildTime=2024-01-01T00:00:00Z"
//
//  2. Go's debug.ReadBuildInfo (Go 1.18+) — populated automatically
//     for any binary built from a module, including those installed
//     via `go install github.com/.../cmd/chok@latest`. Provides
//     vcs.revision (git hash), vcs.time (build time from commit),
//     vcs.modified (dirty flag), and main.Version (the @-suffix in
//     `go install path@v1.2.3`).
//
// The ldflags path always wins because release builds need an
// explicit, human-curated semver tag. ReadBuildInfo is the fallback
// that makes `go install ...@latest` show meaningful values instead
// of "dev / unknown / unknown".
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// These are overwritten via -ldflags. Defaults survive when neither
// ldflags nor build info are available (e.g. `go test` of a downstream
// package that imports this one).
var (
	version   = "dev"
	gitHash   = "unknown"
	buildTime = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	GitHash   string `json:"git_hash"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	Modified  bool   `json:"modified,omitempty"`
}

// Get returns the active build metadata. Values set via ldflags take
// precedence; missing fields fall back to debug.ReadBuildInfo so that
// `go install ...@latest` and `go run` still produce useful output.
func Get() Info {
	info := Info{
		Version:   version,
		GitHash:   gitHash,
		BuildTime: buildTime,
		GoVersion: runtime.Version(),
	}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}

	// main.Version is set when the binary was built via
	// `go install path@v1.2.3` — surface it when ldflags didn't
	// provide an explicit override.
	if info.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}

	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.GitHash == "unknown" && s.Value != "" {
				info.GitHash = s.Value
				if len(info.GitHash) > 12 {
					info.GitHash = info.GitHash[:12]
				}
			}
		case "vcs.time":
			if info.BuildTime == "unknown" && s.Value != "" {
				info.BuildTime = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				info.Modified = true
			}
		}
	}

	return info
}

// String renders Info in a single line suited to startup logs and
// `chok version` output. Appends "+dirty" when the build came from a
// modified working tree, unless the Version already encodes the
// dirty marker. Both forms are recognised: Go pseudo-versions use
// "+dirty" (e.g. v0.1.1-0.20260419-abcd+dirty) while
// `git describe --dirty` uses "-dirty" (e.g. v0.1.0-75-g39917e8-dirty).
func (i Info) String() string {
	suffix := ""
	if i.Modified && !strings.Contains(i.Version, "dirty") {
		suffix = "+dirty"
	}
	return fmt.Sprintf("%s%s (git: %s, built: %s, %s)",
		i.Version, suffix, i.GitHash, i.BuildTime, i.GoVersion)
}
