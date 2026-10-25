package main

import (
	"go/doc"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestAPIDoc_IsCurrent keeps docs/api.md from rotting: it must be what
// apidoc writes from go doc -short and the source comments.
func TestAPIDoc_IsCurrent(t *testing.T) {
	root := repoRoot(t)
	want, err := render(root)
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(filepath.Join(root, apiDoc))
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != string(want) {
		t.Fatalf("%s is not current: run go run ./internal/tools/apidoc", apiDoc)
	}
}

// TestAPIDoc_MatchesTheBudget: the declarations docs/api.md lists per
// package, and their total, are the exports make budget counts — the
// numbers come from running hack/apibudget.sh itself.
func TestAPIDoc_MatchesTheBudget(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("bash", "hack/apibudget.sh")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hack/apibudget.sh: %v\n%s", err, out)
	}
	budget := map[string]int{}
	row := regexp.MustCompile(`(?m)^exports (\S+)(?: \(provider\))?\s+(\d+)\s`)
	for _, m := range row.FindAllStringSubmatch(string(out), -1) {
		n, _ := strconv.Atoi(m[2])
		budget[m[1]] = n
	}
	if len(budget) < 15 {
		t.Fatalf("too few export rows in the budget output:\n%s", out)
	}

	doc, err := os.ReadFile(filepath.Join(root, apiDoc))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]int{}
	summary := regexp.MustCompile("(?m)^\\| \\[`([^` ]+)(?: \\(provider\\))?`\\]\\(#\\w+\\) \\| (\\d+) \\|$")
	for _, m := range summary.FindAllStringSubmatch(string(doc), -1) {
		n, _ := strconv.Atoi(m[2])
		listed[m[1]] = n
	}
	total := regexp.MustCompile(`(?m)^\| total \| (\d+) \|$`).FindStringSubmatch(string(doc))
	if total == nil {
		t.Fatal("docs/api.md has no total row")
	}
	listed["total"], _ = strconv.Atoi(total[1])
	if len(listed) != len(budget) {
		t.Fatalf("packages: api.md %v, budget %v", listed, budget)
	}
	for p, n := range budget {
		if listed[p] != n {
			t.Errorf("%s: api.md lists %d declarations, make budget counts %d", p, listed[p], n)
		}
	}
	// Each package's table has as many rows as its summary says.
	for _, sec := range strings.Split(string(doc), "\n## ")[1:] {
		name, _, _ := strings.Cut(sec, "\n")
		rows := strings.Count(sec, "\n| `") + strings.Count(sec, "\n| ↳ `")
		if rows != listed[name] {
			t.Errorf("%s: %d rows, summary %d", name, rows, listed[name])
		}
	}
}

// TestSynopsis_ReadsAuthorize: go/doc drops the synopsis of a comment
// that starts with "Author", taking it for a copyright line; the doc of
// realtime.Authorize does, and api.md still describes it.
func TestSynopsis_ReadsAuthorize(t *testing.T) {
	p := &doc.Package{}
	for text, want := range map[string]string{
		"Authorize decides who may subscribe. More follows.": "Authorize decides who may subscribe.",
		"Authorization header aside, it works.":              "Authorization header aside, it works.",
		"Copyright 2026 nobody.\n":                           "Copyright 2026 nobody.",
		"New builds a hub.\n\nDetails.":                      "New builds a hub.",
		"":                                                   "",
	} {
		if got := synopsis(p, text); got != want {
			t.Errorf("synopsis(%q) = %q, want %q", text, got, want)
		}
	}
}
