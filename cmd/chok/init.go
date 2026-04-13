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

	"github.com/zynthara/chok/version"
)

//go:embed templates/*
var templateFS embed.FS

type projectData struct {
	Name         string // project name, e.g. "myapp"
	Module       string // Go module path, same as Name
	NameUpper    string // e.g. "MYAPP" (for env prefix docs)
	ChokVersion  string // e.g. "v0.1.0" or "v0.0.0-dev" for local
	ChokReplace  string // local path for replace directive (empty if published)
}

func runInit(args []string) error {
	var dir, name string

	if len(args) > 0 {
		name = args[0]
		dir = name
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
		fmt.Printf("目录 %s 不为空，是否继续？[y/N] ", dir)
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
		Name:         name,
		Module:       name,
		NameUpper:    strings.ToUpper(strings.ReplaceAll(name, "-", "_")),
		ChokVersion:  chokVer,
		ChokReplace:  localPath,
	}

	// Template → output path mapping.
	// Follows miniblog convention: cmd/{name}/, internal/, configs/.
	files := []struct {
		tmpl string
		out  string
	}{
		// cmd
		{"templates/main.go.tmpl", "cmd/" + name + "/main.go"},
		// internal
		{"templates/config.go.tmpl", "internal/app/config.go"},
		{"templates/server.go.tmpl", "internal/app/server.go"},
		{"templates/handler.go.tmpl", "internal/handler/handler.go"},
		// configs
		{"templates/app.yaml.tmpl", "configs/" + name + ".yaml"},
		// root
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

	fmt.Printf("\n项目 %s 已创建：\n\n", name)
	printTree(dir, name)

	fmt.Println("\n开始使用：")
	if len(args) > 0 {
		fmt.Printf("  cd %s\n", name)
	}
	fmt.Println("  go mod tidy")
	fmt.Printf("  make run\n")
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

// detectChokVersion returns the chok module version to use in generated go.mod.
// Uses the build-time injected version. Falls back to "dev" for local builds.
func detectChokVersion() string {
	v := version.Get().Version
	if v == "" || v == "dev" || v == "unknown" {
		return "dev"
	}
	return v
}

// detectChokLocalPath finds the local chok source directory.
// 1. Walk up from the binary location (works for `go build`/`go run` from source tree).
// 2. Check GOPATH/src/ common locations (works for `go install ./cmd/chok/`).
// Returns "" only if chok is truly installed from the module proxy (published version).
func detectChokLocalPath() string {
	// Method 1: walk up from binary.
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

	// Method 2: check GOPATH/src/ common locations.
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
