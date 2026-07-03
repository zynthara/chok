package conf

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func valueOf(v any) reflect.Value { return reflect.ValueOf(v) }

// --- fixtures ---------------------------------------------------------

type webOpts struct {
	Addr    string        `mapstructure:"addr"    default:":8080" reload:"restart"`
	Timeout time.Duration `mapstructure:"timeout" default:"5s"    reload:"hot"`
}

type logOpts struct {
	Level  string   `mapstructure:"level"  default:"info" reload:"hot"`
	Output []string `mapstructure:"output"                reload:"restart"`
}

type validOpts struct {
	Port int `mapstructure:"port"`
	Rate int `mapstructure:"rate"`
}

func (o *validOpts) Validate() error {
	var errs []error
	if o.Port <= 0 {
		errs = append(errs, errors.New("port must be positive"))
	}
	if o.Rate <= 0 {
		errs = append(errs, errors.New("rate must be positive"))
	}
	return errors.Join(errs...)
}

// discriminator fixture: only the selected branch may be validated.
type branchOpts struct {
	Kind string  `mapstructure:"kind"`
	A    aBranch `mapstructure:"a"`
	B    bBranch `mapstructure:"b"`
}

func (o *branchOpts) Validate() error {
	switch o.Kind {
	case "a":
		return o.A.Validate()
	case "b":
		return o.B.Validate()
	default:
		return errors.New("kind must be a or b")
	}
}
func (o *branchOpts) IsSelfValidating() {}

type aBranch struct {
	Val string `mapstructure:"val"`
}

func (b *aBranch) Validate() error {
	if b.Val == "" {
		return errors.New("a.val required")
	}
	return nil
}

type bBranch struct {
	Num int `mapstructure:"num"`
}

func (b *bBranch) Validate() error {
	if b.Num <= 0 {
		return errors.New("b.num required")
	}
	return nil
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoader(t *testing.T, sections map[string]any) *Loader {
	t.Helper()
	l := NewLoader("testapp", "TESTAPP")
	for k, v := range sections {
		if err := l.Register(k, v); err != nil {
			t.Fatalf("register %s: %v", k, err)
		}
	}
	return l
}

// --- source precedence (ports config_test.go DefaultTag / EnvOverride /
// FileOverridesDefault) -------------------------------------------------

func TestLoad_DefaultTag(t *testing.T) {
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	snap, _, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	var w webOpts
	if err := snap.Section("web", &w); err != nil {
		t.Fatal(err)
	}
	if w.Addr != ":8080" || w.Timeout != 5*time.Second {
		t.Fatalf("defaults not applied: %+v", w)
	}
}

func TestLoad_FileOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", "web:\n  addr: \":9090\"\n")
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	l.SetPath(p)
	snap, path, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if path != p {
		t.Fatalf("resolved path = %q, want %q", path, p)
	}
	var w webOpts
	if err := snap.Section("web", &w); err != nil {
		t.Fatal(err)
	}
	if w.Addr != ":9090" {
		t.Fatalf("file should beat default: %+v", w)
	}
	if w.Timeout != 5*time.Second {
		t.Fatalf("untouched field keeps default: %+v", w)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", "web:\n  addr: \":9090\"\n  timeout: 7s\n")
	t.Setenv("TESTAPP_WEB_ADDR", ":7070")
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	l.SetPath(p)
	snap, _, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	var w webOpts
	if err := snap.Section("web", &w); err != nil {
		t.Fatal(err)
	}
	if w.Addr != ":7070" {
		t.Fatalf("env should beat file: %+v", w)
	}
	if w.Timeout != 7*time.Second {
		t.Fatalf("file value survives for un-overridden field: %+v", w)
	}
}

func TestLoad_ExplicitPathNotFound_Error(t *testing.T) {
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	l.SetPath(filepath.Join(t.TempDir(), "missing.yaml"))
	if _, _, err := l.Load(); err == nil {
		t.Fatal("explicit missing config must error")
	}
}

func TestLoad_DefaultPathNotFound_Skip(t *testing.T) {
	t.Chdir(t.TempDir()) // nothing to auto-detect here
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	snap, path, err := l.Load()
	if err != nil {
		t.Fatalf("default lookup miss must not error: %v", err)
	}
	if path != "" {
		t.Fatalf("no file should have been read, got %q", path)
	}
	var w webOpts
	if err := snap.Section("web", &w); err != nil {
		t.Fatal(err)
	}
	if w.Addr != ":8080" {
		t.Fatalf("defaults still apply without a file: %+v", w)
	}
}

