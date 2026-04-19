package component

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/log"
)

// ---------------------------------------------------------------------------
// DepsValidator
// ---------------------------------------------------------------------------

// stubDepsValidator is a stub that implements DepsValidator.
type stubDepsValidator struct {
	*stub
	validateErr error
}

func (s stubDepsValidator) ValidateDeps(k Kernel) error {
	return s.validateErr
}

func TestDepsValidator_BlocksStart(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()
	s := &stubDepsValidator{
		stub:        mkStub("validator", el),
		validateErr: errors.New("db not configured"),
	}
	r.Register(s)

	err := r.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to fail when DepsValidator returns error")
	}
	if !strings.Contains(err.Error(), "db not configured") {
		t.Fatalf("expected validation error message, got: %v", err)
	}

	// Init should NOT have been called.
	events := el.snapshot()
	for _, e := range events {
		if strings.HasPrefix(e, "init:") {
			t.Fatalf("Init should not be called when DepsValidator fails, got event: %s", e)
		}
	}
}

func TestDepsValidator_PassesStart(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()
	s := &stubDepsValidator{
		stub:        mkStub("validator", el),
		validateErr: nil,
	}
	r.Register(s)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start should succeed when DepsValidator returns nil: %v", err)
	}
	defer r.Stop(context.Background())

	events := el.snapshot()
	if !contains(events, "init:validator") {
		t.Fatal("Init should have been called after DepsValidator passes")
	}
}

