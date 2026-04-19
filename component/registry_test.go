package component

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/log"
)

// ---------------------------------------------------------------------------
// Test helpers: tiny Component implementations
// ---------------------------------------------------------------------------

// stub is a minimal Component. It records init/close/reload/migrate/health
// calls so tests can inspect ordering and dispatch. Optional behaviour is
// toggled via struct fields so individual tests stay terse.
type stub struct {
	name       string
	deps       []string
	initErr    error
	closeErr   error
	reloadErr  error
	migrateErr error

	// health: nil → no Healther, otherwise the returned status
	healthLevel HealthLevel

	// Log of events, shared across stubs in a test via a pointer.
	log *eventLog
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) record(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, s)
}

func (e *eventLog) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.events))
	copy(out, e.events)
	return out
}

func (s *stub) Name() string      { return s.name }
func (s *stub) ConfigKey() string { return s.name }

func (s *stub) Init(ctx context.Context, k Kernel) error {
	s.log.record("init:" + s.name)
	return s.initErr
}

func (s *stub) Close(ctx context.Context) error {
	s.log.record("close:" + s.name)
	return s.closeErr
}

// Dependent — only activated when deps is non-nil.
type stubWithDeps struct{ *stub }

func (s stubWithDeps) Dependencies() []string { return s.stub.deps }

// Reloadable
type stubReloadable struct{ *stub }

func (s stubReloadable) Reload(ctx context.Context) error {
	s.stub.log.record("reload:" + s.stub.name)
	return s.stub.reloadErr
}

// Migratable
type stubMigratable struct{ *stub }

func (s stubMigratable) Migrate(ctx context.Context) error {
	s.stub.log.record("migrate:" + s.stub.name)
	return s.stub.migrateErr
}

// Healther
type stubHealther struct{ *stub }

func (s stubHealther) Health(ctx context.Context) HealthStatus {
	return HealthStatus{Status: s.stub.healthLevel}
}

// Combined wrapper with every capability. Tests that need a subset can
// just use the appropriate single wrapper.
type fullStub struct{ *stub }

func (s fullStub) Dependencies() []string { return s.stub.deps }
func (s fullStub) Reload(ctx context.Context) error {
	s.stub.log.record("reload:" + s.stub.name)
	return s.stub.reloadErr
}
func (s fullStub) Migrate(ctx context.Context) error {
	s.stub.log.record("migrate:" + s.stub.name)
	return s.stub.migrateErr
}
func (s fullStub) Health(ctx context.Context) HealthStatus {
	return HealthStatus{Status: s.stub.healthLevel}
}

// stubSlowHealth simulates a slow health probe with optional context cancellation handling.
type stubSlowHealth struct {
	*stub
	delay        time.Duration
	downOnCancel bool // if true, return HealthDown when ctx is cancelled
}

func (s stubSlowHealth) Health(ctx context.Context) HealthStatus {
	select {
	case <-time.After(s.delay):
		return HealthStatus{Status: HealthOK}
	case <-ctx.Done():
		if s.downOnCancel {
			return HealthStatus{Status: HealthDown, Error: ctx.Err().Error()}
		}
		return HealthStatus{Status: HealthOK}
	}
}

func newLog() *eventLog { return &eventLog{} }

func mkStub(name string, log *eventLog) *stub {
	return &stub{name: name, log: log}
}

func mkReg() *Registry { return New(nil, log.Empty()) }

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestRegister_LookupByName(t *testing.T) {
	r := mkReg()
	a := mkStub("a", newLog())
	r.Register(a)

	if got := r.Get("a"); got != a {
		t.Fatalf("Get returned %v, want %v", got, a)
	}
	if got := r.Get("missing"); got != nil {
		t.Fatalf("Get(missing) = %v, want nil", got)
	}
}

func TestRegister_DuplicateNamePanics(t *testing.T) {
	r := mkReg()
	r.Register(mkStub("x", newLog()))
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register should panic")
		}
	}()
	r.Register(mkStub("x", newLog()))
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	r := mkReg()
	defer func() {
		if recover() == nil {
			t.Fatal("empty name should panic")
		}
	}()
	r.Register(mkStub("", newLog()))
}