func TestLoad_DefaultPathDetection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "testapp.yaml", "web:\n  addr: \":6060\"\n")
	t.Chdir(dir)
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	snap, path, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "testapp.yaml" {
		t.Fatalf("expected ./testapp.yaml detection, got %q", path)
	}
	var w webOpts
	_ = snap.Section("web", &w)
	if w.Addr != ":6060" {
		t.Fatalf("detected file not loaded: %+v", w)
	}
}

func TestLoad_ConfigsSubdirFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "configs"), "testapp.yaml", "web:\n  addr: \":6161\"\n")
	t.Chdir(dir)
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	snap, _, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	var w webOpts
	_ = snap.Section("web", &w)
	if w.Addr != ":6161" {
		t.Fatalf("configs/ fallback not loaded: %+v", w)
	}
}

// Ports TestConfig_PrefixEnvBootstrap: {PREFIX}_CONFIG picks the file
// and a missing file at that explicit-by-env path errors.
func TestLoad_PrefixEnvConfigPath(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "custom.yaml", "web:\n  addr: \":5050\"\n")
	t.Setenv("TESTAPP_CONFIG", p)
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	snap, path, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if path != p {
		t.Fatalf("path = %q, want %q", path, p)
	}
	var w webOpts
	_ = snap.Section("web", &w)
	if w.Addr != ":5050" {
		t.Fatalf("env-pointed file not loaded: %+v", w)
	}

	t.Setenv("TESTAPP_CONFIG", filepath.Join(dir, "gone.yaml"))
	if _, _, err := l.Load(); err == nil {
		t.Fatal("missing {PREFIX}_CONFIG file must error")
	}
}

