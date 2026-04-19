package main

import (
	"bufio"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/version"
)

//go:embed templates/*
var templateFS embed.FS

type projectData struct {
	Name        string // project name, e.g. "myapp"
	Module      string // Go module path, same as Name
	NameUpper   string // e.g. "MYAPP" (for env prefix docs)
	ChokVersion string // e.g. "v0.1.0" or "v0.0.0-dev" for local
	ChokReplace string // local path for replace directive (empty if published)
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init [name]",
		Short: "Scaffold a new chok project",
		Long: `Scaffold a new chok project.

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
		// args[0] is the target directory; the project *name* (used for
		// `cmd/<name>/main.go`, env-var prefix, module identifier) is
		// the basename. Without splitting these, `chok init /tmp/foo`
		// produced `cmd/tmp/foo/main.go` and a project named "foo" with
		// scaffolding under a `tmp` subdir — clearly not the intent.
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
		fmt.Printf("Directory %s is not empty. Continue? [y/N] ", dir)
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(answer)
		if !strings.EqualFold(answer, "y") {
			return fmt.Errorf("cancelled")
		}
	}

	localPath := detectChokLocalPath()
	chokVer := detectChokVersion()
	if localPath != "" && chokVer == "dev" {
		chokVer = "v0.0.0-dev"
	}

	data := projectData{
		Name:        name,
		Module:      name,
		NameUpper:   strings.ToUpper(strings.ReplaceAll(name, "-", "_")),
		ChokVersion: chokVer,
		ChokReplace: localPath,
	}

	// Template → output path mapping.
	files := []struct {
		tmpl string
		out  string
	}{
		{"templates/main.go.tmpl", "cmd/" + name + "/main.go"},
		{"templates/config.go.tmpl", "internal/app/config.go"},
		{"templates/server.go.tmpl", "internal/app/server.go"},
		{"templates/handler.go.tmpl", "internal/handler/handler.go"},
		{"templates/app.yaml.tmpl", "configs/" + name + ".yaml"},
		{"templates/Makefile.tmpl", "Makefile"},
		{"templates/gitignore.tmpl", ".gitignore"},
		{"templates/golangci.yaml.tmpl", ".golangci.yaml"},
		{"templates/go.mod.tmpl", "go.mod"},
	}

	for _, f := range files {
		if err := renderTemplate(dir, f.tmpl, f.out, data); err != nil {
			return fmt.Errorf("render %s: %w", f.out, err)
		}
	}

	fmt.Printf("\nProject %s created:\n\n", name)
	printTree(dir, name)

	fmt.Println("\nNext steps:")
	if len(args) > 0 {
		// `cd` to the actual destination, not the project name —
		// `chok init /tmp/foo` should suggest `cd /tmp/foo`, not `cd foo`.
		fmt.Printf("  cd %s\n", dir)
	}
	fmt.Println("  go mod tidy")
	fmt.Println("  make run")
	fmt.Println()

	return nil
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

const chokModule = "module github.com/zynthara/chok"

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
	return strings.Contains(string(data), chokModule)
}

func isDirEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	return len(entries) == 0
}

func printTree(dir, name string) {
	fmt.Printf("  %s/\n", name)
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		depth := strings.Count(rel, string(os.PathSeparator))
		indent := strings.Repeat("  ", depth)
		if d.IsDir() {
			fmt.Printf("  %s├── %s/\n", indent, d.Name())
		} else {
			fmt.Printf("  %s├── %s\n", indent, d.Name())
		}
		return nil
	})
}
