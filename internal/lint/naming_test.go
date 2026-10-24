package lint

import (
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/packages"
)

// The naming rules of the v3 plan (§1 D10, §6), checked on every public
// package of the module, i.e. every package that is neither internal nor
// main:
//
//   - a functional option type is named Option, RouteOption or LoadOption;
//   - a struct carrying yaml tags is named *Config;
//   - no exported top-level function starts with With;
//   - a top-level function returning *T, optionally followed by an error,
//     where T is a struct of the same package, starts with New, MustNew,
//     Open or MustOpen.
const (
	msgOption = "functional option type must be named Option, RouteOption or LoadOption (plan D10)"
	msgConfig = "struct with yaml tags must be named *Config (plan D10)"
	msgWith   = "exported function must not start with With (plan D10)"
	msgCtor   = "constructor must start with New, MustNew, Open or MustOpen (plan §6)"
)

var (
	optionTypeNames     = []string{"Option", "RouteOption", "LoadOption"}
	constructorPrefixes = []string{"New", "MustNew", "Open", "MustOpen"}
)

// contractExemptions are the symbols the frozen v3 contract names against
// a rule on purpose, keyed by package path relative to the module (the
// root package reports as chok). Each entry is an orchestrator decision:
// dropping one means renaming the symbol in a contract revision, and no
// entry is added without one.
var contractExemptions = map[string]string{
	"chok.Base":               "contract §1.1: the standard section every app config embeds",
	"chok.WithPrincipal":      "contract §1.4b: context setter",
	"db.StorePolicy":          "contract §3.2: shared by db.Config and store.Policy",
	"store.Patch":             "contract §3.1: returns *PatchBuilder",
	"store.Set":               "contract §3.1: returns *Changes",
	"store/where.WithTrashed": "contract §3.3: query modifier, verb form kept on purpose",
	"choktest.App":            "contract §8: returns *Client",
}

func TestPublicAPINaming(t *testing.T) {
	pkgs := loadPublic(t, moduleRoot(t))
	t.Logf("checked %d public package(s)", len(pkgs))
	for _, v := range checkNaming(pkgs, contractExemptions) {
		t.Error(v)
	}
}

// TestNamingRules_Fixture keeps the rules honest while the module has no
// public package yet: testdata/naming breaks each rule once, next to the
// compliant shapes and an internal and a main package that must be skipped.
func TestNamingRules_Fixture(t *testing.T) {
	pkgs := loadPublic(t, filepath.Join("testdata", "naming"))
	got := checkNaming(pkgs, map[string]string{"good.WithExempted": "fixture"})
	want := []string{
		"bad.BuildClient: " + msgCtor,
		"bad.FooOption: " + msgOption,
		"bad.MakeClient: " + msgCtor,
		"bad.ServerOpts: " + msgConfig,
		"bad.Setter: " + msgOption,
		"bad.WithAddr: " + msgWith,
		"naming.WithRoot: " + msgWith,
	}
	if !slices.Equal(got, want) {
		t.Errorf("violations:\ngot  %q\nwant %q", got, want)
	}
}

// loadPublic type-checks every package under dir and returns the public
// ones. Load errors fail the test, so broken code cannot pass as clean.
func loadPublic(t *testing.T, dir string) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedModule,
		Dir:  dir,
		Env:  append(os.Environ(), "GOWORK=off"),
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	var public []*packages.Package
	for _, p := range pkgs {
		for _, e := range p.Errors {
			t.Errorf("%s: %v", p.PkgPath, e)
		}
		if p.Name != "main" && !slices.Contains(strings.Split(p.PkgPath, "/"), "internal") {
			public = append(public, p)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return public
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// checkNaming returns the sorted violations as "<pkg>.<Symbol>: <rule>",
// skipping exempted symbols.
func checkNaming(pkgs []*packages.Package, exempt map[string]string) []string {
	var out []string
	for _, p := range pkgs {
		rel := p.Name
		if p.Module != nil && p.PkgPath != p.Module.Path {
			rel = strings.TrimPrefix(p.PkgPath, p.Module.Path+"/")
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			qual := rel + "." + name
			if _, ok := exempt[qual]; ok || !obj.Exported() {
				continue
			}
			for _, msg := range violations(p.Types, obj) {
				out = append(out, qual+": "+msg)
			}
		}
	}
	slices.Sort(out)
	return out
}

func violations(pkg *types.Package, obj types.Object) []string {
	var msgs []string
	switch obj := obj.(type) {
	case *types.TypeName:
		if obj.IsAlias() {
			return nil
		}
		name := obj.Name()
		if (strings.HasSuffix(name, "Option") || isFunctionalOption(pkg, obj.Type())) &&
			!slices.Contains(optionTypeNames, name) {
			msgs = append(msgs, msgOption)
		}
		if hasYAMLTags(obj.Type()) && !strings.HasSuffix(name, "Config") {
			msgs = append(msgs, msgConfig)
		}
	case *types.Func:
		name := obj.Name()
		if hasWithPrefix(name) {
			msgs = append(msgs, msgWith)
		}
		if isConstructor(pkg, obj) && !slices.ContainsFunc(constructorPrefixes, func(p string) bool {
			return strings.HasPrefix(name, p)
		}) {
			msgs = append(msgs, msgCtor)
		}
	}
	return msgs
}

// isFunctionalOption reports the func(*cfg) shape where cfg is an
// unexported struct of the same package — the usual functional option. A
// func over an exported struct (chok.Option is func(*App)) is left to the
// name check, so callbacks such as func(*Server) are not taken for options.
func isFunctionalOption(pkg *types.Package, t types.Type) bool {
	sig, ok := t.Underlying().(*types.Signature)
	if !ok || sig.Params().Len() != 1 || sig.Results().Len() != 0 {
		return false
	}
	ptr, ok := sig.Params().At(0).Type().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(ptr.Elem()).(*types.Named)
	if !ok {
		return false
	}
	_, isStruct := named.Underlying().(*types.Struct)
	obj := named.Origin().Obj()
	return isStruct && obj.Pkg() == pkg && !obj.Exported()
}

func hasYAMLTags(t types.Type) bool {
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range st.NumFields() {
		if _, ok := reflect.StructTag(st.Tag(i)).Lookup("yaml"); ok {
			return true
		}
	}
	return false
}

// hasWithPrefix matches With and WithX, but not words such as Within.
func hasWithPrefix(name string) bool {
	rest, ok := strings.CutPrefix(name, "With")
	if !ok {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || !unicode.IsLower(r)
}

// isConstructor reports a func returning *T, optionally followed by an
// error, where T is a struct declared in the same package.
func isConstructor(pkg *types.Package, fn *types.Func) bool {
	res := fn.Signature().Results()
	switch {
	case res.Len() == 1:
	case res.Len() == 2 && types.Identical(res.At(1).Type(), types.Universe.Lookup("error").Type()):
	default:
		return false
	}
	ptr, ok := res.At(0).Type().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(ptr.Elem()).(*types.Named)
	if !ok {
		return false
	}
	_, isStruct := named.Underlying().(*types.Struct)
	return isStruct && named.Origin().Obj().Pkg() == pkg
}
