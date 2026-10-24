package main

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/zynthara/chok/v3/internal/conf"
)

//go:embed templates
var templates embed.FS

// releaseVersion is the chok release this source tree prepares. chok init
// writes it into go.mod when the running binary is not a release itself
// (a build of a working tree reports no usable version); bump it with
// every release.
const releaseVersion = "v3.0.0-alpha.1"

// scaffold is what the templates see.
type scaffold struct {
	Name        string // the project's directory, binary and app name
	Module      string // the go.mod module path
	EnvPrefix   string // the environment prefix chok.Load derives from Name
	ChokVersion string // the chok version go.mod requires
}

// scaffoldFiles maps each template to the file it renders.
var scaffoldFiles = []struct{ tmpl, out string }{
	{"go.mod.tmpl", "go.mod"},
	{"main.go.tmpl", "main.go"},
	{"chok.yaml.tmpl", "chok.yaml"},
	{"Makefile.tmpl", "Makefile"},
	{"gitignore.tmpl", ".gitignore"},
	{"migrations-readme.md.tmpl", "migrations/README.md"},
}

var projectName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func newInitCmd() *cobra.Command {
	var module string
	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Create a chok project in the directory <name>",
		Long: `Create a chok project in the new directory <name>: a main.go with
one resource and its routes, chok.yaml, a Makefile, migrations/ and a
go.mod that requires chok and names this command as a tool. Then:

    cd <name> && go mod tidy && go run .

<name> is also the binary's name and the application's: chok.Load reads
<NAME>_* environment variables. --module sets the module path when it
differs from the name, such as github.com/acme/<name>.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, fallback := scaffoldVersion(currentBuild())
			if fallback {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"chok init: this chok command is not a release build (%s); the project requires chok %s\n",
					currentBuild().Version, v)
			}
			return runInit(cmd.OutOrStdout(), args[0], module, v)
		},
	}
	cmd.Flags().StringVar(&module, "module", "", "module path of the project (default <name>)")
	return cmd
}

// scaffoldVersion is the chok version a new project requires: the
// running command's own, when it is a release or a pseudo-version a
// proxy can serve, else releaseVersion (fallback true).
func scaffoldVersion(b build) (v string, fallback bool) {
	if strings.HasPrefix(b.Version, "v3.") && !strings.Contains(b.Version, "+") {
		return b.Version, false
	}
	return releaseVersion, true
}

func runInit(out io.Writer, name, module, chokVersion string) error {
	if !projectName.MatchString(name) {
		return fmt.Errorf("init: %q is not a project name: start with a letter, then letters, digits, - or _", name)
	}
	if module == "" {
		module = name
	}
	if entries, err := os.ReadDir(name); err == nil && len(entries) > 0 {
		return fmt.Errorf("init: %s exists and is not empty", name)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("init: %w", err)
	}
	data := scaffold{Name: name, Module: module, EnvPrefix: conf.Prefix(name), ChokVersion: chokVersion}
	for _, f := range scaffoldFiles {
		if err := render(filepath.Join(name, f.out), "templates/"+f.tmpl, data); err != nil {
			return fmt.Errorf("init: %s: %w", f.out, err)
		}
	}
	_, err := fmt.Fprintf(out, `created %[1]s/ (module %[2]s, chok %[3]s):

  %[1]s/main.go               the app: configuration, database, routes
  %[1]s/chok.yaml             its configuration
  %[1]s/Makefile              run, build, test, migrate-status, migrate-up
  %[1]s/migrations/README.md  versioned migrations, for migrate: versioned
  %[1]s/go.mod                requires chok and names go tool chok
  %[1]s/.gitignore

next:

  cd %[1]s
  go mod tidy
  go run .

then curl localhost:8080/healthz, and open http://localhost:8080/docs.
`, name, module, chokVersion)
	return err
}

func render(path, tmpl string, data scaffold) error {
	src, err := templates.ReadFile(tmpl)
	if err != nil {
		return err
	}
	t, err := template.New(filepath.Base(tmpl)).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := t.Execute(f, data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
