package kernel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel/event"
)

// --- test doubles -------------------------------------------------------

type recorder struct {
	mu sync.Mutex
	ev []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, s)
}

func (r *recorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ev...)
}

func (r *recorder) indexOf(s string) int {
	for i, e := range r.events() {
		if e == s {
			return i
		}
	}
	return -1
}

func (r *recorder) has(s string) bool { return r.indexOf(s) >= 0 }

// assertBefore fails unless a appears before b in the recorded stream.
func (r *recorder) assertBefore(t *testing.T, a, b string) {
	t.Helper()
	ia, ib := r.indexOf(a), r.indexOf(b)
	if ia < 0 || ib < 0 {
		t.Fatalf("missing events %q(%d) / %q(%d): %v", a, ia, b, ib, r.events())
	}
	if ia >= ib {
		t.Fatalf("want %q before %q: %v", a, b, r.events())
	}
}

type fake struct {
	d        Descriptor
	rec      *recorder
	initErr  error
	closeErr error
	initHook func(ctx context.Context, k Kernel) error
}

func (f *fake) Describe() Descriptor { return f.d }

func (f *fake) Init(ctx context.Context, k Kernel) error {
	f.rec.add("init:" + KeyOf(f.d).String())
	if f.initHook != nil {
		if err := f.initHook(ctx, k); err != nil {
			return err
		}
	}
	return f.initErr
}

func (f *fake) Close(ctx context.Context) error {
	f.rec.add("close:" + KeyOf(f.d).String())
	return f.closeErr
}

func comp(rec *recorder, kind string, mods ...func(*fake)) *fake {
	f := &fake{d: Descriptor{Kind: kind}, rec: rec}
	for _, m := range mods {
		m(f)
	}
	return f
}

func needs(deps ...Dep) func(*fake) {
	return func(f *fake) { f.d.Needs = deps }
}

func withConfigKey(k string) func(*fake) {
	return func(f *fake) { f.d.ConfigKey = k }
}

type reloaderComp struct {
	fake
	reloadErr error
}

func (c *reloaderComp) Reload(ctx context.Context) error {
	c.rec.add("reload:" + KeyOf(c.d).String())
	return c.reloadErr
}

type mounterComp struct {
	fake
}

func (c *mounterComp) Mount(r Router) error {
	c.rec.add("mount:" + KeyOf(c.d).String())
	return nil
}

type migratorComp struct {
	fake
	migrateErr error
}

func (c *migratorComp) Migrate(context.Context) error {
	c.rec.add("migrate:" + KeyOf(c.d).String())
	return c.migrateErr
}

type serverComp struct {
	fake
	failBeforeReady error
	exitEarly       bool // return nil right after ready
	serveErr        error
	started         chan struct{} // closed when Serve begins (optional)
}

func (c *serverComp) Serve(ctx context.Context, ready func()) error {
	c.rec.add("serve:" + KeyOf(c.d).String())
	if c.started != nil {
		close(c.started)
	}
	if c.failBeforeReady != nil {
		return c.failBeforeReady
	}
	ready()
	if c.exitEarly {
		return c.serveErr
	}
	<-ctx.Done()
	c.rec.add("serve-exit:" + KeyOf(c.d).String())
	return c.serveErr
}

type drainerComp struct {
	fake
	drainHook func(ctx context.Context)
}

func (c *drainerComp) Drain(ctx context.Context) {
	c.rec.add("drain:" + KeyOf(c.d).String())
	if c.drainHook != nil {
		c.drainHook(ctx)
	}
}

type healtherComp struct {
	fake
	healthErr error
}

func (c *healtherComp) Health(ctx context.Context) error { return c.healthErr }

type providerComp struct {
	fake
	router Router
}

func (c *providerComp) ProvideRouter() Router { return c.router }

type routerDouble struct {
	rec *recorder
}

func (r *routerDouble) Handle(method, pattern string, h http.Handler, mw ...Middleware) {
	r.rec.add("handle:" + method + ":" + pattern)
}

func (r *routerDouble) Group(prefix string, mw ...Middleware) Router { return r }

// --- fixtures -------------------------------------------------------------

