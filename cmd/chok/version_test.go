package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// setStamp sets the variables a release build stamps with -ldflags for
// the length of the test.
func setStamp(t *testing.T, v, c, d string) {
	t.Helper()
	ov, oc, od := version, commit, date
	version, commit, date = v, c, d
	t.Cleanup(func() { version, commit, date = ov, oc, od })
}

// run executes the chok command with args and returns what it printed on
// stdout; its stderr goes to the test log.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	if errOut.Len() > 0 {
		t.Logf("chok %s, stderr:\n%s", strings.Join(args, " "), errOut.String())
	}
	return out.String(), err
}

// TestVersion_Shape: chok version prints one line naming the version, the
// toolchain and the platform; --json the same as an object; --version on
// the root command the same line.
func TestVersion_Shape(t *testing.T) {
	setStamp(t, "", "", "")
	line, err := run(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^chok \S+ \(.*go1\.\d+\S* \w+/\w+\)\n$`).MatchString(line) {
		t.Fatalf("chok version: %q", line)
	}
	if again, _ := run(t, "--version"); again != line {
		t.Fatalf("chok --version %q, chok version %q", again, line)
	}
	out, err := run(t, "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var b build
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("chok version --json: %v\n%s", err, out)
	}
	if b.Version == "" || !strings.HasPrefix(b.Go, "go1.") || !strings.Contains(b.Platform, "/") {
		t.Fatalf("chok version --json: %+v", b)
	}
	if _, err := run(t, "version", "extra"); err == nil {
		t.Fatal("chok version takes no arguments")
	}
}

// TestVersion_FromBuildInfo: without a release stamp the version is the
// chok module's as Go recorded it — the main module's for go install and
// a build in a checkout, the dependency's (or its replacement's) for go
// tool chok inside an application — with the VCS revision and time.
func TestVersion_FromBuildInfo(t *testing.T) {
	setStamp(t, "", "", "")
	vcs := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123"},
		{Key: "vcs.time", Value: "2026-10-25T08:00:00Z"},
		{Key: "vcs.modified", Value: "false"},
	}
	cases := []struct {
		name string
		bi   *debug.BuildInfo
		want string
	}{
		{"go install", &debug.BuildInfo{Main: debug.Module{Path: chokModule, Version: "v0.2.0"}}, "v0.2.0"},
		{"working tree", &debug.BuildInfo{Main: debug.Module{Path: chokModule, Version: "(devel)"}, Settings: vcs}, "(devel)"},
		{"go tool chok", &debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}, Deps: []*debug.Module{
			{Path: "github.com/spf13/cobra", Version: "v1.10.2"},
			{Path: chokModule, Version: "v0.2.0"},
		}}, "v0.2.0"},
		{"replaced", &debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}, Deps: []*debug.Module{
			{Path: chokModule, Version: "v0.2.0", Replace: &debug.Module{Path: "../chok"}},
		}}, "(devel)"},
		{"not linked", &debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}}, "(devel)"},
	}
	for _, c := range cases {
		if got := buildOf(c.bi, true).Version; got != c.want {
			t.Errorf("%s: version %q, want %q", c.name, got, c.want)
		}
	}
	b := buildOf(&debug.BuildInfo{Main: debug.Module{Path: chokModule, Version: "(devel)"}, Settings: vcs}, true)
	if b.Commit != "0123456789abcdef0123" || b.Date != "2026-10-25T08:00:00Z" || b.Modified {
		t.Fatalf("vcs stamp: %+v", b)
	}
	if got := b.String(); !strings.HasPrefix(got, "chok (devel) (commit 0123456789ab, 2026-10-25T08:00:00Z, go1.") {
		t.Fatalf("line %q", got)
	}
	if got := buildOf(nil, false).Version; got != "(devel)" {
		t.Fatalf("no build information: %q", got)
	}
}

// TestVersion_ReleaseStampWins: the version, commit and date a release
// build stamps replace what Go recorded, a tag without its v gets one, and
// the release does not report the runner's working tree as modified.
func TestVersion_ReleaseStampWins(t *testing.T) {
	setStamp(t, "0.2.0", "fedcba9876543210", "2026-10-26T00:00:00Z")
	bi := &debug.BuildInfo{Main: debug.Module{Path: chokModule, Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.modified", Value: "true"},
	}}
	b := buildOf(bi, true)
	if b.Version != "v0.2.0" || b.Commit != "fedcba9876543210" || b.Date != "2026-10-26T00:00:00Z" || b.Modified {
		t.Fatalf("release build: %+v", b)
	}
	if strings.Contains(b.String(), "modified") {
		t.Fatalf("a release line never says modified: %s", b.String())
	}
	setStamp(t, "v0.3.0", "", "")
	if got := buildOf(bi, true).Version; got != "v0.3.0" {
		t.Fatalf("a tag with its v stays as is: %q", got)
	}
}

// TestVersion_ModifiedMarkedOnce: a source build with uncommitted changes
// says so once, next to the revision, and a version Go already marked
// +dirty is printed as recorded rather than marked again.
func TestVersion_ModifiedMarkedOnce(t *testing.T) {
	setStamp(t, "", "", "")
	bi := &debug.BuildInfo{
		Main: debug.Module{Path: chokModule, Version: "v0.2.1-0.20261025080000-0123456789ab+dirty"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	line := buildOf(bi, true).String()
	if strings.Count(line, "dirty") != 1 || strings.Count(line, "modified") != 1 {
		t.Fatalf("modified marked more than once: %s", line)
	}
	if !strings.HasPrefix(line, "chok v0.2.1-0.20261025080000-0123456789ab+dirty (commit 0123456789ab, modified, go1.") {
		t.Fatalf("line %q", line)
	}
}