func TestRegister_AfterStartPanics(t *testing.T) {
	r := mkReg()
	r.Register(mkStub("a", newLog()))
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	defer func() {
		if recover() == nil {
			t.Fatal("Register after Start should panic")
		}
	}()
	r.Register(mkStub("b", newLog()))
}

// ---------------------------------------------------------------------------
// Topological ordering
// ---------------------------------------------------------------------------

func TestStart_RespectsDependencyOrder(t *testing.T) {
	// account depends on db and log; db and log depend on nothing.
	// db and log are at the same level and may Init in parallel (either order).
	// Migrate for a level runs after all Init in that level.
	// account must Init after both db and log have Init'd and Migrated.
	l := newLog()
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "account", deps: []string{"db", "log"}, log: l}})
	r.Register(fullStub{stub: &stub{name: "db", log: l}})
	r.Register(fullStub{stub: &stub{name: "log", log: l}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	events := l.snapshot()

	// Helper: index of event in the log.
	indexOf := func(e string) int {
		for i, s := range events {
			if s == e {
				return i
			}
		}
		t.Fatalf("event %q not found in %v", e, events)
		return -1
	}

	// Constraints:
	// 1. Both db and log Init before their Migrate.
	if indexOf("init:db") >= indexOf("migrate:db") {
		t.Fatal("init:db must precede migrate:db")
	}
	if indexOf("init:log") >= indexOf("migrate:log") {
		t.Fatal("init:log must precede migrate:log")
	}
	// 2. Level 0 Migrate completes before level 1 Init.
	if indexOf("migrate:db") >= indexOf("init:account") {
		t.Fatal("migrate:db must precede init:account")
	}
	if indexOf("migrate:log") >= indexOf("init:account") {
		t.Fatal("migrate:log must precede init:account")
	}
	// 3. account Init before account Migrate.
	if indexOf("init:account") >= indexOf("migrate:account") {
		t.Fatal("init:account must precede migrate:account")
	}
	// 4. All 6 events present.
	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d: %v", len(events), events)
	}
}

func TestStart_DetectsCycle(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", deps: []string{"b"}, log: newLog()}})
	r.Register(fullStub{stub: &stub{name: "b", deps: []string{"a"}, log: newLog()}})

	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestStart_DetectsUnknownDependency(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", deps: []string{"ghost"}, log: newLog()}})

	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected unknown-dep error, got %v", err)
	}
}

func TestStart_RejectsSelfDependency(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", deps: []string{"a"}, log: newLog()}})

	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "self") {
		t.Fatalf("expected self-dep error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Init/Close symmetry
// ---------------------------------------------------------------------------

func TestStop_ReverseOrder(t *testing.T) {
	l := newLog()
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: l}})
	r.Register(fullStub{stub: &stub{name: "b", deps: []string{"a"}, log: l}})
	r.Register(fullStub{stub: &stub{name: "c", deps: []string{"b"}, log: l}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := l.snapshot()
	// After init/migrate sequence, Close should be c, b, a.
	closeEvents := filterPrefix(events, "close:")
	want := []string{"close:c", "close:b", "close:a"}
	if !sliceEq(closeEvents, want) {
		t.Fatalf("close order wrong:\n got: %v\nwant: %v", closeEvents, want)
	}
}

func TestStart_InitFailure_RollsBack(t *testing.T) {
	l := newLog()
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: l}})
	r.Register(fullStub{stub: &stub{name: "b", deps: []string{"a"}, log: l, initErr: errors.New("boom")}})
	r.Register(fullStub{stub: &stub{name: "c", deps: []string{"b"}, log: l}})

	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected Init error, got %v", err)
	}

	events := l.snapshot()
	// Expected: init:a, migrate:a, init:b (fails) — then rollback closes a.
	// c never runs.
	if !contains(events, "init:a") {
		t.Fatalf("a should have initialised: %v", events)
	}
	if contains(events, "init:c") {
		t.Fatalf("c should not have run: %v", events)
	}
	if !contains(events, "close:a") {
		t.Fatalf("a should have been closed on rollback: %v", events)
	}
	if contains(events, "close:b") {
		// b's Init failed, so Close should NOT run for it.
		t.Fatalf("failed b should not be closed: %v", events)
	}
}

