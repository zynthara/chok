package doccheck

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The documents whose go blocks readers copy whole.
var documents = []string{"docs/quickstart.md", "README.md", "README_zh.md", "docs/realtime.md"}

// appName finds the name a program gives chok.MustLoad, whose upper case
// prefixes its environment variables.
var appName = regexp.MustCompile(`chok\.MustLoad\[\w+(?:\.\w+)?\]\("([\w-]+)"\)`)

// block is one fenced code block.
type block struct {
	doc, section, lang, text string
	line                     int
}

func blocks(t *testing.T, root, doc string) []block {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, doc))
	if err != nil {
		t.Fatal(err)
	}
	var out []block
	var cur *block
	section := ""
	for i, line := range strings.Split(string(raw), "\n") {
		switch {
		case cur != nil && strings.HasPrefix(line, "```"):
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			cur.text += line + "\n"
		case strings.HasPrefix(line, "```"):
			cur = &block{doc: doc, section: section, lang: strings.TrimPrefix(line, "```"), line: i + 1}
		case strings.HasPrefix(line, "## "):
			section = line
		}
	}
	if cur != nil {
		t.Fatalf("%s: the code block at line %d is not closed", doc, cur.line)
	}
	return out
}

// TestDocs_GoProgramsBuild builds every go block of the documents as a
// program of a module that requires this checkout, vets it, and runs it
// with --check-config against the yaml block of the same section, if any.
func TestDocs_GoProgramsBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the documentation's programs with the go command")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mod := t.TempDir()
	type program struct {
		b    block
		dir  string
		yaml string
	}
	var programs []program
	for _, doc := range documents {
		bs := blocks(t, root, doc)
		for i, b := range bs {
			if b.lang != "go" {
				continue
			}
			if !strings.HasPrefix(b.text, "package main\n") {
				t.Errorf("%s:%d: a go block must be a whole program, starting with package main", b.doc, b.line)
				continue
			}
			p := program{b: b, dir: fmt.Sprintf("p%02d", len(programs)+1)}
			for _, y := range bs[i+1:] {
				if y.section != b.section || y.lang == "go" {
					break
				}
				if y.lang == "yaml" {
					p.yaml = y.text
					break
				}
			}
			if err := os.MkdirAll(filepath.Join(mod, p.dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mod, p.dir, "main.go"), []byte(b.text), 0o644); err != nil {
				t.Fatal(err)
			}
			programs = append(programs, p)
		}
	}
	if len(programs) < 8 {
		t.Fatalf("found %d programs; the quickstart has three, each README two and docs/realtime.md one", len(programs))
	}
	gomod := "module example.com/docs\n\ngo 1.27\n\nrequire github.com/zynthara/chok/v3 v3.0.0\n\nreplace github.com/zynthara/chok/v3 => " + root + "\n"
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	gocmd := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("go", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	gocmd(mod, "mod", "tidy")
	gocmd(mod, "vet", "./...")
	gocmd(mod, "build", "-o", "bin"+string(filepath.Separator), "./...")

	for _, p := range programs {
		if p.yaml == "" {
			continue
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "chok.yaml"), []byte(p.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		m := appName.FindStringSubmatch(p.b.text)
		if m == nil {
			t.Errorf("%s:%d: the program has a chok.yaml block but no chok.MustLoad name", p.b.doc, p.b.line)
			continue
		}
		prefix := strings.ToUpper(strings.ReplaceAll(m[1], "-", "_"))
		c := exec.Command(filepath.Join(mod, "bin", p.dir), "--check-config")
		c.Dir = dir
		c.Env = append(os.Environ(), prefix+"_AUTH_SIGNING_KEY=doccheck-signing-key-of-at-least-32-bytes")
		out, err := c.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "configuration is valid (file chok.yaml") {
			t.Errorf("%s:%d (%s): --check-config with the section's chok.yaml: %v\n%s", p.b.doc, p.b.line, p.b.section, err, out)
		}
	}
}