func mkStore(t *testing.T, yaml string, sections map[string]any) (*conf.Store, string) {
	t.Helper()
	l := conf.NewLoader("ktest", "KTEST")
	path := ""
	if yaml != "" {
		dir := t.TempDir()
		path = filepath.Join(dir, "ktest.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		l.SetPath(path)
	}
	for k, v := range sections {
		if err := l.Register(k, v); err != nil {
			t.Fatal(err)
		}
	}
	st, err := conf.NewStore(l)
	if err != nil {
		t.Fatal(err)
	}
	return st, path
}

func mkRegistry(t *testing.T, cfg Config) *Registry {
	t.Helper()
	if cfg.Store == nil {
		cfg.Store, _ = mkStore(t, "", nil)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func startOK(t *testing.T, r *Registry) {
	t.Helper()
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
}

// --- topology / lifecycle (ports registry_test.go themes) -----------------

func TestStart_TopoOrder_InitAndReverseClose(t *testing.T) {
	rec := &recorder{}
	a := comp(rec, "a")
	b := comp(rec, "b", needs(Dep{Kind: "a"}))
	c := comp(rec, "c", needs(Dep{Kind: "b"}))
	r := mkRegistry(t, Config{Components: []Component{c, b, a}}) // assembly order ≠ topo order

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.assertBefore(t, "init:a", "init:b")
	rec.assertBefore(t, "init:b", "init:c")

	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.assertBefore(t, "close:c", "close:b")
	rec.assertBefore(t, "close:b", "close:a")
}

func TestStart_DependencyCycle_Fails(t *testing.T) {
	rec := &recorder{}
	a := comp(rec, "a", needs(Dep{Kind: "b"}))
	b := comp(rec, "b", needs(Dep{Kind: "a"}))
	r := mkRegistry(t, Config{Components: []Component{a, b}})
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestStart_MissingHardDep_Fails(t *testing.T) {
	rec := &recorder{}
	b := comp(rec, "b", needs(Dep{Kind: "ghost"}))
	r := mkRegistry(t, Config{Components: []Component{b}})
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("want missing-dep error, got %v", err)
	}
}

func TestStart_InitFailure_RollsBackReverse(t *testing.T) {
	rec := &recorder{}
	a := comp(rec, "a")
	boom := errors.New("boom")
	b := comp(rec, "b", needs(Dep{Kind: "a"}))
	b.initErr = boom
	r := mkRegistry(t, Config{Components: []Component{a, b}})

	err := r.Start(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if !rec.has("close:a") {
		t.Fatalf("initialized component must be rolled back: %v", rec.events())
	}
	if rec.has("close:b") {
		t.Fatalf("failed component must not be closed: %v", rec.events())
	}
}

func TestStart_MigrateFailure_ClosesInitializedComponent(t *testing.T) {
	rec := &recorder{}
	boom := errors.New("migration boom")
	c := &migratorComp{
		fake:       fake{d: Descriptor{Kind: "db"}, rec: rec},
		migrateErr: boom,
	}
	r := mkRegistry(t, Config{Components: []Component{c}})

	err := r.Start(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want migration boom, got %v", err)
	}
	if !rec.has("close:db") {
		t.Fatalf("component whose Init succeeded must close after Migrate failure: %v", rec.events())
	}
	rec.assertBefore(t, "migrate:db", "close:db")
}

func TestStart_OptionalInitFailure_Degrades(t *testing.T) {
	rec := &recorder{}
	a := comp(rec, "a")
	b := comp(rec, "b")
	b.d.Optional = true
	b.initErr = errors.New("optional boom")
	r := mkRegistry(t, Config{Components: []Component{a, b}})
	startOK(t, r)

	if _, ok := r.Lookup("b"); ok {
		t.Fatal("degraded component must not be Lookup-able")
	}
	if _, ok := r.Lookup("a"); !ok {
		t.Fatal("healthy peer must be available")
	}
	var st *ComponentStatus
	for _, s := range r.Components() {
		if s.Key.Kind == "b" {
			st = &s
			break
		}
	}
	if st == nil || st.State != StateDegraded {
		t.Fatalf("component b must report degraded: %+v", st)
	}
	if rep := r.Health(context.Background()); rep.Status != HealthDegraded {
		t.Fatalf("aggregate health must be degraded: %+v", rep)
	}
}

func TestNew_DuplicateKey_FailFast(t *testing.T) {
	rec := &recorder{}
	st, _ := mkStore(t, "", nil)
	_, err := New(Config{Store: st, Components: []Component{comp(rec, "db"), comp(rec, "db")}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestStart_SingleUse(t *testing.T) {
	rec := &recorder{}
	r := mkRegistry(t, Config{Components: []Component{comp(rec, "a")}})
	startOK(t, r)
	if err := r.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("want ErrAlreadyStarted, got %v", err)
	}
}

func TestStop_BeforeStart_Safe_AndIdempotent(t *testing.T) {
	rec := &recorder{}
	r := mkRegistry(t, Config{Components: []Component{comp(rec, "a")}})
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("stop before start must be safe: %v", err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("stop must be idempotent: %v", err)
	}
}

// --- disabled semantics (SPEC §3.1, four definitions) ----------------------

type switchOpts struct {
	Enabled bool `mapstructure:"enabled" default:"true"`
}

func TestDisabled_HardDepOnDisabled_FailsWithDiagnosis(t *testing.T) {
	rec := &recorder{}
	st, _ := mkStore(t, "a:\n  enabled: false\n", map[string]any{"a": switchOpts{}})
	a := comp(rec, "a", withConfigKey("a"))
	b := comp(rec, "b", needs(Dep{Kind: "a"}))
	r := mkRegistry(t, Config{Store: st, Components: []Component{a, b}})

	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disabled by config") {
		t.Fatalf("want 'disabled by config' diagnosis, got %v", err)
	}
}

func TestDisabled_SoftDepOnDisabled_Degrades(t *testing.T) {
	rec := &recorder{}
	st, _ := mkStore(t, "a:\n  enabled: false\n", map[string]any{"a": switchOpts{}})
	a := comp(rec, "a", withConfigKey("a"))
	b := comp(rec, "b", needs(Dep{Kind: "a", Optional: true}))
	r := mkRegistry(t, Config{Store: st, Components: []Component{a, b}})
	startOK(t, r)

	if !rec.has("init:b") {
		t.Fatal("soft dependent must still start")
	}
	if rec.has("init:a") {
		t.Fatal("disabled component must not Init")
	}
}

func TestDisabled_FourObservables(t *testing.T) {
	rec := &recorder{}
	st, _ := mkStore(t, "a:\n  enabled: false\n", map[string]any{"a": switchOpts{}})
	a := comp(rec, "a", withConfigKey("a"))
	r := mkRegistry(t, Config{Store: st, Components: []Component{a}})
	startOK(t, r)

	// (2) access contract: two-value Get returns false.
	if _, ok := Get[*fake](r, "a"); ok {
		t.Fatal("Get on disabled must return false")
	}
	// (3) observability: present in Components as disabled...
	found := false
	for _, s := range r.Components() {
		if s.Key.Kind == "a" {
			found = true
			if s.State != StateDisabled {
				t.Fatalf("want disabled state, got %s", s.State)
			}
		}
	}
	if !found {
		t.Fatal("disabled component must appear in Components")
	}
	// ...and in Health as informational (aggregate stays Up).
	rep := r.Health(context.Background())
	if rep.Status != HealthUp {
		t.Fatalf("disabled must aggregate as OK: %+v", rep)
	}
	hasEntry := false
	for _, e := range rep.Entries {
		if e.Key.Kind == "a" && e.Status == HealthDisabled {
			hasEntry = true
		}
	}
	if !hasEntry {
		t.Fatalf("health must list the disabled entry: %+v", rep.Entries)
	}
	// (4) lifecycle boundary: no Init happened; Close must not run either.
	if rec.has("init:a") {
		t.Fatal("disabled component must not Init")
	}
	_ = r.Stop(context.Background())
	if rec.has("close:a") {
		t.Fatal("disabled component must not Close")
	}
}

func TestGet_TypedTwoValue(t *testing.T) {
	rec := &recorder{}
	r := mkRegistry(t, Config{Components: []Component{comp(rec, "a")}})
	startOK(t, r)

	if c, ok := Get[*fake](r, "a"); !ok || c == nil {
		t.Fatal("typed Get must succeed for the right type")
	}
	if _, ok := Get[*serverComp](r, "a"); ok {
		t.Fatal("wrong type assertion must return false")
	}
	if _, ok := Get[*fake](r, "nope"); ok {
		t.Fatal("unknown kind must return false")
	}
}

// --- mount phase ------------------------------------------------------------

func TestMount_OrderContract(t *testing.T) {
	rec := &recorder{}
	router := &routerDouble{rec: rec}

	early := &mounterComp{fake: fake{d: Descriptor{Kind: "early", MountOrder: -1}, rec: rec}}
	zero := &mounterComp{fake: fake{d: Descriptor{Kind: "zero"}, rec: rec}}
	late5 := &mounterComp{fake: fake{d: Descriptor{Kind: "late5", MountOrder: 5}, rec: rec}}
	late100 := &mounterComp{fake: fake{d: Descriptor{Kind: "late100", MountOrder: 100}, rec: rec}}
	prov := &providerComp{fake: fake{d: Descriptor{Kind: "prov"}, rec: rec}, router: router}

	r := mkRegistry(t, Config{
		Components: []Component{late100, zero, prov, late5, early},
		Routes: func(rt Router) error {
			rec.add("routes")
			rt.Handle("GET", "/user", http.NotFoundHandler())
			return nil
		},
	})
	startOK(t, r)

	// ≤0 mounters before user routes; >0 strictly after, ascending.
	rec.assertBefore(t, "mount:zero", "routes")
	rec.assertBefore(t, "mount:early", "routes")
	rec.assertBefore(t, "routes", "mount:late5")
	rec.assertBefore(t, "mount:late5", "mount:late100")
	if !rec.has("handle:GET:/user") {
		t.Fatalf("user routes must reach the provider's router: %v", rec.events())
	}
}

func TestMount_MountersWithoutProvider_Fails(t *testing.T) {
	rec := &recorder{}
	m := &mounterComp{fake: fake{d: Descriptor{Kind: "m"}, rec: rec}}
	r := mkRegistry(t, Config{Components: []Component{m}})
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no RouterProvider") {
		t.Fatalf("want provider-missing error, got %v", err)
	}
}

func TestMount_TwoProviders_Fails(t *testing.T) {
	rec := &recorder{}
	p1 := &providerComp{fake: fake{d: Descriptor{Kind: "p1"}, rec: rec}, router: &routerDouble{rec: rec}}
	p2 := &providerComp{fake: fake{d: Descriptor{Kind: "p2"}, rec: rec}, router: &routerDouble{rec: rec}}
	r := mkRegistry(t, Config{Components: []Component{p1, p2}})
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "multiple RouterProviders") {
		t.Fatalf("want multi-provider error, got %v", err)
	}
}

func TestMount_NoMountersNoRoutes_SkipsQuietly(t *testing.T) {
	rec := &recorder{}
	r := mkRegistry(t, Config{Components: []Component{comp(rec, "a")}})
	startOK(t, r) // no provider assembled, nothing to mount → fine
}

// --- serve phase (ports chok_test.go server themes) --------------------------

func TestServe_StartWaitsForAllReady(t *testing.T) {
	rec := &recorder{}
	s1 := &serverComp{fake: fake{d: Descriptor{Kind: "s1"}, rec: rec}}
	s2 := &serverComp{fake: fake{d: Descriptor{Kind: "s2"}, rec: rec}}
	r := mkRegistry(t, Config{Components: []Component{s1, s2}})
	startOK(t, r)
	if !rec.has("serve:s1") || !rec.has("serve:s2") {
		t.Fatalf("both servers must be running: %v", rec.events())
	}
}

func TestServe_PreReadyFailure_RollsBack(t *testing.T) {
	rec := &recorder{}
	a := comp(rec, "a")
	bad := &serverComp{fake: fake{d: Descriptor{Kind: "bad"}, rec: rec}, failBeforeReady: errors.New("bind fail")}
	r := mkRegistry(t, Config{Components: []Component{a, bad}})

	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bind fail") {
		t.Fatalf("want bind failure, got %v", err)
	}
	if !rec.has("close:a") {
		t.Fatalf("startup failure must roll everything back: %v", rec.events())
	}
}

func TestServe_UnexpectedExit_SignalsFailed(t *testing.T) {
	rec := &recorder{}
	flaky := &serverComp{fake: fake{d: Descriptor{Kind: "flaky"}, rec: rec}, exitEarly: true}
	r := mkRegistry(t, Config{Components: []Component{flaky}})
	startOK(t, r)

	select {
	case err := <-r.Failed():
		if !strings.Contains(err.Error(), "exited unexpectedly") {
			t.Fatalf("want unexpected-exit error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unexpected server exit must surface on Failed()")
	}
}

func TestStop_CancelsServeCtx_ThenClosesComponents(t *testing.T) {
	rec := &recorder{}
	srv := &serverComp{fake: fake{d: Descriptor{Kind: "srv"}, rec: rec}}
	r := mkRegistry(t, Config{Components: []Component{srv}})
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Serve returned (ctx cancel) strictly before its component closed:
	// in-flight work ends while dependencies are still alive.
	rec.assertBefore(t, "serve-exit:srv", "close:srv")
}

// --- draining phase -----------------------------------------------------------

func TestDrain_BroadcastBeforeServeCancel_AndReadyFlips(t *testing.T) {
	rec := &recorder{}
	srv := &serverComp{fake: fake{d: Descriptor{Kind: "srv"}, rec: rec}}

	var readyDuringDrain error
	var reg *Registry
	dr := &drainerComp{
		fake: fake{d: Descriptor{Kind: "dr"}, rec: rec},
		drainHook: func(ctx context.Context) {
			readyDuringDrain = reg.Ready(ctx)
		},
	}
	reg = mkRegistry(t, Config{Components: []Component{srv, dr}})
	if err := reg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Ready(context.Background()); err != nil {
		t.Fatalf("ready must pass while serving: %v", err)
	}
	if err := reg.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	rec.assertBefore(t, "drain:dr", "serve-exit:srv")
	if !errors.Is(readyDuringDrain, ErrDraining) {
		t.Fatalf("Ready during draining must return ErrDraining, got %v", readyDuringDrain)
	}
}

// --- health -------------------------------------------------------------------

func TestHealth_RequiredDownBeatsDegraded(t *testing.T) {
	rec := &recorder{}
	ok := &healtherComp{fake: fake{d: Descriptor{Kind: "ok"}, rec: rec}}
	down := &healtherComp{fake: fake{d: Descriptor{Kind: "down"}, rec: rec}, healthErr: errors.New("dead")}
	r := mkRegistry(t, Config{Components: []Component{ok, down}})
	startOK(t, r)

	rep := r.Health(context.Background())
	if rep.Status != HealthDown {
		t.Fatalf("required down ⇒ aggregate down: %+v", rep)
	}

	// Optional failing prober only degrades.
	rec2 := &recorder{}
	opt := &healtherComp{fake: fake{d: Descriptor{Kind: "opt", Optional: true}, rec: rec2}, healthErr: errors.New("meh")}
	r2 := mkRegistry(t, Config{Components: []Component{opt}})
	startOK(t, r2)
	if rep := r2.Health(context.Background()); rep.Status != HealthDegraded {
		t.Fatalf("optional down ⇒ degraded: %+v", rep)
	}
}

// --- reload (ports reload_test.go themes) ---------------------------------------

type hotOpts struct {
	Level string `mapstructure:"level" default:"info" reload:"hot"`
	Path  string `mapstructure:"path"                 reload:"restart"`
}

func reloadFixture(t *testing.T, rec *recorder, post PostReloadFunc) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ktest.yaml")
	if err := os.WriteFile(path, []byte("hot:\n  level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := conf.NewLoader("ktest", "KTEST")
	l.SetPath(path)
	if err := l.Register("hot", hotOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Register("other", switchOpts{}); err != nil {
		t.Fatal(err)
	}
	st, err := conf.NewStore(l)
	if err != nil {
		t.Fatal(err)
	}

	hot := &reloaderComp{fake: fake{d: Descriptor{Kind: "hot", ConfigKey: "hot"}, rec: rec}}
	other := &reloaderComp{fake: fake{d: Descriptor{Kind: "other", ConfigKey: "other"}, rec: rec}}
	free := &reloaderComp{fake: fake{d: Descriptor{Kind: "free"}, rec: rec}} // no ConfigKey

	r := mkRegistry(t, Config{Store: st, Components: []Component{hot, other, free}, PostReload: post})
	startOK(t, r)
	return r, path
}

func TestReload_HotDispatch_SkipsUnchanged_NoSectionLast(t *testing.T) {
	rec := &recorder{}
	r, path := reloadFixture(t, rec, func(ctx context.Context) error {
		rec.add("user-callback")
		return nil
	})

	if err := os.WriteFile(path, []byte("hot:\n  level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !rec.has("reload:hot") {
		t.Fatalf("hot-changed section must dispatch: %v", rec.events())
	}
	if rec.has("reload:other") {
		t.Fatalf("unchanged section must be skipped: %v", rec.events())
	}
	// no-ConfigKey Reloader: every reload, after all sectioned ones.
	rec.assertBefore(t, "reload:hot", "reload:free")
	rec.assertBefore(t, "reload:free", "user-callback")
}

func TestReload_RestartOnlyChange_NoDispatch(t *testing.T) {
	rec := &recorder{}
	r, path := reloadFixture(t, rec, nil)

	if err := os.WriteFile(path, []byte("hot:\n  level: info\n  path: /tmp/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.has("reload:hot") {
		t.Fatalf("restart-only change must not dispatch Reload: %v", rec.events())
	}
	if !rec.has("reload:free") {
		t.Fatalf("no-section reloader still dispatches: %v", rec.events())
	}
}

func TestReload_ConfigInvalid_NothingDispatched(t *testing.T) {
	rec := &recorder{}
	r, path := reloadFixture(t, rec, func(ctx context.Context) error {
		rec.add("user-callback")
		return nil
	})

	if err := os.WriteFile(path, []byte("hot: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); err == nil {
		t.Fatal("invalid config must fail the reload")
	}
	for _, ev := range rec.events() {
		if strings.HasPrefix(ev, "reload:") || ev == "user-callback" {
			t.Fatalf("stage-one failure must short-circuit dispatch: %v", rec.events())
		}
	}
	// Old snapshot still live.
	var h hotOpts
	if err := r.Config().Section("hot", &h); err != nil {
		t.Fatal(err)
	}
	if h.Level != "info" {
		t.Fatalf("failed reload must not pollute config: %+v", h)
	}
}

func TestReload_ComponentFailure_GatesUserCallback(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()
	path := filepath.Join(dir, "ktest.yaml")
	if err := os.WriteFile(path, []byte("hot:\n  level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := conf.NewLoader("ktest", "KTEST")
	l.SetPath(path)
	_ = l.Register("hot", hotOpts{})
	st, err := conf.NewStore(l)
	if err != nil {
		t.Fatal(err)
	}
	bad := &reloaderComp{fake: fake{d: Descriptor{Kind: "hot", ConfigKey: "hot"}, rec: rec}, reloadErr: errors.New("reload boom")}
	r := mkRegistry(t, Config{Store: st, Components: []Component{bad}, PostReload: func(ctx context.Context) error {
		rec.add("user-callback")
		return nil
	}})
	startOK(t, r)

	if err := os.WriteFile(path, []byte("hot:\n  level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = r.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reload boom") {
		t.Fatalf("component failure must fail the reload: %v", err)
	}
	if rec.has("user-callback") {
		t.Fatal("user callback must be gated on component dispatch success")
	}
}

func TestReload_UserCallbackError_FailsReload(t *testing.T) {
	rec := &recorder{}
	r, path := reloadFixture(t, rec, func(ctx context.Context) error {
		return errors.New("user veto")
	})
	if err := os.WriteFile(path, []byte("hot:\n  level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := r.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "user veto") {
		t.Fatalf("user callback error must fail the reload: %v", err)
	}
}

func TestReload_Concurrent_ErrReloadInProgress(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()
	path := filepath.Join(dir, "ktest.yaml")
	if err := os.WriteFile(path, []byte("hot:\n  level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := conf.NewLoader("ktest", "KTEST")
	l.SetPath(path)
	_ = l.Register("hot", hotOpts{})
	st, err := conf.NewStore(l)
	if err != nil {
		t.Fatal(err)
	}

	block := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	slow := &reloaderComp{fake: fake{d: Descriptor{Kind: "hot", ConfigKey: "hot"}, rec: rec}}
	slow.d.Timeouts.Reload = 30 * time.Second
	slowReload := &blockingReloader{inner: slow, entered: &once, enteredCh: entered, block: block}

	r := mkRegistry(t, Config{Store: st, Components: []Component{slowReload}})
	startOK(t, r)

	if err := os.WriteFile(path, []byte("hot:\n  level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- r.Reload(context.Background()) }()
	<-entered // first reload is inside component dispatch

	if err := r.Reload(context.Background()); !errors.Is(err, ErrReloadInProgress) {
		t.Fatalf("overlapping reload must get ErrReloadInProgress, got %v", err)
	}
	close(block)
	if err := <-firstDone; err != nil {
		t.Fatalf("first reload must succeed: %v", err)
	}

	// Sequential reloads (SIGHUP one after another) both work.
	if err := os.WriteFile(path, []byte("hot:\n  level: warn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("sequential reload must succeed: %v", err)
	}
}

// blockingReloader wraps a component to block inside Reload.
type blockingReloader struct {
	inner     *reloaderComp
	entered   *sync.Once
	enteredCh chan struct{}
	block     chan struct{}
}

func (b *blockingReloader) Describe() Descriptor { return b.inner.Describe() }
func (b *blockingReloader) Init(ctx context.Context, k Kernel) error {
	return b.inner.Init(ctx, k)
}
func (b *blockingReloader) Close(ctx context.Context) error { return b.inner.Close(ctx) }
func (b *blockingReloader) Reload(ctx context.Context) error {
	b.entered.Do(func() { close(b.enteredCh) })
	<-b.block
	return b.inner.Reload(ctx)
}

func TestReload_BeforeStart_ErrNotStarted(t *testing.T) {
	rec := &recorder{}
	r := mkRegistry(t, Config{Components: []Component{comp(rec, "a")}})
	if err := r.Reload(context.Background()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("want ErrNotStarted, got %v", err)
	}
}

func TestReload_EnabledFlip_WarnOnly_ComponentKeepsRunning(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()
	path := filepath.Join(dir, "ktest.yaml")
	if err := os.WriteFile(path, []byte("sw:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := conf.NewLoader("ktest", "KTEST")
	l.SetPath(path)
	_ = l.Register("sw", switchOpts{})
	st, err := conf.NewStore(l)
	if err != nil {
		t.Fatal(err)
	}
	c := &reloaderComp{fake: fake{d: Descriptor{Kind: "sw", ConfigKey: "sw"}, rec: rec}}
	r := mkRegistry(t, Config{Store: st, Components: []Component{c}})
	startOK(t, r)

	if err := os.WriteFile(path, []byte("sw:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.has("reload:sw") {
		t.Fatalf("enabled flip is restart-only, no hot dispatch: %v", rec.events())
	}
	if _, ok := r.Lookup("sw"); !ok {
		t.Fatal("component must keep running after an enabled flip (no hot-disable)")
	}
}

// --- events on the bus --------------------------------------------------------

func TestLifecycleEvents_PublishedToBus(t *testing.T) {
	rec := &recorder{}
	bus := event.NewBus()
	var mu sync.Mutex
	var seen []string
	event.Subscribe(bus, func(_ context.Context, e ComponentInitialized) {
		mu.Lock()
		seen = append(seen, "init:"+e.Key.String())
		mu.Unlock()
	})
	event.Subscribe(bus, func(_ context.Context, e AppStarted) {
		mu.Lock()
		seen = append(seen, "started")
		mu.Unlock()
	})
	event.Subscribe(bus, func(_ context.Context, e ComponentClosed) {
		mu.Lock()
		seen = append(seen, "closed:"+e.Key.String())
		mu.Unlock()
	})

	r := mkRegistry(t, Config{Bus: bus, Components: []Component{comp(rec, "a")}})
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Stop closes the bus after draining subscribers — events are in.
	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{"init:a": false, "started": false, "closed:a": false}
	for _, s := range seen {
		want[s] = true
	}
	for k, ok := range want {
		if !ok {
			t.Fatalf("missing lifecycle event %q: %v", k, seen)
		}
	}
}

// --- timeout vs cancel classification (M0 lesson) -------------------------------

func TestInit_TimeoutVsCancel_Distinguished(t *testing.T) {
	rec := &recorder{}
	slow := comp(rec, "slow")
	slow.d.Timeouts.Init = 30 * time.Millisecond
	slow.initHook = func(ctx context.Context, k Kernel) error {
		<-ctx.Done()
		return ctx.Err()
	}
	r := mkRegistry(t, Config{Components: []Component{slow}})
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timeout exceeded") {
		t.Fatalf("deadline expiry must read as timeout: %v", err)
	}
	if strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("timeout must not be reported as cancellation: %v", err)
	}

	rec2 := &recorder{}
	slow2 := comp(rec2, "slow2")
	slow2.initHook = func(ctx context.Context, k Kernel) error {
		<-ctx.Done()
		return ctx.Err()
	}
	r2 := mkRegistry(t, Config{Components: []Component{slow2}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err = r2.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("caller cancellation must read as cancelled: %v", err)
	}
	if strings.Contains(err.Error(), "timeout exceeded") {
		t.Fatalf("cancellation must not be misreported as timeout (the v1 emit bug): %v", err)
	}
}

var _ = fmt.Sprintf // keep fmt import when assertions change