func TestStop_Idempotent(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: newLog()}})
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second Stop is a no-op.
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop should be nil, got %v", err)
	}
}

func TestStop_CollectsCloseErrors(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: newLog(), closeErr: errors.New("a-fail")}})
	r.Register(fullStub{stub: &stub{name: "b", log: newLog(), closeErr: errors.New("b-fail")}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := r.Stop(context.Background())
	if err == nil {
		t.Fatal("expected joined close errors")
	}
	if !strings.Contains(err.Error(), "a-fail") || !strings.Contains(err.Error(), "b-fail") {
		t.Fatalf("both close errors should surface: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reload dispatch
// ---------------------------------------------------------------------------

type plainNoReload struct {
	*stub
	reloadCalled *atomic.Bool
}

func (p plainNoReload) ConfigKey() string { return p.stub.name }

// NB: intentionally does NOT implement Reloadable.

func TestReload_OnlyDispatchesToReloadable(t *testing.T) {
	l := newLog()
	r := mkReg()

	r.Register(fullStub{stub: &stub{name: "has-reload", log: l}})

	var called atomic.Bool
	r.Register(plainNoReload{stub: mkStub("no-reload", l), reloadCalled: &called})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := l.snapshot()
	if !contains(events, "reload:has-reload") {
		t.Fatalf("Reloadable component should have been reloaded: %v", events)
	}
	if contains(events, "reload:no-reload") {
		t.Fatalf("non-Reloadable must not see reload: %v", events)
	}
	if called.Load() {
		t.Fatal("non-Reloadable reloadCalled flag unexpectedly set")
	}
}

func TestReload_BeforeStart_Errors(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: newLog()}})

	err := r.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Start") {
		t.Fatalf("expected pre-start error, got %v", err)
	}
}

func TestReload_CollectsErrors(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: newLog(), reloadErr: errors.New("a-bad")}})
	r.Register(fullStub{stub: &stub{name: "b", log: newLog(), reloadErr: errors.New("b-bad")}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	err := r.Reload(context.Background())
	if err == nil {
		t.Fatal("expected joined reload errors")
	}
	if !strings.Contains(err.Error(), "a-bad") || !strings.Contains(err.Error(), "b-bad") {
		t.Fatalf("both reload errors should surface: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Health aggregation
// ---------------------------------------------------------------------------

func TestHealth_AllOK(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: newLog(), healthLevel: HealthOK}})
	r.Register(fullStub{stub: &stub{name: "b", log: newLog(), healthLevel: HealthOK}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	rep := r.Health(context.Background())
	if rep.Status != HealthOK {
		t.Fatalf("aggregate should be ok, got %q", rep.Status)
	}
	if len(rep.Components) != 2 {
		t.Fatalf("expected 2 component entries, got %v", rep.Components)
	}
}

func TestHealth_AnyDown_PullsAggregateDown(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: newLog(), healthLevel: HealthOK}})
	r.Register(fullStub{stub: &stub{name: "b", log: newLog(), healthLevel: HealthDown}})
	r.Register(fullStub{stub: &stub{name: "c", log: newLog(), healthLevel: HealthDegraded}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	rep := r.Health(context.Background())
	if rep.Status != HealthDown {
		t.Fatalf("aggregate should be down, got %q", rep.Status)
	}
}

func TestHealth_AnyDegraded_WhenNoneDown(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: newLog(), healthLevel: HealthOK}})
	r.Register(fullStub{stub: &stub{name: "b", log: newLog(), healthLevel: HealthDegraded}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	rep := r.Health(context.Background())
	if rep.Status != HealthDegraded {
		t.Fatalf("aggregate should be degraded, got %q", rep.Status)
	}
}

func TestHealth_ParallelExecution(t *testing.T) {
	// Two components each sleep 50ms. With sequential execution this
	// takes ≥100ms; parallel should complete in ~50ms.
	r := mkReg()
	r.Register(stubSlowHealth{stub: mkStub("a", newLog()), delay: 50 * time.Millisecond})
	r.Register(stubSlowHealth{stub: mkStub("b", newLog()), delay: 50 * time.Millisecond})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	start := time.Now()
	rep := r.Health(context.Background())
	elapsed := time.Since(start)

	if rep.Status != HealthOK {
		t.Fatalf("expected ok, got %q", rep.Status)
	}
	if len(rep.Components) != 2 {
		t.Fatalf("expected 2 components, got %d", len(rep.Components))
	}
	// Allow generous margin (80ms) but it must be < 100ms (sequential).
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("health checks appear sequential: took %v", elapsed)
	}
}

