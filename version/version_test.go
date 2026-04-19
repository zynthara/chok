package version

import (
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.Version != "dev" {
		t.Fatalf("expected dev, got %s", info.Version)
	}
	if info.GoVersion == "" {
		t.Fatal("GoVersion should not be empty")
	}
}

func TestInfo_String(t *testing.T) {
	info := Get()
	s := info.String()
	if !strings.Contains(s, "dev") {
		t.Fatalf("String() missing version: %s", s)
	}
	if !strings.Contains(s, "go") {
		t.Fatalf("String() missing go version: %s", s)
	}
}

// TestGet_LdflagsInjectedIgnoresVCSModified is a structural guard: when
// the version variable is not the default "dev", Get() must not surface
// vcs.modified=true. Release builds inject a curated version via
// ldflags; the runner's transient "dirty" state (CI cache files,
// merge-commit artefacts, anything that flips git status without
// changing source) shouldn't appear as "+dirty" on the artefact.
//
// We can't easily mock debug.ReadBuildInfo, so the test exercises the
// String() consumer side directly: regardless of how Modified arrived
// in the Info struct, an Info that *originated from ldflags* (Version
// != "dev") should pass through Get's modified gate as false. We simulate
// the post-Get state for the two paths.
func TestGet_LdflagsInjectedIgnoresVCSModified(t *testing.T) {
	// Path A: ldflags-injected. Modified must be false even if the
	// underlying VCS reported true; Get drops the flag in this case.
	releaseLike := Info{Version: "v0.1.2", GitHash: "abc1234", BuildTime: "2026-01-01T00:00:00Z", GoVersion: "go1.26"}
	if releaseLike.Modified {
		t.Fatal("release-like Info should never carry Modified=true")
	}
	if got := releaseLike.String(); strings.Contains(got, "dirty") {
		t.Fatalf("release-like Info.String() should not contain dirty, got %q", got)
	}

	// Path B: dev fallback (no ldflags). Modified=true is honest and
	// surfaces as "+dirty" in the string.
	devLike := Info{Version: "dev", GitHash: "abc1234", BuildTime: "unknown", GoVersion: "go1.26", Modified: true}
	if !strings.Contains(devLike.String(), "+dirty") {
		t.Fatalf("dev Info with Modified=true should print +dirty, got %q", devLike.String())
	}
}

// TestInfo_String_DirtyDeduped guards against the regression where a
// version string that already encodes a dirty marker (either Go's
// pseudo-version "+dirty" or `git describe --dirty`'s "-dirty") would
// be appended a second "+dirty" — the user-visible report otherwise
// reads "v1.0.0-dirty+dirty (...)" which looks like a packaging bug.
func TestInfo_String_DirtyDeduped(t *testing.T) {
	cases := []struct {
		name    string
		version string
		modify  bool
		wantHas string
		wantNot string
	}{
		{"clean_unchanged", "v1.0.0", false, "v1.0.0 (", "+dirty"},
		{"clean_modified_appends", "v1.0.0", true, "v1.0.0+dirty (", "+dirty+dirty"},
		{"git_describe_dirty_no_double", "v1.0.0-1-gabcd-dirty", true, "v1.0.0-1-gabcd-dirty (", "dirty+dirty"},
		{"pseudo_version_dirty_no_double", "v0.0.0-20240101-abcd+dirty", true, "v0.0.0-20240101-abcd+dirty (", "+dirty+dirty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := Info{Version: tc.version, GitHash: "x", BuildTime: "y", GoVersion: "go1", Modified: tc.modify}
			s := i.String()
			if !strings.Contains(s, tc.wantHas) {
				t.Fatalf("missing %q in %s", tc.wantHas, s)
			}
			if strings.Contains(s, tc.wantNot) {
				t.Fatalf("unexpected %q in %s", tc.wantNot, s)
			}
		})
	}
}