func TestDepsValidator_MultipleErrors(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()
	s1 := &stubDepsValidator{
		stub:        mkStub("v1", el),
		validateErr: errors.New("error from v1"),
	}
	s2 := &stubDepsValidator{
		stub:        mkStub("v2", el),
		validateErr: errors.New("error from v2"),
	}
	r.Register(s1)
	r.Register(s2)

	err := r.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to fail")
	}
	if !strings.Contains(err.Error(), "error from v1") || !strings.Contains(err.Error(), "error from v2") {
		t.Fatalf("expected both validation errors, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CloseTimeouter
// ---------------------------------------------------------------------------

type stubCloseTimeouter struct {
	*stub
	timeout  time.Duration
	closeDur time.Duration // how long Close actually takes
}

func (s stubCloseTimeouter) CloseTimeout() time.Duration { return s.timeout }

func (s stubCloseTimeouter) Close(ctx context.Context) error {
	s.stub.log.record("close:" + s.stub.name)
	select {
	case <-time.After(s.closeDur):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCloseTimeouter_TimeoutApplied(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()
	s := stubCloseTimeouter{
		stub:     mkStub("slow", el),
		timeout:  50 * time.Millisecond,
		closeDur: 5 * time.Second, // would block without timeout
	}
	r.Register(s)

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_ = r.Stop(context.Background())
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("Stop took %v, expected close timeout of ~50ms to cut it short", elapsed)
	}
}

func TestSetCloseTimeout_DefaultApplied(t *testing.T) {
	r := New(nil, log.Empty())
	r.SetCloseTimeout(50 * time.Millisecond)

	el := newLog()
	// This stub does NOT implement CloseTimeouter, so the default applies.
	slowStub := mkStub("slow", el)
	slowStub.closeErr = nil // Close won't error, but we override Close below
	r.Register(slowCloser{stub: slowStub, dur: 5 * time.Second})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_ = r.Stop(context.Background())
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("Stop took %v, expected default close timeout of ~50ms", elapsed)
	}
}

// slowCloser overrides Close to sleep.
type slowCloser struct {
	*stub
	dur time.Duration
}

func (s slowCloser) Close(ctx context.Context) error {
	s.stub.log.record("close:" + s.stub.name)
	select {
	case <-time.After(s.dur):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// initLevel cancellation on failure
// ---------------------------------------------------------------------------

func TestInitLevel_CancelsPeersOnFailure(t *testing.T) {
	r := New(nil, log.Empty())
	r.SetDefaultInitTimeout(5 * time.Second)

	el := newLog()
	// Two components at the same level (no deps on each other).
	// "fast-fail" fails immediately, "slow-peer" blocks until ctx is cancelled.
	fastFail := mkStub("fast-fail", el)
	fastFail.initErr = errors.New("instant failure")

	slowPeer := &stubSlowInit{
		stub: mkStub("slow-peer", el),
		dur:  10 * time.Second, // would block forever without cancellation
	}

	r.Register(fastFail)
	r.Register(slowPeer)

	start := time.Now()
	err := r.Start(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		defer r.Stop(context.Background())
		t.Fatal("expected Start to fail")
	}
	if !strings.Contains(err.Error(), "fast-fail") {
		t.Fatalf("expected error from fast-fail, got: %v", err)
	}
	// The slow peer should have been cancelled quickly (not waited 10s).
	if elapsed > 3*time.Second {
		t.Fatalf("initLevel took %v, expected quick cancellation of slow peer", elapsed)
	}
}

// stubSlowInit blocks in Init until ctx is cancelled or dur elapses.
type stubSlowInit struct {
	*stub
	dur time.Duration
}

func (s *stubSlowInit) Init(ctx context.Context, k Kernel) error {
	s.stub.log.record("init:" + s.stub.name)
	select {
	case <-time.After(s.dur):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// ConfigChanged context
// ---------------------------------------------------------------------------

func TestConfigChanged_DefaultTrue(t *testing.T) {
	// No WithConfigChanged → conservative default: true.
	ctx := context.Background()
	if !ConfigChanged(ctx) {
		t.Fatal("ConfigChanged should default to true when not set")
	}
}

func TestConfigChanged_SetTrue(t *testing.T) {
	ctx := WithConfigChanged(context.Background(), true)
	if !ConfigChanged(ctx) {
		t.Fatal("ConfigChanged should return true")
	}
}

func TestConfigChanged_SetFalse(t *testing.T) {
	ctx := WithConfigChanged(context.Background(), false)
	if ConfigChanged(ctx) {
		t.Fatal("ConfigChanged should return false")
	}
}

// ---------------------------------------------------------------------------
// WithReloadConfigChanged
// ---------------------------------------------------------------------------

type stubReloadConfigCheck struct {
	*stub
	configChanged bool
}

func (s *stubReloadConfigCheck) Reload(ctx context.Context) error {
	s.configChanged = ConfigChanged(ctx)
	s.stub.log.record("reload:" + s.stub.name)
	return nil
}

func TestWithReloadConfigChanged_PropagatedToReload(t *testing.T) {
	tests := []struct {
		name    string
		changed bool
	}{
		{"changed=true", true},
		{"changed=false", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(nil, log.Empty())
			el := newLog()
			s := &stubReloadConfigCheck{stub: mkStub("checker", el)}
			r.Register(s)

			if err := r.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer r.Stop(context.Background())

			err := r.Reload(context.Background(), WithReloadConfigChanged(tt.changed))
			if err != nil {
				t.Fatal(err)
			}

			if s.configChanged != tt.changed {
				t.Fatalf("expected ConfigChanged=%v in Reload ctx, got %v", tt.changed, s.configChanged)
			}
		})
	}
}

func TestWithReloadConfigChanged_DefaultTrueWithoutOption(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()
	s := &stubReloadConfigCheck{stub: mkStub("checker", el)}
	r.Register(s)

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	// No WithReloadConfigChanged → default true (conservative).
	err := r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.configChanged {
		t.Fatal("expected ConfigChanged=true when no option is passed")
	}
}

// ---------------------------------------------------------------------------
// closeContext priority: CloseTimeouter > registry default
// ---------------------------------------------------------------------------

func TestCloseContext_ComponentTimeoutOverridesDefault(t *testing.T) {
	r := New(nil, log.Empty())
	r.SetCloseTimeout(10 * time.Second) // generous default

	el := newLog()
	s := stubCloseTimeouter{
		stub:     mkStub("fast-close", el),
		timeout:  50 * time.Millisecond, // component says 50ms
		closeDur: 5 * time.Second,       // would block
	}
	r.Register(s)

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_ = r.Stop(context.Background())
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("Close took %v; CloseTimeouter's 50ms should override the 10s default", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Utility: ensure slow-peer gets Name/ConfigKey right
// ---------------------------------------------------------------------------

func (s *stubSlowInit) Name() string      { return s.stub.name }
func (s *stubSlowInit) ConfigKey() string { return s.stub.name }
func (s *stubSlowInit) Close(ctx context.Context) error {
	s.stub.log.record("close:" + s.stub.name)
	return nil
}

// ---------------------------------------------------------------------------
// validateDeps integrates with full Start lifecycle
// ---------------------------------------------------------------------------

func TestDepsValidator_RunsBeforeAnyInit(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()

	// Register a normal component and a validator that fails.
	normal := mkStub("normal", el)
	validator := &stubDepsValidator{
		stub:        mkStub("validator", el),
		validateErr: fmt.Errorf("missing dependency"),
	}

	r.Register(normal)
	r.Register(validator)

	err := r.Start(context.Background())
	if err == nil {
		defer r.Stop(context.Background())
		t.Fatal("expected Start to fail")
	}

	// Neither component should have Init called.
	events := el.snapshot()
	for _, e := range events {
		if strings.HasPrefix(e, "init:") {
			t.Fatalf("no Init should run when validateDeps fails, got event: %s", e)
		}
	}
}

// ---------------------------------------------------------------------------
// Health does NOT probe optional components that failed Init
// ---------------------------------------------------------------------------

// optionalHealtherStub is an optional component that implements Healther.
// If Health() is called on an uninitialised instance, it panics — which
// is exactly the scenario we want to prevent.
type optionalHealtherStub struct {
	*stub
	initialised bool
}

func (o *optionalHealtherStub) Optional() bool { return true }

func (o *optionalHealtherStub) Init(ctx context.Context, k Kernel) error {
	o.stub.log.record("init:" + o.stub.name)
	if o.stub.initErr != nil {
		return o.stub.initErr
	}
	o.initialised = true
	return nil
}

func (o *optionalHealtherStub) Health(ctx context.Context) HealthStatus {
	if !o.initialised {
		panic("Health called on uninitialised optional component")
	}
	return HealthStatus{Status: HealthOK}
}

func TestHealth_SkipsFailedOptionalHealther(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()

	// "ok" component succeeds Init; "opt" is optional and fails Init.
	r.Register(mkStub("ok", el))
	r.Register(&optionalHealtherStub{
		stub: &stub{name: "opt", log: el, initErr: errors.New("unavailable")},
	})

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("optional failure should not abort: %v", err)
	}
	defer r.Stop(context.Background())

	// Health must NOT call opt.Health() (it would panic).
	// Instead, opt should appear as Degraded from the failed map.
	report := r.Health(context.Background())

	got, ok := report.Components["opt"]
	if !ok {
		t.Fatal("failed optional should appear in health report")
	}
	if got.Status != HealthDegraded {
		t.Fatalf("expected HealthDegraded for failed optional, got %q", got.Status)
	}

	// Aggregate should NOT be Down.
	if report.Status == HealthDown {
		t.Fatal("aggregate should not be Down when only an optional component failed")
	}
}

// ---------------------------------------------------------------------------
// StartedComponents excludes failed optional components
// ---------------------------------------------------------------------------

func TestStartedComponents_ExcludesFailedOptional(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()

	r.Register(mkStub("a", el))
	r.Register(optionalStub{stub: &stub{name: "opt", log: el, initErr: errors.New("fail")}})
	r.Register(mkStub("b", el))

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	started := r.StartedComponents()
	names := make([]string, len(started))
	for i, c := range started {
		names[i] = c.Name()
	}

	for _, n := range names {
		if n == "opt" {
			t.Fatal("StartedComponents should not include failed optional component")
		}
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 started components, got %d: %v", len(names), names)
	}
}

// ---------------------------------------------------------------------------
// StartedComponents returns topo (start) order
// ---------------------------------------------------------------------------

func TestStartedComponents_SameLevelPreservesRegistrationOrder(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()

	// Two components at the same level (no deps). "slow" is registered
	// first but takes longer to Init. StartedComponents() must still
	// return [slow, fast] (registration order), not [fast, slow].
	slow := &stubSlowInit{stub: mkStub("slow", el), dur: 80 * time.Millisecond}
	fast := mkStub("fast", el)

	r.Register(slow)
	r.Register(fast)

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	started := r.StartedComponents()
	if len(started) != 2 {
		t.Fatalf("expected 2, got %d", len(started))
	}
	if started[0].Name() != "slow" || started[1].Name() != "fast" {
		t.Fatalf("expected [slow, fast], got [%s, %s]", started[0].Name(), started[1].Name())
	}
}

func TestStartedComponents_ReturnsStartOrder(t *testing.T) {
	r := New(nil, log.Empty())
	el := newLog()

	// b depends on a → start order must be [a, b].
	r.Register(mkStub("a", el))
	r.Register(stubWithDeps{stub: &stub{name: "b", log: el, deps: []string{"a"}}})

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	started := r.StartedComponents()
	if len(started) != 2 {
		t.Fatalf("expected 2 components, got %d", len(started))
	}
	if started[0].Name() != "a" || started[1].Name() != "b" {
		t.Fatalf("expected start order [a, b], got [%s, %s]", started[0].Name(), started[1].Name())
	}
}
