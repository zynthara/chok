package main

import (
	"bufio"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/v2/version"
)

//go:embed templates/*
var templateFS embed.FS

type projectData struct {
	Name        string // project name, e.g. "myapp"
	Module      string // Go module path, same as Name
	NameUpper   string // e.g. "MYAPP" (env prefix)
	ChokVersion string // e.g. "v2.0.0-beta.1", or "v2.0.0-dev" for local
	ChokReplace string // local path for replace directive (empty if published)
	SigningKey  string // generated dev-only account signing key
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init [name]",
		Short: "Scaffold a new chok v2 project",
		Long: `Scaffold a new chok v2 project.

The scaffold is the v2 wiring model in miniature: chok.yaml declares
the modules, chok_modules_gen.go (generated here by the same engine
as ` + "`chok sync`" + `) assembles them, main.go holds your models and
routes. The project boots immediately:

    cd <name> && go mod tidy && go run .

When [name] is provided it is treated as the destination directory;
the project name is the directory's basename. When omitted, the
current working directory is used and its basename becomes the
project name.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runInit,
	}
}

func runInit(cmd *cobra.Command, args []string) error {
	var dir, name string

	if len(args) > 0 {
		// args[0] is the target directory; the project *name* (env-var
		// prefix, module identifier, binary name) is the basename.
		dir = args[0]
		name = filepath.Base(filepath.Clean(dir))
		if name == "." || name == ".." || name == string(filepath.Separator) || name == "" {
			return fmt.Errorf("invalid project name derived from %q (use a directory whose basename is a valid identifier)", args[0])
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = wd
		name = filepath.Base(wd)
	}

	if !isDirEmpty(dir) {
		fmt.Fprintf(cmd.OutOrStdout(), "Directory %s is not empty. Continue? [y/N] ", dir)
		reader := bufio.NewReader(cmd.InOrStdin())
		answer, _ := reader.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			return fmt.Errorf("cancelled")
		}
	}

	localPath := detectChokLocalPath()
	chokVer := detectChokVersion()
	if chokVer == "dev" {
		// go.mod needs valid semver; the replace directive (when a
		// local checkout was found) is what actually resolves it.
		chokVer = "v2.0.0-dev"
	}

	data := projectData{
		Name:        name,
		Module:      name,
		NameUpper:   strings.ToUpper(strings.ReplaceAll(name, "-", "_")),
		ChokVersion: chokVer,
		ChokReplace: localPath,
		SigningKey:  newSigningKey(),
	}

	files := []struct {
		tmpl string
		out  string
	}{
		{"templates/chok.yaml.tmpl", "chok.yaml"},
		{"templates/main.go.tmpl", "main.go"},
		{"templates/gomod.tmpl", "go.mod"},
		{"templates/Makefile.tmpl", "Makefile"},
		{"templates/gitignore.tmpl", ".gitignore"},
		{"templates/migrations-readme.tmpl", "migrations/README.md"},
	}
	for _, f := range files {
		if err := renderTemplate(dir, f.tmpl, f.out, data); err != nil {
			return fmt.Errorf("render %s: %w", f.out, err)
		}
	}

	// chok_modules_gen.go comes from the sync engine itself — init and
	// sync can never drift because they are the same code path.
	if err := runSync(cmd, filepath.Join(dir, "chok.yaml"), dir, false); err != nil {
		return fmt.Errorf("generate %s: %w", genFileName, err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\nProject %s created:\n\n", name)
	printTree(cmd, dir, name)

	fmt.Fprintln(cmd.OutOrStdout(), "\nNext steps:")
	if len(args) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "  cd %s\n", dir)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "  go mod tidy")
	fmt.Fprintln(cmd.OutOrStdout(), "  go run .")
	fmt.Fprintf(cmd.OutOrStdout(), "\nThen: curl localhost:8080/healthz — and http://localhost:8080/swagger for the API docs.\n")
	fmt.Fprintf(cmd.OutOrStdout(), "Edit chok.yaml to add modules; `chok sync` (or make sync) refreshes the assembly.\n\n")

	return nil
}

// newSigningKey generates the dev-only account signing key baked into
// the scaffold yaml (64 hex chars ≥ the 32-byte minimum).
func newSigningKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failing means a broken platform RNG; a fixed and
		// clearly-marked dev key beats aborting the scaffold.
		return "dev-only-signing-key-change-me-0000000000000000"
	}
	return hex.EncodeToString(b)
}

func renderTemplate(baseDir, tmplPath, outPath string, data projectData) error {
	content, err := templateFS.ReadFile(tmplPath)
	if err != nil {
		return err
	}
	t, err := template.New(filepath.Base(tmplPath)).Parse(string(content))
	if err != nil {
		return err
	}
	fullPath := filepath.Join(baseDir, outPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return t.Execute(f, data)
}

// chokModule is the module line of a chok v2 checkout — the only
// thing a scaffold can use as a local `replace` target.
const chokModule = "module github.com/zynthara/chok/v2"

func detectChokVersion() string {
	v := version.Get().Version
	if v == "" || v == "dev" || v == "unknown" {
		return "dev"
	}
	return v
}

func detectChokLocalPath() string {
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			dir := filepath.Dir(exe)
			for {
				if isChokRoot(dir) {
					return dir
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
		}
	}

	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			gopath = filepath.Join(home, "go")
		}
	}
	if gopath != "" {
		candidates := []string{
			filepath.Join(gopath, "src", "chok"),
			filepath.Join(gopath, "src", "github.com", "zynthara", "chok"),
		}
		for _, c := range candidates {
			if isChokRoot(c) {
				return c
			}
		}
	}
	return ""
}

func isChokRoot(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == chokModule {
			return true
		}
	}
	return false
}

func isDirEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	return len(entries) == 0
}

func printTree(cmd *cobra.Command, dir, name string) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "  %s/\n", name)
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		depth := strings.Count(rel, string(os.PathSeparator))
		indent := strings.Repeat("  ", depth)
		if d.IsDir() {
			fmt.Fprintf(out, "  %s├── %s/\n", indent, d.Name())
		} else {
			fmt.Fprintf(out, "  %s├── %s\n", indent, d.Name())
		}
		return nil
	})
}
