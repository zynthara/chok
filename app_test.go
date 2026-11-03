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

	"github.com/zynthara/chok/internal/conf"
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

// TestStart_UnhealthyAfterStartTimeout: the start error carries each
// failing component's error, http.health.details or not; the polls
// before it log nothing of their own.
func TestStart_UnhealthyAfterStartTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := testBase(t)
		base.App.StartTimeout = 2 * time.Second
		var logs syncBuffer
		app := New(base, Logger(slog.New(slog.NewTextHandler(&logs, nil))))
		app.Use(&recorder{name: "db", log: &eventLog{}, health: errors.New("connection refused")})
		err := app.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not healthy after app.start_timeout (2s): db: connection refused") {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(logs.String(), "chok: health check failed") {
			t.Fatalf("a poll of the start logged a failure:\n%s", logs.String())
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

// stuckCloser is a component whose Close ignores its ctx, like one that
// wraps a library without a context (a headless browser, say). It returns
// once release is closed.
type stuckCloser struct {
	name    string
	release <-chan struct{}
}

func (s stuckCloser) Name() string { return s.name }

func (s stuckCloser) Close(context.Context) error {
	<-s.release
	return nil
}

// TestStop_CloseIgnoringCtxIsAbandoned: a Close that ignores its ctx holds
// the stop no longer than app.shutdown_timeout plus closeGrace. Run then
// returns, with an error and an Error log naming the component; the
// component registered before it, closed only after the budget is spent,
// still gets its grace and finishes before Run returns.
func TestStop_CloseIgnoringCtxIsAbandoned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := testBase(t)
		base.App.ShutdownTimeout = 3 * time.Second
		var logs syncBuffer
		app := New(base, Logger(slog.New(slog.NewTextHandler(&logs, nil))))
		release := make(chan struct{})
		defer close(release)
		events := &eventLog{}
		app.Use(&recorder{name: "first", log: events}, stuckCloser{name: "engine", release: release})
		ctx, cancel := context.WithCancel(context.Background())
		done := runAsync(app, ctx)
		synctest.Wait()
		cancel()
		begin := time.Now()
		err := <-done
		closed := events.all() // what was closed by the time Run returned
		if d := time.Since(begin); d != 3*time.Second+closeGrace {
			t.Fatalf("the stop takes its budget and the grace of the abandoned Close, no more: took %s", d)
		}
		if err == nil || !strings.Contains(err.Error(), "chok: close engine did not return within the stop budget; abandoned: app.shutdown_timeout (3s)") {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(err.Error(), "close first") {
			t.Fatalf("a Close that returns within its grace is not abandoned: %v", err)
		}
		if out := logs.String(); !strings.Contains(out, `level=ERROR msg="chok: close did not return within the stop budget; abandoned" component=engine`) {
			t.Fatalf("the abandoned Close is logged at error level, naming the component:\n%s", out)
		}
		if !slices.Contains(closed, "close first") {
			t.Fatalf("the component after the abandoned one is closed before Run returns: %v", closed)
		}
	})
}

// bomb is a component whose Close panics.
type bomb struct{}

func (bomb) Close(context.Context) error { panic("boom") }

// TestClose_BeforeRunAbandonsACloseIgnoringCtx: App.Close waits for each
// Close while its ctx lasts and closeGrace longer. A Close that panics, in
// the goroutine it now runs in, is reported; one that ignores ctx is
// abandoned at the end of its grace, with an error naming it; the
// component registered before it gets its own grace and is closed before
// App.Close returns.
func TestClose_BeforeRunAbandonsACloseIgnoringCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := New(testBase(t), Logger(quiet()))
		release := make(chan struct{})
		defer close(release)
		events := &eventLog{}
		app.Use(&recorder{name: "first", log: events}, stuckCloser{name: "engine", release: release}, bomb{})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		begin := time.Now()
		err := app.Close(ctx)
		closed := events.all()
		if d := time.Since(begin); d != 2*time.Second+closeGrace {
			t.Fatalf("Close takes what its ctx allows and the abandoned Close's grace, no more: took %s", d)
		}
		for _, want := range []string{"chok: close bomb: close panicked: boom", "chok: close engine did not return within the stop budget; abandoned"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want %q in %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "close first") {
			t.Fatalf("a Close that returns within its grace is not abandoned: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the abandoned Close's error wraps why ctx ended: %v", err)
		}
		if !slices.Contains(closed, "close first") {
			t.Fatalf("the component after the abandoned one is closed before App.Close returns: %v", closed)
		}
	})
}