// Ports TestConfig_ProviderEnvOverridesYAML: dynamic map keys (only
// discoverable from yaml) still receive env overrides.
func TestLoad_DynamicMapKeyEnvOverride(t *testing.T) {
	type provRaw struct {
		Providers map[string]map[string]any `mapstructure:"providers"`
	}
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", `
account:
  providers:
    google:
      client_secret: "from-yaml"
      client_id: "id-1"
`)
	t.Setenv("TESTAPP_ACCOUNT_PROVIDERS_GOOGLE_CLIENT_SECRET", "from-env")
	l := mustLoader(t, map[string]any{"account": provRaw{}})
	l.SetPath(p)
	snap, _, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	var a provRaw
	if err := snap.Section("account", &a); err != nil {
		t.Fatal(err)
	}
	g := a.Providers["google"]
	if g["client_secret"] != "from-env" {
		t.Fatalf("env must override yaml map leaf: %v", g)
	}
	if g["client_id"] != "id-1" {
		t.Fatalf("untouched map leaf keeps yaml value: %v", g)
	}
}

// --- validation (ports ValidateCollectsAllErrors / RootValidatable /
// DiscriminatorSkipsBranchRecursion) -------------------------------------

func TestLoad_ValidateCollectsAllErrors(t *testing.T) {
	l := mustLoader(t, map[string]any{"v": validOpts{}})
	_, _, err := l.Load()
	if err == nil {
		t.Fatal("want validation failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "port must be positive") || !strings.Contains(msg, "rate must be positive") {
		t.Fatalf("all errors must be collected, got: %v", err)
	}
}

func TestLoad_DiscriminatorSkipsBranchRecursion(t *testing.T) {
	dir := t.TempDir()
	// kind=a with a valid A branch; B branch left empty — recursing
	// into B would produce a spurious "b.num required".
	p := writeFile(t, dir, "app.yaml", "d:\n  kind: a\n  a:\n    val: x\n")
	l := mustLoader(t, map[string]any{"d": branchOpts{}})
	l.SetPath(p)
	if _, _, err := l.Load(); err != nil {
		t.Fatalf("unselected branch must not be validated: %v", err)
	}
}

func TestRegister_ReservedInstancesField(t *testing.T) {
	type bad struct {
		Instances map[string]string `mapstructure:"instances"`
	}
	l := NewLoader("x", "X")
	if err := l.Register("db", bad{}); err == nil {
		t.Fatal("claiming the reserved instances key must fail")
	}
}

func TestRegister_DuplicateKey(t *testing.T) {
	l := NewLoader("x", "X")
	if err := l.Register("web", webOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Register("web", webOpts{}); err != nil {
		t.Fatalf("same-type re-register is idempotent: %v", err)
	}
	if err := l.Register("web", logOpts{}); err == nil {
		t.Fatal("conflicting type for same key must fail")
	}
}

// Mini-SPEC §1: named instances address <key>.instances.<name> and env
// vars map through the nested path.
func TestLoad_NamedInstanceSection(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", `
web:
  addr: ":1111"
  instances:
    admin:
      addr: ":2222"
`)
	t.Setenv("TESTAPP_WEB_INSTANCES_ADMIN_TIMEOUT", "9s")
	l := mustLoader(t, map[string]any{
		"web":                 webOpts{},
		"web.instances.admin": webOpts{},
	})
	l.SetPath(p)
	snap, _, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	var def, admin webOpts
	if err := snap.Section("web", &def); err != nil {
		t.Fatal(err)
	}
	if err := snap.Section("web.instances.admin", &admin); err != nil {
		t.Fatal(err)
	}
	if def.Addr != ":1111" || admin.Addr != ":2222" {
		t.Fatalf("instance sections mixed up: def=%+v admin=%+v", def, admin)
	}
	if admin.Timeout != 9*time.Second {
		t.Fatalf("nested-instance env mapping failed: %+v", admin)
	}
}

// --- snapshot semantics -------------------------------------------------

func TestSnapshot_EnabledFor(t *testing.T) {
	type sw struct {
		Enabled bool `mapstructure:"enabled" default:"true"`
	}
	type swOff struct {
		Enabled bool `mapstructure:"enabled" default:"false"`
	}
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", "b:\n  enabled: false\n")
	l := mustLoader(t, map[string]any{"a": sw{}, "b": sw{}, "c": swOff{}, "nosec": webOpts{}})
	l.SetPath(p)
	snap, _, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.EnabledFor("a") {
		t.Fatal("default:true section must be enabled")
	}
	if snap.EnabledFor("b") {
		t.Fatal("yaml enabled:false must disable")
	}
	if snap.EnabledFor("c") {
		t.Fatal("default:false module must be disabled without yaml")
	}
	if !snap.EnabledFor("nosec") {
		t.Fatal("section without enabled key defaults to enabled")
	}
	if !snap.EnabledFor("") {
		t.Fatal("empty ConfigKey is always enabled")
	}
}

func TestSnapshot_SectionUnregisteredAdHoc(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", "biz:\n  name: hello\n")
	l := mustLoader(t, map[string]any{"web": webOpts{}})
	l.SetPath(p)
	snap, _, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Name string `mapstructure:"name"`
	}
	if err := snap.Section("biz", &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "hello" {
		t.Fatalf("ad-hoc decode failed: %+v", out)
	}
}

// --- reload / RCU (ports Immutable_PreservesOldOnValidationFailure) -----

func TestStore_Reload_SwapAndDiff(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", "log:\n  level: info\n  output: [stdout]\n")
	l := mustLoader(t, map[string]any{"log": logOpts{}})
	l.SetPath(p)
	st, err := NewStore(l)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, dir, "app.yaml", "log:\n  level: debug\n  output: [stdout]\n")
	diff, err := st.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Changed {
		t.Fatal("tree change must be detected")
	}
	sc := diff.Sections["log"]
	if len(sc.Hot) != 1 || sc.Hot[0] != "log.level" {
		t.Fatalf("level is reload:\"hot\": %+v", sc)
	}
	if len(sc.Restart) != 0 {
		t.Fatalf("no restart fields changed: %+v", sc)
	}
	var lo logOpts
	if err := st.Snapshot().Section("log", &lo); err != nil {
		t.Fatal(err)
	}
	if lo.Level != "debug" {
		t.Fatalf("new snapshot must carry the new value: %+v", lo)
	}
}

func TestStore_Reload_FailurePreservesOldSnapshot(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", "v:\n  port: 1\n  rate: 2\n")
	l := mustLoader(t, map[string]any{"v": validOpts{}})
	l.SetPath(p)
	st, err := NewStore(l)
	if err != nil {
		t.Fatal(err)
	}
	before := st.Snapshot()

	writeFile(t, dir, "app.yaml", "v:\n  port: 0\n  rate: 2\n") // invalid
	if _, err := st.Reload(); err == nil {
		t.Fatal("invalid reload must fail")
	}
	if st.Snapshot() != before {
		t.Fatal("failed reload must not swap the snapshot (zero pollution)")
	}
	var v validOpts
	_ = st.Snapshot().Section("v", &v)
	if v.Port != 1 {
		t.Fatalf("old values must survive: %+v", v)
	}
}

func TestStore_Reload_NoChange(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.yaml", "log:\n  level: info\n")
	l := mustLoader(t, map[string]any{"log": logOpts{}})
	l.SetPath(p)
	st, err := NewStore(l)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := st.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if diff.Changed {
		t.Fatal("identical sources must diff as unchanged")
	}
	if diff.Sections["log"].Changed() {
		t.Fatal("section must be unchanged")
	}
}

// --- tag diff engine ----------------------------------------------------

type diffFixture struct {
	Enabled bool     `mapstructure:"enabled" reload:"hot"` // tag must NOT override the framework rule
	Hot     string   `mapstructure:"hot"     reload:"hot"`
	Cold    string   `mapstructure:"cold"    reload:"restart"`
	Untag   string   `mapstructure:"untag"`
	List    []string `mapstructure:"list"    reload:"hot"`
	Nested  diffNest `mapstructure:"nested"  reload:"hot"`
}

type diffNest struct {
	Inherit  string `mapstructure:"inherit"`                   // inherits hot from Nested
	Override string `mapstructure:"override" reload:"restart"` // overrides to restart
	Enabled  bool   `mapstructure:"enabled"`                   // NON-root enabled: normal rules (inherits hot)
}

func diffOf(t *testing.T, old, fresh diffFixture) SectionChange {
	t.Helper()
	ch := SectionChange{Key: "x"}
	diffStructs(valueOf(old), valueOf(fresh), "x", "", true, &ch)
	return ch
}

func TestDiff_TagClassification(t *testing.T) {
	base := diffFixture{Hot: "a", Cold: "b", Untag: "c", List: []string{"1"}}

	mod := base
	mod.Hot = "A"
	ch := diffOf(t, base, mod)
	if len(ch.Hot) != 1 || ch.Hot[0] != "x.hot" || len(ch.Restart) != 0 {
		t.Fatalf("hot tag: %+v", ch)
	}

	mod = base
	mod.Cold = "B"
	ch = diffOf(t, base, mod)
	if len(ch.Restart) != 1 || ch.Restart[0] != "x.cold" {
		t.Fatalf("restart tag: %+v", ch)
	}

	mod = base
	mod.Untag = "C"
	ch = diffOf(t, base, mod)
	if len(ch.Restart) != 1 || len(ch.Hot) != 0 {
		t.Fatalf("untagged defaults to restart: %+v", ch)
	}
}

func TestDiff_RootEnabledForcedRestart(t *testing.T) {
	base := diffFixture{Enabled: true}
	mod := base
	mod.Enabled = false
	ch := diffOf(t, base, mod)
	if !ch.EnabledFlipped {
		t.Fatal("enabled flip must be flagged")
	}
	if len(ch.Hot) != 0 {
		t.Fatalf("reload:\"hot\" on root enabled must be ignored: %+v", ch)
	}
	if len(ch.Restart) != 1 || ch.Restart[0] != "x.enabled" {
		t.Fatalf("enabled is restart-only: %+v", ch)
	}
}

func TestDiff_NestedInheritanceAndOverride(t *testing.T) {
	base := diffFixture{Nested: diffNest{Inherit: "i", Override: "o", Enabled: false}}

	mod := base
	mod.Nested.Inherit = "I"
	ch := diffOf(t, base, mod)
	if len(ch.Hot) != 1 || ch.Hot[0] != "x.nested.inherit" {
		t.Fatalf("nested field inherits parent hot: %+v", ch)
	}

	mod = base
	mod.Nested.Override = "O"
	ch = diffOf(t, base, mod)
	if len(ch.Restart) != 1 || ch.Restart[0] != "x.nested.override" {
		t.Fatalf("nested override to restart: %+v", ch)
	}

	mod = base
	mod.Nested.Enabled = true
	ch = diffOf(t, base, mod)
	if len(ch.Hot) != 1 || ch.Hot[0] != "x.nested.enabled" || ch.EnabledFlipped {
		t.Fatalf("non-root enabled follows normal tag rules: %+v", ch)
	}
}

func TestDiff_SliceWholeValue(t *testing.T) {
	base := diffFixture{List: []string{"a", "b"}}
	mod := base
	mod.List = []string{"a", "c"}
	ch := diffOf(t, base, mod)
	if len(ch.Hot) != 1 || ch.Hot[0] != "x.list" {
		t.Fatalf("slice diffs as whole value under one tag: %+v", ch)
	}
}