func TestHealth_ProbeTimeout(t *testing.T) {
	r := mkReg()
	r.SetHealthTimeout(30 * time.Millisecond)
	r.Register(stubSlowHealth{stub: mkStub("slow", newLog()), delay: 5 * time.Second, downOnCancel: true})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	rep := r.Health(context.Background())
	st, ok := rep.Components["slow"]
	if !ok {
		t.Fatal("slow component missing from report")
	}
	if st.Status != HealthDown {
		t.Fatalf("timed-out probe should be down, got %q", st.Status)
	}
}

func TestHealth_NonHealtherDefaultsToOK(t *testing.T) {
	// plainNoReload doesn't implement Healther.
	r := mkReg()
	var cb atomic.Bool
	r.Register(plainNoReload{stub: mkStub("no-health", newLog()), reloadCalled: &cb})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	rep := r.Health(context.Background())
	got, ok := rep.Components["no-health"]
	if !ok {
		t.Fatalf("component missing from report: %+v", rep)
	}
	if got.Status != HealthOK {
		t.Fatalf("non-Healther default should be ok, got %q", got.Status)
	}
}

// ---------------------------------------------------------------------------
// Event hooks
// ---------------------------------------------------------------------------

func TestEventHooks_FireInOrder(t *testing.T) {
	l := newLog()
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: l}})

	r.On(EventBeforeStart, func(ctx context.Context) error { l.record("hook:before_start"); return nil })
	r.On(EventAfterStart, func(ctx context.Context) error { l.record("hook:after_start"); return nil })
	r.On(EventBeforeStop, func(ctx context.Context) error { l.record("hook:before_stop"); return nil })
	r.On(EventAfterStop, func(ctx context.Context) error { l.record("hook:after_stop"); return nil })

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := l.snapshot()
	// Sanity: before_start → init:a → migrate:a → after_start → before_stop → close:a → after_stop.
	want := []string{
		"hook:before_start",
		"init:a",
		"migrate:a",
		"hook:after_start",
		"hook:before_stop",
		"close:a",
		"hook:after_stop",
	}
	if !sliceEq(events, want) {
		t.Fatalf("event order wrong:\n got: %v\nwant: %v", events, want)
	}
}

func TestEventHook_BeforeStart_AbortsStart(t *testing.T) {
	l := newLog()
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: l}})
	r.On(EventBeforeStart, func(ctx context.Context) error { return errors.New("veto") })

	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "veto") {
		t.Fatalf("expected before_start error, got %v", err)
	}
	if contains(l.snapshot(), "init:a") {
		t.Fatal("Init should not run after before_start veto")
	}
}

func TestKernelOn_AvailableToComponents(t *testing.T) {
	// A component registering a hook in its own Init should see it
	// fire as expected.
	l := newLog()
	r := mkReg()
	r.Register(&hookingComponent{name: "h", l: l})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := l.snapshot()
	if !contains(events, "hook:component-after_stop") {
		t.Fatalf("component-registered hook did not fire: %v", events)
	}
}

type hookingComponent struct {
	name string
	l    *eventLog
}