// TestStop_DrainDelayHolds503: through http.drain_delay /readyz and the
// health endpoint answer 503; the health report says down and lists no
// component, as the checks are not run.
func TestStop_DrainDelayHolds503(t *testing.T) {
	base := testBase(t)
	base.HTTP.Enabled = true
	base.HTTP.Addr = "127.0.0.1:0"
	base.HTTP.DrainDelay = 500 * time.Millisecond
	app := New(base, Logger(quiet()))
	app.Use(&recorder{name: "db", log: &eventLog{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	root := "http://" + app.Addr()
	for _, path := range []string{"/readyz", "/healthz"} {
		if code := get(t, root+path); code != http.StatusOK {
			t.Fatalf("%s while running = %d", path, code)
		}
	}
	cancel()
	waitFor(t, app.draining.Load, "draining")
	if code := get(t, root+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz during the drain delay = %d, want 503", code)
	}
	resp, err := http.Get(root + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var report healthReport
	err = json.UnmarshalRead(resp.Body, &report)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || report.Status != statusDown || len(report.Components) != 0 {
		t.Fatalf("healthz during the drain delay = %d %+v (%v), want 503, down and no component", resp.StatusCode, report, err)
	}
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
}

// TestStop_DrainOnlyWhenProbed: http.drain_delay is waited only when
// /readyz or the health endpoint, wherever http.health.path puts it, has
// been requested since the start — a load balancer or a Kubernetes probe
// watches — and says so at info level; with nobody probing the stop skips
// it and says so at debug level.
func TestStop_DrainOnlyWhenProbed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, probe := range []string{"", "/readyz", "/healthz", "/status"} {
			base := testBase(t)
			base.HTTP.DrainDelay = 5 * time.Second
			if probe == "/status" {
				base.HTTP.Health.Path = probe
			}
			var logs syncBuffer
			app := New(base, Logger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
			if probe != "" {
				rec := httptest.NewRecorder()
				app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, probe, nil))
				if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s = %d", probe, rec.Code)
				}
			}
			begin := time.Now()
			if err := app.stop(context.Background(), &http.Server{}, func() {}, &sync.WaitGroup{}, nil, false); err != nil {
				t.Fatal(err)
			}
			probed := probe != ""
			want := time.Duration(0)
			if probed {
				want = 5 * time.Second
			}
			if d := time.Since(begin); d != want {
				t.Errorf("probe %q: the stop took %s, want %s", probe, d, want)
			}
			if skipped := strings.Contains(logs.String(), "chok: drain skipped: neither /readyz nor the health endpoint was requested"); skipped == probed {
				t.Errorf("probe %q: the skip logged: %v\n%s", probe, skipped, logs.String())
			}
			if drained := strings.Contains(logs.String(), "chok: draining before the HTTP server stops"); drained != probed {
				t.Errorf("probe %q: the drain logged: %v\n%s", probe, drained, logs.String())
			}
		}
	})
}

// TestStop_UnprobedRunSkipsDrain: a Run nobody probed stops without
// waiting out the drain delay.
func TestStop_UnprobedRunSkipsDrain(t *testing.T) {
	base := testBase(t)
	base.HTTP.Enabled = true
	base.HTTP.Addr = "127.0.0.1:0"
	base.HTTP.DrainDelay = time.Hour
	app := New(base, Logger(quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	if code := get(t, "http://"+app.Addr()+"/livez"); code != http.StatusOK {
		t.Fatalf("livez = %d", code) // another probe is not a readiness probe
	}
	cancel()
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
}

// TestStop_FastCutsTheDrainAndCapsTheRest: a stop turned fast midway — a
// second signal — ends the drain at once and caps what is left of the
// budget at 5s.
func TestStop_FastCutsTheDrainAndCapsTheRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, stubborn := range []bool{false, true} {
			base := testBase(t)
			base.HTTP.DrainDelay = time.Hour
			base.App.ShutdownTimeout = 2 * time.Hour
			app := New(base, Logger(quiet()))
			app.probed.Store(true)
			release := make(chan struct{})
			var starters sync.WaitGroup
			if stubborn {
				starters.Go(func() { <-release }) // a Starter that ignores its ctx
			}
			done := make(chan error, 1)
			begin := time.Now()
			go func() {
				done <- app.stop(context.Background(), &http.Server{}, func() {}, &starters, nil, false)
			}()
			time.Sleep(time.Minute) // into the drain
			synctest.Wait()
			if !app.stopFast() || app.stopFast() {
				t.Fatal("stopFast reports the first change only")
			}
			err := <-done
			close(release)
			took := time.Since(begin)
			switch {
			case !stubborn && (err != nil || took != time.Minute):
				t.Errorf("the drain did not end at once: %s, %v", took, err)
			case stubborn && took != time.Minute+5*time.Second:
				t.Errorf("the stop took %s; want the minute it drained plus the 5s cap", took)
			case stubborn && (err == nil || !strings.Contains(err.Error(), "components still running after the fast stop's budget (5s)")):
				t.Errorf("got %v", err)
			}
		}
	})
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

