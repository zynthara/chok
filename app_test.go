package chok

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zynthara/chok/v3/internal/conf"
)

// testBase is a defaulted Base that does not listen.
func testBase(t *testing.T) Base {
	t.Helper()
	var b Base
	if err := conf.Defaults(&b); err != nil {
		t.Fatal(err)
	}
	b.HTTP.Enabled = false
	b.HTTP.DrainDelay = 0
	return b
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// recorder is a component that records what the App did to it.
type recorder struct {
	name    string
	log     *eventLog
	health  error
	startFn func(ctx context.Context) error
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Start(ctx context.Context) error {
	r.log.add("start " + r.name)
	if r.startFn != nil {
		return r.startFn(ctx)
	}
	<-ctx.Done()
	r.log.add("stopped " + r.name)
	return nil
}

func (r *recorder) Close(ctx context.Context) error {
	r.log.add("close " + r.name)
	return nil
}

func (r *recorder) Health(ctx context.Context) error { return r.health }

func runAsync(app *App, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	return done
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func await(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func TestRun_CtxCancelReturnsNil(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	app.Use(&recorder{name: "a", log: &eventLog{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	cancel()
	if err := await(t, done); err != nil {
		t.Fatalf("a canceled context is an orderly stop, got %v", err)
	}
}

func TestRun_DeadlineExceededReturnsError(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := app.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an expired deadline is an error, got %v", err)
	}
}

func TestRun_SingleUse(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = app.Run(ctx)
	if err := app.Run(ctx); err == nil || !strings.Contains(err.Error(), "runs once") {
		t.Fatalf("second Run must fail, got %v", err)
	}
}

func TestRun_LifecycleOrder(t *testing.T) {
	log := &eventLog{}
	app := New(testBase(t), Logger(quiet()))
	app.Use(&recorder{name: "a", log: log}, &recorder{name: "b", log: log}, &recorder{name: "c", log: log})
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	cancel()
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	ev := log.all()
	var closes []string
	for _, e := range ev {
		if strings.HasPrefix(e, "close ") {
			closes = append(closes, e)
		}
	}
	if !slices.Equal(closes, []string{"close c", "close b", "close a"}) {
		t.Fatalf("Close runs in reverse Use order: %v", ev)
	}
	last := slices.Index(ev, "close c")
	for _, s := range []string{"stopped a", "stopped b", "stopped c"} {
		if i := slices.Index(ev, s); i < 0 || i > last {
			t.Fatalf("every Starter returns before the first Close: %v", ev)
		}
	}
}

func TestStart_FailurePreventsListening(t *testing.T) {
	base := testBase(t)
	base.HTTP.Enabled = true
	base.HTTP.Addr = "127.0.0.1:0"
	app := New(base, Logger(quiet()))
	failedStart := make(chan struct{})
	log := &eventLog{}
	c := &recorder{name: "broken", log: log, health: errors.New("not yet"), startFn: func(ctx context.Context) error {
		close(failedStart)
		return errors.New("cannot connect")
	}}
	app.Use(c)
	err := app.Run(context.Background())
	<-failedStart
	if err == nil || !strings.Contains(err.Error(), "broken: cannot connect") {
		t.Fatalf("a failed Start must end Run, got %v", err)
	}
	if app.Addr() != "" {
		t.Fatalf("the server must never listen, got %s", app.Addr())
	}
	if !slices.Contains(log.all(), "close broken") {
		t.Fatalf("components are still closed after a failed start: %v", log.all())
	}
}

func TestStart_UnhealthyAfterStartTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := testBase(t)
		base.App.StartTimeout = 2 * time.Second
		app := New(base, Logger(quiet()))
		app.Use(&recorder{name: "db", log: &eventLog{}, health: errors.New("connection refused")})
		err := app.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not healthy after app.start_timeout (2s): db: connection refused") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestStop_StarterIgnoringCtxHitsBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := testBase(t)
		base.App.ShutdownTimeout = 3 * time.Second
		app := New(base, Logger(quiet()))
		release := make(chan struct{})
		defer close(release)
		app.Use(&recorder{name: "stubborn", log: &eventLog{}, startFn: func(ctx context.Context) error {
			<-release // ignores ctx
			return nil
		}})
		ctx, cancel := context.WithCancel(context.Background())
		done := runAsync(app, ctx)
		synctest.Wait()
		cancel()
		begin := time.Now()
		err := <-done
		if err == nil || !strings.Contains(err.Error(), "still running after app.shutdown_timeout (3s)") {
			t.Fatalf("got %v", err)
		}
		if d := time.Since(begin); d != 3*time.Second {
			t.Fatalf("the stop phase waits exactly its budget, waited %s", d)
		}
	})
}

func TestStop_FastStopCapsBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := testBase(t)
		base.App.ShutdownTimeout = time.Minute
		app := New(base, Logger(quiet()))
		release := make(chan struct{})
		defer close(release)
		app.Use(&recorder{name: "stubborn", log: &eventLog{}, startFn: func(ctx context.Context) error {
			<-release
			return nil
		}})
		ctx, cancel := context.WithCancelCause(context.Background())
		done := runAsync(app, ctx)
		synctest.Wait()
		cancel(errFastStop)
		begin := time.Now()
		if err := <-done; err == nil {
			t.Fatal("a Starter that outlives the budget is an error")
		}
		if d := time.Since(begin); d != 5*time.Second {
			t.Fatalf("a fast stop is capped at 5s, took %s", d)
		}
	})
}

