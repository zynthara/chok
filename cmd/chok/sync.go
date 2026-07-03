package main

import (
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/zynthara/chok/v2/internal/blessed"
	"github.com/zynthara/chok/v2/kernel"
)

// chok sync — the reconciliation point between "config drives
// behaviour" and "explicit imports prune the binary" (SPEC §7.2):
// reads chok.yaml, emits chok_modules_gen.go with the chok.Use(...)
// assembly derived from the sections present. yaml `enabled` stays
// the runtime switch; presence of the section is the link-time one.

const genFileName = "chok_modules_gen.go"

func syncCmd() *cobra.Command {
	var (
		configPath string
		dir        string
		check      bool
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Generate chok_modules_gen.go from chok.yaml",
		Long: `Generate (or refresh) chok_modules_gen.go from chok.yaml.

Every module section present in the yaml becomes one entry in the
generated chok.Use(...) list — including sections with enabled: false
(assembly is link-time intent, enabled is the runtime switch). Under
account.providers, every provider with enabled: true gets its
registration emitted. The file is regenerated wholesale and is
byte-stable: rerunning sync without config changes is a no-op.

Customization (db.WithTables, extra middleware, ...) belongs in your
main.go via chok.Override(...), never in the generated file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSync(cmd, configPath, dir, check)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "chok.yaml", "path to the project's chok.yaml")
	cmd.Flags().StringVar(&dir, "dir", ".", "package directory to write "+genFileName+" into")
	cmd.Flags().BoolVar(&check, "check", false, "verify the generated file is current; non-zero exit on drift (CI gate)")
	return cmd
}

func runSync(cmd *cobra.Command, configPath, dir string, check bool) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("sync: read %s: %w", configPath, err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("sync: parse %s: %w", configPath, err)
	}

	plan, warnings := buildSyncPlan(tree)
	for _, w := range warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "sync: warning:", w)
	}

	pkg, err := detectPackageName(dir)
	if err != nil {
		return err
	}
	src, err := renderModulesFile(pkg, configPath, plan)
	if err != nil {
		return err
	}

	outPath := filepath.Join(dir, genFileName)
	existing, readErr := os.ReadFile(outPath)
	if check {
		if readErr != nil {
			return fmt.Errorf("sync --check: %s missing — run `chok sync`", outPath)
		}
		if string(existing) != string(src) {
			return fmt.Errorf("sync --check: %s is stale — run `chok sync`", outPath)
		}
		fmt.Fprintln(cmd.OutOrStdout(), outPath, "up to date")
		return nil
	}
	if readErr == nil && string(existing) == string(src) {
		fmt.Fprintln(cmd.OutOrStdout(), outPath, "unchanged")
		return nil
	}
	if err := os.WriteFile(outPath, src, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), outPath, "written")
	return nil
}

// syncEntry is one line of the generated Use list.
type syncEntry struct {
	imports []string // package import paths this entry needs
	expr    string   // the constructor expression
}

// syncPlan is the resolved assembly.
type syncPlan struct {
	entries []syncEntry
}

// buildSyncPlan maps yaml sections to blessed modules in canonical
// order. Unknown top-level keys are business sections — ignored by
// design (chok.Section[T] owns them). Warnings cover the cases where
// yaml expresses intent sync cannot link: unknown provider names and
// hard module dependencies whose section is absent.
func buildSyncPlan(tree map[string]any) (syncPlan, []string) {
	present := make(map[string]map[string]any, len(tree))
	for k, v := range tree {
		sub, _ := v.(map[string]any)
		present[strings.ToLower(k)] = sub
	}

	var plan syncPlan
	var warnings []string
	included := make(map[string]bool)

	for _, m := range blessed.Modules() {
		section := kernel.SectionKeyOf(m.New().Describe())
		if _, ok := present[section]; !ok {
			continue
		}
		included[m.New().Describe().Kind] = true

		switch section {
		case "account":
			expr, imports, warn := accountEntry(m, present[section])
			warnings = append(warnings, warn...)
			plan.entries = append(plan.entries, syncEntry{imports: imports, expr: expr})
		default:
			plan.entries = append(plan.entries, syncEntry{imports: []string{m.ImportPath}, expr: m.Constructor})
			if m.MultiInstance {
				for _, name := range instanceNames(present[section]) {
					plan.entries = append(plan.entries, syncEntry{
						imports: []string{m.ImportPath},
						expr:    fmt.Sprintf("%s.Module(%s.As(%q))", m.Pkg, m.Pkg, name),
					})
				}
			}
		}
	}

	// Hard-dependency lint: the kernel is the enforcer, but telling
	// the operator at generation time beats a startup failure later.
	for _, m := range blessed.Modules() {
		d := m.New().Describe()
		if _, ok := present[kernel.SectionKeyOf(d)]; !ok {
			continue
		}
		for _, dep := range d.Needs {
			if !dep.Optional && !included[dep.Kind] {
				warnings = append(warnings, fmt.Sprintf(
					"%s requires the %q module, but no %q section is in the yaml — the app will fail at startup",
					d.Kind, dep.Kind, sectionForKind(dep.Kind)))
			}
		}
	}
	return plan, warnings
}

// sectionForKind resolves a Dep.Kind back to its yaml section key.
func sectionForKind(kind string) string {
	for _, m := range blessed.Modules() {
		d := m.New().Describe()
		if d.Kind == kind {
			return kernel.SectionKeyOf(d)
		}
	}
	return kind
}

// accountEntry renders account.Module(), attaching WithProviders for
// every yaml provider with an explicit enabled: true.
func accountEntry(m blessed.Module, section map[string]any) (expr string, imports []string, warnings []string) {
	imports = []string{m.ImportPath}
	providers, _ := section["providers"].(map[string]any)

	var names []string
	for name := range providers {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)

	var specs []string
	for _, name := range names {
		cfg, _ := providers[name].(map[string]any)
		if on, _ := cfg["enabled"].(bool); !on {
			continue // enabled defaults to false for providers (explicit opt-in)
		}
		p, ok := blessed.ProviderByName(name)
		if !ok {
			warnings = append(warnings, fmt.Sprintf(
				"account.providers.%s is not a blessed provider — register it yourself via chok.Override(account.Module(account.WithProviders(...)))", name))
			continue
		}
		imports = append(imports, p.ImportPath)
		specs = append(specs, fmt.Sprintf("%s.Provider()", p.Pkg))
	}
	if len(specs) == 0 {
		return m.Constructor, imports, warnings
	}
	return fmt.Sprintf("account.Module(account.WithProviders(%s))", strings.Join(specs, ", ")), imports, warnings
}

// instanceNames lists <section>.instances.* keys, sorted.
func instanceNames(section map[string]any) []string {
	instances, _ := section["instances"].(map[string]any)
	names := make([]string, 0, len(instances))
	for name := range instances {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// renderModulesFile emits the generated file (gofmt-formatted).
func renderModulesFile(pkg, configPath string, plan syncPlan) ([]byte, error) {
	seen := make(map[string]bool)
	var imports []string
	for _, e := range plan.entries {
		for _, im := range e.imports {
			if !seen[im] {
				seen[im] = true
				imports = append(imports, im)
			}
		}
	}
	sort.Strings(imports)

	var b strings.Builder
	b.WriteString("// Code generated by chok sync; DO NOT EDIT.\n")
	fmt.Fprintf(&b, "// Source: %s — rerun `chok sync` after adding or removing module sections.\n", filepath.Base(configPath))
	b.WriteString("//\n// Customize modules in your own code with chok.Override(...); this\n")
	b.WriteString("// file always mirrors the yaml.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString("import (\n")
	b.WriteString("\t\"github.com/zynthara/chok/v2\"\n\n")
	for _, im := range imports {
		fmt.Fprintf(&b, "\t%q\n", im)
	}
	b.WriteString(")\n\n")
	b.WriteString("// chokModules is the assembly derived from chok.yaml: one module per\n")
	b.WriteString("// section present. yaml `enabled` remains the runtime switch.\n")
	b.WriteString("func chokModules() chok.Option {\n")
	b.WriteString("\treturn chok.Use(\n")
	for _, e := range plan.entries {
		fmt.Fprintf(&b, "\t\t%s,\n", e.expr)
	}
	b.WriteString("\t)\n}\n")

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("sync: format generated code: %w", err)
	}
	return src, nil
}

// detectPackageName parses the target directory's Go files for the
// package clause; a fresh (or Go-less) directory defaults to main.
func detectPackageName(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("sync: read dir %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == genFileName {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.PackageClauseOnly)
		if err != nil {
			continue // unparsable file: let the next one decide
		}
		return f.Name.Name, nil
	}
	return "main", nil
}