func (h *hookingComponent) Name() string      { return h.name }
func (h *hookingComponent) ConfigKey() string { return h.name }
func (h *hookingComponent) Init(ctx context.Context, k Kernel) error {
	k.On(EventAfterStop, func(ctx context.Context) error {
		h.l.record("hook:component-after_stop")
		return nil
	})
	return nil
}
func (h *hookingComponent) Close(ctx context.Context) error { return nil }

func TestEmit_InjectsEventIntoContext(t *testing.T) {
	r := mkReg()
	r.Register(mkStub("a", newLog()))

	var gotEvent Event
	var gotReason string
	r.On(EventBeforeReload, func(ctx context.Context) error {
		gotEvent = EventFrom(ctx)
		gotReason = ReasonFrom(ctx)
		return nil
	})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	// Inject a reason via context, then reload.
	ctx := WithReason(context.Background(), "test-trigger")
	_ = r.Reload(ctx)

	if gotEvent != EventBeforeReload {
		t.Fatalf("expected EventBeforeReload, got %q", gotEvent)
	}
	if gotReason != "test-trigger" {
		t.Fatalf("expected reason test-trigger, got %q", gotReason)
	}
}

func TestReasonFrom_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if r := ReasonFrom(ctx); r != "" {
		t.Fatalf("empty ctx should return empty reason, got %q", r)
	}
	ctx = WithReason(ctx, "signal")
	if r := ReasonFrom(ctx); r != "signal" {
		t.Fatalf("expected signal, got %q", r)
	}
}

func TestEventFrom_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if e := EventFrom(ctx); e != "" {
		t.Fatalf("empty ctx should return empty event, got %q", e)
	}
	ctx = WithEvent(ctx, EventAfterStart)
	if e := EventFrom(ctx); e != EventAfterStart {
		t.Fatalf("expected after_start, got %q", e)
	}
}

// ---------------------------------------------------------------------------
// Kernel wiring
// ---------------------------------------------------------------------------

func TestKernelConfig_FlowsThrough(t *testing.T) {
	type appCfg struct{ Marker string }
	cfg := &appCfg{Marker: "hello"}
	r := New(cfg, log.Empty())

	seen := make(chan any, 1)
	r.Register(&configAware{seen: seen})
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	got := <-seen
	gotCfg, ok := got.(*appCfg)
	if !ok || gotCfg.Marker != "hello" {
		t.Fatalf("config did not reach component: %+v", got)
	}
}

type configAware struct {
	seen chan any
}

func (c *configAware) Name() string      { return "cfg-aware" }
func (c *configAware) ConfigKey() string { return "" }
func (c *configAware) Init(ctx context.Context, k Kernel) error {
	c.seen <- k.Config()
	return nil
}
func (c *configAware) Close(ctx context.Context) error { return nil }

func TestKernelGet_ResolvesDependency(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "db", log: newLog()}})
	r.Register(&peekAtDb{})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
}

type peekAtDb struct{}

func (p *peekAtDb) Name() string                    { return "p" }
func (p *peekAtDb) ConfigKey() string               { return "p" }
func (p *peekAtDb) Dependencies() []string          { return []string{"db"} }
func (p *peekAtDb) Close(ctx context.Context) error { return nil }
func (p *peekAtDb) Init(ctx context.Context, k Kernel) error {
	dep := k.Get("db")
	if dep == nil {
		return fmt.Errorf("db dependency not resolvable via Kernel.Get")
	}
	if dep.Name() != "db" {
		return fmt.Errorf("expected db, got %q", dep.Name())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Verify (dry-run graph validation)
// ---------------------------------------------------------------------------

func TestVerify_ValidGraph(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", log: newLog()}})
	r.Register(fullStub{stub: &stub{name: "b", deps: []string{"a"}, log: newLog()}})

	if err := r.Verify(); err != nil {
		t.Fatalf("valid graph should pass Verify: %v", err)
	}
}

func TestVerify_DetectsCycle(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", deps: []string{"b"}, log: newLog()}})
	r.Register(fullStub{stub: &stub{name: "b", deps: []string{"a"}, log: newLog()}})

	err := r.Verify()
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestVerify_DetectsUnknownDependency(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "a", deps: []string{"ghost"}, log: newLog()}})

	err := r.Verify()
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected unknown-dep error, got %v", err)
	}
}

