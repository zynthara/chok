package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// chokModule is the module this command belongs to.
const chokModule = "github.com/zynthara/chok/v3"

// The release build sets these with -ldflags "-X main.version=...", as
// .goreleaser.yaml does; a binary built any other way reads its version
// and VCS stamp from the build information Go embeds.
var (
	version string
	commit  string
	date    string
)

// build describes the binary.
type build struct {
	// Version is the chok module's version: the release tag, a
	// pseudo-version, or "(devel)" for a build of a working tree.
	Version string `json:"version"`
	// Commit and Date are the VCS revision and its commit time, when
	// known; Modified reports uncommitted changes in a source build.
	Commit   string `json:"commit,omitzero"`
	Date     string `json:"date,omitzero"`
	Modified bool   `json:"modified,omitzero"`
	// Go is the toolchain that built the binary, Platform its GOOS/GOARCH.
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

// currentBuild describes this binary.
func currentBuild() build {
	bi, ok := debug.ReadBuildInfo()
	return buildOf(bi, ok)
}

func buildOf(bi *debug.BuildInfo, ok bool) build {
	b := build{Version: "(devel)", Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if ok && bi != nil {
		b.Version = moduleVersion(bi)
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Commit = s.Value
			case "vcs.time":
				b.Date = s.Value
			case "vcs.modified":
				b.Modified = s.Value == "true"
			}
		}
	}
	if version != "" {
		// A release build names its tag; the runner's working tree is not
		// the release's business.
		b.Version, b.Modified = withV(version), false
	}
	if commit != "" {
		b.Commit = commit
	}
	if date != "" {
		b.Date = date
	}
	return b
}

// moduleVersion is the version of the chok module the binary was built
// from: the main module's when chok is the main module (go install
// .../cmd/chok@v3.x.y, go build in a checkout), the dependency's when the
// command is built inside an application (go tool chok).
func moduleVersion(bi *debug.BuildInfo) string {
	if bi.Main.Path == chokModule {
		return orDevel(bi.Main.Version)
	}
	for _, d := range bi.Deps {
		if d.Path != chokModule {
			continue
		}
		if d.Replace != nil {
			return orDevel(d.Replace.Version)
		}
		return orDevel(d.Version)
	}
	return "(devel)"
}

func orDevel(v string) string {
	if v == "" {
		return "(devel)"
	}
	return v
}

var semverish = regexp.MustCompile(`^\d+\.\d+\.\d+`)

// withV adds the v a module version starts with when the release tooling
// dropped it (goreleaser's .Version is the tag without its v).
func withV(v string) string {
	if semverish.MatchString(v) {
		return "v" + v
	}
	return v
}

// String is the one line chok version prints, such as
// "chok v3.0.0-alpha.1 (commit 1a2b3c4d5e6f, 2026-10-25T08:00:00Z, go1.27.3 darwin/arm64)".
func (b build) String() string {
	var parts []string
	if b.Commit != "" {
		c := b.Commit
		if len(c) > 12 {
			c = c[:12]
		}
		if b.Modified {
			c += ", modified"
		}
		parts = append(parts, "commit "+c)
	}
	if b.Date != "" {
		parts = append(parts, b.Date)
	}
	parts = append(parts, b.Go+" "+b.Platform)
	return "chok " + b.Version + " (" + strings.Join(parts, ", ") + ")"
}

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version of this chok command and how it was built",
		Long: `Print the chok module version this command was built from, its VCS
revision and commit time when known, and the Go toolchain and platform.

A release binary carries the tag goreleaser stamped into it; one built by
go install or go tool chok reports the module version Go recorded, and a
build of a working tree reports (devel) with the revision it was built at.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b := currentBuild()
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(b)
			}
			_, err := fmt.Fprintln(out, b.String())
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print a JSON object instead of one line")
	return cmd
}