func TestStop_DrainDelayHoldsReadyz503(t *testing.T) {
	base := testBase(t)
	base.HTTP.Enabled = true
	base.HTTP.Addr = "127.0.0.1:0"
	base.HTTP.DrainDelay = 300 * time.Millisecond
	app := New(base, Logger(quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	url := "http://" + app.Addr() + "/readyz"
	if code := get(t, url); code != http.StatusOK {
		t.Fatalf("readyz while running = %d", code)
	}
	cancel()
	waitFor(t, app.draining.Load, "draining")
	if code := get(t, url); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz during the drain delay = %d, want 503", code)
	}
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestStart_FailureAfterReadyStopsApp(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	crash := make(chan struct{})
	app.Use(&recorder{name: "worker", log: &eventLog{}, startFn: func(ctx context.Context) error {
		select {
		case <-crash:
			return errors.New("queue lost")
		case <-ctx.Done():
			return nil
		}
	}})
	done := runAsync(app, context.Background())
	waitFor(t, app.ready.Load, "ready")
	close(crash)
	if err := await(t, done); err == nil || !strings.Contains(err.Error(), "worker: queue lost") {
		t.Fatalf("got %v", err)
	}
}

func TestUse_IgnoresValueWithoutLifecycle(t *testing.T) {
	var buf bytes.Buffer
	app := New(testBase(t), Logger(slog.New(slog.NewTextHandler(&buf, nil))))
	app.Use(struct{}{})
	if len(app.components) != 0 || !strings.Contains(buf.String(), "implements none of") {
		t.Fatalf("components=%d log=%s", len(app.components), buf.String())
	}
}

func TestUse_AfterRunPanics(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = app.Run(ctx)
	defer func() {
		if recover() == nil {
			t.Fatal("Use after Run must panic")
		}
	}()
	app.Use(&recorder{name: "late", log: &eventLog{}})
}

type mounter struct{ mounted atomic.Bool }

func (m *mounter) Mount(r Router) {
	m.mounted.Store(true)
	r.Handle(http.MethodGet, "/battery", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
}

func TestUse_MountsImmediately(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	m := &mounter{}
	app.Use(m)
	if !m.mounted.Load() {
		t.Fatal("Use must call Mount right away")
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/battery", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("battery route = %d", rec.Code)
	}
}

func TestHealth_AggregatesComponents(t *testing.T) {
	app := New(testBase(t), Logger(quiet()), Version("1.2.3"))
	app.Use(&recorder{name: "db", log: &eventLog{}}, &recorder{name: "cache", log: &eventLog{}, health: errors.New("redis down")})
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("one failing component makes /healthz 503, got %d", rec.Code)
	}
	var report healthReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != statusDown || report.Version != "1.2.3" || len(report.Components) != 2 ||
		report.Components[0].Component != "cache" || report.Components[0].Error != "redis down" ||
		report.Components[1].Status != statusUp {
		t.Fatalf("report: %+v", report)
	}
}

func TestHealth_ProbesBeforeAndAfterStart(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	h := app.Handler()
	probe := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if probe("/livez") != http.StatusOK || probe("/readyz") != http.StatusServiceUnavailable {
		t.Fatal("before Run: alive but not ready")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	if probe("/readyz") != http.StatusOK || probe("/healthz") != http.StatusOK {
		t.Fatal("after start: ready and healthy")
	}
	cancel()
	_ = await(t, done)
}

func TestHealth_DisabledMountsNothing(t *testing.T) {
	base := testBase(t)
	base.HTTP.Health.Enabled = false
	app := New(base, Logger(quiet()))
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestLogger_InjectedIsNotInstalledOrClosed(t *testing.T) {
	before := slog.Default()
	l := quiet()
	app := New(testBase(t), Logger(l))
	if slog.Default() != before {
		t.Fatal("an injected logger must not become slog.Default")
	}
	if app.Logger() != l || app.level != nil || len(app.closers) != 0 {
		t.Fatal("the App neither owns nor re-levels an injected logger")
	}
}

func TestNew_BuildsLoggerFromConfigAndInstallsDefault(t *testing.T) {
	before := slog.Default()
	t.Cleanup(func() { slog.SetDefault(before) })
	base := testBase(t)
	path := t.TempDir() + "/app.log"
	base.Log.Output = []string{path}
	base.Log.Level = "warn"
	app := New(base)
	if slog.Default() != app.Logger() {
		t.Fatal("New installs the logger it built as slog.Default")
	}
	slog.Info("dropped")
	slog.Warn("kept")
	app.closeLogs()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b := string(raw); strings.Contains(b, "dropped") || !strings.Contains(b, `"msg":"kept"`) {
		t.Fatalf("level and format come from base.Log: %s", b)
	}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