func TestOrder_ReturnsNames(t *testing.T) {
	r := mkReg()
	r.Register(fullStub{stub: &stub{name: "b", deps: []string{"a"}, log: newLog()}})
	r.Register(fullStub{stub: &stub{name: "a", log: newLog()}})

	names, err := r.Order()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b"}
	if !sliceEq(names, want) {
		t.Fatalf("order = %v, want %v", names, want)
	}
}

// ---------------------------------------------------------------------------
// Optional components
// ---------------------------------------------------------------------------

type optionalStub struct {
	*stub
}

func (o optionalStub) Optional() bool { return true }

func TestStart_OptionalInitFailure_DoesNotAbort(t *testing.T) {
	l := newLog()
	r := mkReg()
	r.Register(mkStub("a", l))
	r.Register(optionalStub{stub: &stub{name: "opt", log: l, initErr: errors.New("cache down")}})
	r.Register(mkStub("b", l))

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("optional failure should not abort: %v", err)
	}
	defer r.Stop(context.Background())

	// a and b should have init'd; opt should be skipped.
	events := l.snapshot()
	if !contains(events, "init:a") || !contains(events, "init:b") {
		t.Fatalf("required components should init: %v", events)
	}
	// opt should be removed from byName — Get returns nil.
	if r.Get("opt") != nil {
		t.Fatal("Get(opt) should return nil after optional init failure")
	}
}

type optionalMigratableStub struct {
	optionalStub
}

func (o optionalMigratableStub) Migrate(ctx context.Context) error {
	o.stub.log.record("migrate:" + o.stub.name)
	return o.stub.migrateErr
}

func TestStart_OptionalMigrateFailure_DoesNotAbort(t *testing.T) {
	l := newLog()
	r := mkReg()
	r.Register(mkStub("a", l))
	r.Register(optionalMigratableStub{
		optionalStub: optionalStub{stub: &stub{name: "opt", log: l, migrateErr: errors.New("migrate fail")}},
	})
	r.Register(mkStub("b", l))

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("optional migrate failure should not abort: %v", err)
	}
	defer r.Stop(context.Background())

	events := l.snapshot()
	if !contains(events, "init:a") || !contains(events, "init:b") {
		t.Fatalf("required components should init: %v", events)
	}
	// opt should have been closed after migrate failure.
	if !contains(events, "close:opt") {
		t.Fatalf("failed optional component should be closed: %v", events)
	}
}

func TestStart_RequiredInitFailure_StillAborts(t *testing.T) {
	// Verify that non-optional failures still abort as before.
	l := newLog()
	r := mkReg()
	r.Register(mkStub("a", l))
	r.Register(&stub{name: "required", log: l, initErr: errors.New("boom")})

	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("required failure should abort: %v", err)
	}
}