// countingInstrumenter counts the requests its middleware sees.
type countingInstrumenter struct{ n *atomic.Int32 }

func (c countingInstrumenter) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.n.Add(1)
			next.ServeHTTP(w, r)
		})
	}
}

// TestUse_InstrumenterAfterHandlerPanics: the first Handler call fixes the
// middleware stack, so an Instrumenter registered after it would never see
// a request; Use panics rather than leave it out silently. A component of
// another kind is still registered after Handler.
func TestUse_InstrumenterAfterHandlerPanics(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	app.Handler()
	m := &mounter{}
	app.Use(m)
	if !m.mounted.Load() || len(app.components) != 1 {
		t.Fatal("a component that is not an Instrumenter is still registered after Handler")
	}
	defer func() {
		msg, _ := recover().(string)
		if !strings.Contains(msg, "chok: Use of an Instrumenter (chok.countingInstrumenter) after Handler") ||
			!strings.Contains(msg, "register it before calling Handler or Run") {
			t.Fatalf("Use of an Instrumenter after Handler must panic and say what to do, got %q", msg)
		}
	}()
	app.Use(countingInstrumenter{n: new(atomic.Int32)})
}

// TestUse_InstrumenterBeforeHandlerSeesRequests: registered before
// Handler, an Instrumenter's middleware is in the stack.
func TestUse_InstrumenterBeforeHandlerSeesRequests(t *testing.T) {
	var n atomic.Int32
	app := New(testBase(t), Logger(quiet()))
	app.Use(countingInstrumenter{n: &n})
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK || n.Load() != 1 {
		t.Fatalf("livez = %d; the Instrumenter saw %d requests, want 1", rec.Code, n.Load())
	}
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
		report.Components[0].Component != "cache" || report.Components[0].Status != statusDown ||
		report.Components[1].Status != statusUp {
		t.Fatalf("report: %+v", report)
	}
}

// TestHealth_DetailsShowTheErrors: a failing component turns the health
// endpoint 503 and its entry down, but its error, which can hold an
// address or part of a DSN, is in the body only with http.health.details;
// either way the failure is logged at warn with its error and how long
// the check took, and a passing check logs nothing. /readyz runs no
// checks and never names a component.
func TestHealth_DetailsShowTheErrors(t *testing.T) {
	const cause = "dial tcp 10.0.0.7:6379: connection refused"
	for _, details := range []bool{false, true} {
		base := testBase(t)
		base.HTTP.Health.Details = details
		var logs syncBuffer
		app := New(base, Logger(slog.New(slog.NewTextHandler(&logs, nil))))
		app.Use(&recorder{name: "db", log: &eventLog{}}, &recorder{name: "cache", log: &eventLog{}, health: errors.New(cause)})
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var report healthReport
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusServiceUnavailable || report.Status != statusDown || len(report.Components) != 2 ||
			report.Components[0].Component != "cache" || report.Components[0].Status != statusDown || report.Components[1].Status != statusUp {
			t.Fatalf("details=%v: %d %s", details, rec.Code, rec.Body)
		}
		if shown := strings.Contains(rec.Body.String(), `"error":`) || strings.Contains(rec.Body.String(), "10.0.0.7"); shown != details {
			t.Errorf("details=%v: the body shows the error: %v\n%s", details, shown, rec.Body)
		}
		if details && report.Components[0].Error != cause {
			t.Errorf("details: the error is %q, want %q", report.Components[0].Error, cause)
		}
		out := logs.String()
		for _, want := range []string{`level=WARN msg="chok: health check failed"`, "component=cache", `error="` + cause + `"`, "elapsed="} {
			if !strings.Contains(out, want) {
				t.Errorf("details=%v: the log lacks %s:\n%s", details, want, out)
			}
		}
		if strings.Contains(out, "component=db") {
			t.Errorf("details=%v: a passing check is logged:\n%s", details, out)
		}
		rec = httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if body := strings.TrimSpace(rec.Body.String()); body != `{"status":"down"}` {
			t.Errorf("details=%v: /readyz = %s, want the status alone", details, body)
		}
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

func TestHealth_PathFromConfig(t *testing.T) {
	base := testBase(t)
	base.HTTP.Health.Path = "/status"
	app := New(base, Logger(quiet()))
	for path, want := range map[string]int{"/status": http.StatusOK, "/healthz": http.StatusNotFound, "/livez": http.StatusOK} {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("%s = %d, want %d", path, rec.Code, want)
		}
	}
}