func TestStart_OptionalInitFailure_HealthReportsFailed(t *testing.T) {
	l := newLog()
	r := mkReg()
	r.Register(mkStub("a", l))
	r.Register(optionalStub{stub: &stub{name: "opt", log: l, initErr: errors.New("cache down")}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("optional failure should not abort: %v", err)
	}
	defer r.Stop(context.Background())

	rep := r.Health(context.Background())
	got, ok := rep.Components["opt"]
	if !ok {
		t.Fatalf("failed optional component should appear in health report: %+v", rep)
	}
	if got.Status != HealthDegraded {
		t.Fatalf("expected HealthDegraded for failed optional, got %q", got.Status)
	}
	if got.Error != "cache down" {
		t.Fatalf("expected error message in health status, got %q", got.Error)
	}
	if rep.Status != HealthDegraded {
		t.Fatalf("aggregate status should be HealthDegraded, got %q", rep.Status)
	}
}

func TestStart_ParallelInit_SameLevel(t *testing.T) {
	// Two independent components each sleep 50ms. With parallel init
	// the total should be ~50ms, not ~100ms.
	l := newLog()
	r := mkReg()
	r.Register(slowInit{stub: &stub{name: "x", log: l}, delay: 50 * time.Millisecond})
	r.Register(slowInit{stub: &stub{name: "y", log: l}, delay: 50 * time.Millisecond})

	start := time.Now()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	elapsed := time.Since(start)

	if elapsed > 90*time.Millisecond {
		t.Fatalf("parallel init too slow (%v), expected ~50ms", elapsed)
	}
}

func TestTopoSortLeveled_Levels(t *testing.T) {
	// a, b: no deps (level 0)
	// c: depends on a (level 1)
	// d: depends on a, b (level 1)
	// e: depends on c, d (level 2)
	l := newLog()
	r := New(nil, log.Empty())
	r.Register(&stub{name: "a", log: l})
	r.Register(&stub{name: "b", log: l})
	r.Register(stubWithDeps{&stub{name: "c", deps: []string{"a"}, log: l}})
	r.Register(stubWithDeps{&stub{name: "d", deps: []string{"a", "b"}, log: l}})
	r.Register(stubWithDeps{&stub{name: "e", deps: []string{"c", "d"}, log: l}})

	levels, err := r.topoSortLeveled()
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
	nameSet := func(lvl []Component) map[string]bool {
		m := make(map[string]bool)
		for _, c := range lvl {
			m[c.Name()] = true
		}
		return m
	}
	l0 := nameSet(levels[0])
	l1 := nameSet(levels[1])
	l2 := nameSet(levels[2])
	if !l0["a"] || !l0["b"] || len(l0) != 2 {
		t.Fatalf("level 0 wrong: %v", l0)
	}
	if !l1["c"] || !l1["d"] || len(l1) != 2 {
		t.Fatalf("level 1 wrong: %v", l1)
	}
	if !l2["e"] || len(l2) != 1 {
		t.Fatalf("level 2 wrong: %v", l2)
	}
}

// ---------------------------------------------------------------------------
// Per-component init timeout
// ---------------------------------------------------------------------------

type slowInit struct {
	*stub
	delay time.Duration
}

func (s slowInit) Init(ctx context.Context, k Kernel) error {
	s.stub.log.record("init:" + s.stub.name)
	select {
	case <-time.After(s.delay):
		return s.stub.initErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

type slowInitWithTimeout struct {
	slowInit
	timeout time.Duration
}

func (s slowInitWithTimeout) InitTimeout() time.Duration { return s.timeout }

func TestStart_DefaultInitTimeout(t *testing.T) {
	l := newLog()
	r := New(nil, log.Empty())
	r.SetDefaultInitTimeout(50 * time.Millisecond)
	r.Register(slowInit{stub: &stub{name: "slow", log: l}, delay: 5 * time.Second})

	err := r.Start(context.Background())
	if err == nil {
		r.Stop(context.Background())
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected deadline error, got: %v", err)
	}
}

func TestStart_ComponentInitTimeoutOverridesDefault(t *testing.T) {
	l := newLog()
	r := New(nil, log.Empty())
	r.SetDefaultInitTimeout(5 * time.Second) // long default

	r.Register(slowInitWithTimeout{
		slowInit: slowInit{stub: &stub{name: "slow", log: l}, delay: 5 * time.Second},
		timeout:  50 * time.Millisecond, // short override
	})

	err := r.Start(context.Background())
	if err == nil {
		r.Stop(context.Background())
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected deadline error, got: %v", err)
	}
}

func TestStart_NoTimeoutByDefault(t *testing.T) {
	// Without SetDefaultInitTimeout, a quick init should still work fine.
	l := newLog()
	r := mkReg()
	r.Register(slowInit{stub: &stub{name: "fast", log: l}, delay: 1 * time.Millisecond})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Stop(context.Background())
}

// ---------------------------------------------------------------------------
// Type-safe Get / MustGet
// ---------------------------------------------------------------------------

func TestGet_TypeSafe(t *testing.T) {
	r := mkReg()
	l := newLog()
	s := fullStub{stub: &stub{name: "db", log: l}}
	r.Register(s)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	got, ok := Get[fullStub](r, "db")
	if !ok {
		t.Fatal("Get should find registered component")
	}
	if got.Name() != "db" {
		t.Fatalf("expected db, got %q", got.Name())
	}
}

func TestGet_MissingReturnsZero(t *testing.T) {
	r := mkReg()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	_, ok := Get[fullStub](r, "ghost")
	if ok {
		t.Fatal("Get should return false for missing component")
	}
}

func TestGet_WrongTypeReturnsFalse(t *testing.T) {
	r := mkReg()
	r.Register(mkStub("a", newLog()))
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	// a is *stub, not fullStub
	_, ok := Get[fullStub](r, "a")
	if ok {
		t.Fatal("Get should return false for type mismatch")
	}
}

func TestMustGet_Panics(t *testing.T) {
	r := mkReg()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	defer func() {
		if recover() == nil {
			t.Fatal("MustGet should panic for missing component")
		}
	}()
	MustGet[fullStub](r, "ghost")
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func filterPrefix(events []string, prefix string) []string {
	var out []string
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// TestGet_StopScrubsAvailable verifies the C1 fix: once Stop has
// Close'd a component, Get must stop returning a reference to it. The
// hook seen during EventBeforeStop still observes the live component
// because Stop scrubs entries lazily as Close runs.
func TestGet_StopScrubsAvailable(t *testing.T) {
	r := mkReg()
	a := mkStub("a", newLog())
	r.Register(a)

	beforeStopGet := make(chan Component, 1)
	r.On(EventBeforeStop, func(context.Context) error {
		beforeStopGet <- r.Get("a")
		return nil
	})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.Get("a"); got == nil {
		t.Fatal("expected component visible after Start")
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-beforeStopGet:
		if got == nil {
			t.Fatal("BeforeStop hook saw nil component, expected live one")
		}
	default:
		t.Fatal("BeforeStop hook did not run")
	}
	if got := r.Get("a"); got != nil {
		t.Fatalf("expected Get to return nil after Stop, got %v", got)
	}
}

// TestGet_RollbackScrubsAvailable verifies that when a required
// component fails Init, the rollback path Close's already-Init'd peers
// and Get returns nil for them — preventing post-rollback hooks from
// touching torn-down state.
func TestGet_RollbackScrubsAvailable(t *testing.T) {
	r := mkReg()
	good := mkStub("good", newLog())
	bad := &stub{name: "bad", deps: []string{"good"}, initErr: errors.New("boom"), log: newLog()}
	r.Register(good)
	r.Register(bad)

	if err := r.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail")
	}
	if got := r.Get("good"); got != nil {
		t.Fatalf("good should be Get-invisible after rollback, got %v", got)
	}
}

// panicValidator panics inside ValidateDeps; the registry must turn the
// panic into a structured error rather than crashing Start.
type panicValidator struct{ stub *stub }

func (p *panicValidator) Name() string                          { return p.stub.Name() }
func (p *panicValidator) ConfigKey() string                     { return p.stub.Name() }
func (p *panicValidator) Init(c context.Context, k Kernel) error {
	return p.stub.Init(c, k)
}
func (p *panicValidator) Close(c context.Context) error  { return p.stub.Close(c) }
func (p *panicValidator) ValidateDeps(_ Kernel) error    { panic("nope") }

func TestValidateDeps_RecoversPanic(t *testing.T) {
	r := mkReg()
	r.Register(&panicValidator{stub: mkStub("v", newLog())})

	err := r.Start(context.Background())
	if err == nil {
		t.Fatal("Start should fail when ValidateDeps panics")
	}
	if !strings.Contains(err.Error(), "validate deps panicked") {
		t.Fatalf("error should mention recovered panic, got %v", err)
	}
}
