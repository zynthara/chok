package jobs

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

// The battery's tests through its public face: New, the options, Enqueue,
// Start / Close and Stats, on the lane's database. v2's scheduler and
// outbox module tests land here under v3 names; the report maps them.

// fastConfig sweeps every 10ms, so a test sees a delivery quickly, and
// tries a failed message again 10ms later, without ever parking it: the
// tests of parking set MaxAttempts themselves.
func fastConfig() Config {
	return Config{
		Enabled: true, PollInterval: 10 * time.Millisecond,
		RetryBackoff: 10 * time.Millisecond, MaxBackoff: 10 * time.Millisecond, MaxAttempts: 1000,
	}
}

func newJobs(t *testing.T, d *db.DB, cfg Config, opts ...Option) *Jobs {
	t.Helper()
	j, err := New(cfg, d, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// run starts j until the test ends; stop ends it and returns what Start
// returned.
func run(t *testing.T, j *Jobs) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- j.Start(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() { cancel(); err = <-done })
		return err
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("jobs stopped with %v", err)
		}
	})
	return stop
}

// eventually polls cond until it holds or the deadline passes: CI's race
// detector is slow, so the deadline is generous and nothing sleeps a
// fixed time hoping for the best.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// enqueueTx commits one Enqueue in its own transaction.
func enqueueTx(t *testing.T, d *db.DB, j *Jobs, topic string, payload any) {
	t.Helper()
	err := d.RunInTx(t.Context(), func(txCtx context.Context) error { return j.Enqueue(txCtx, topic, payload) })
	if err != nil {
		t.Fatalf("enqueue to %s: %v", topic, err)
	}
	if dbtest.Driver() == "mysql" {
		time.Sleep(time.Millisecond) // distinct created_at under datetime(3)
	}
}

// inbox is a worker function that records what it got.
type inbox struct {
	mu   sync.Mutex
	msgs []Message[jsontext.Value]
}

func (in *inbox) handle(_ context.Context, m Message[jsontext.Value]) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.msgs = append(in.msgs, m)
	return nil
}

func (in *inbox) payloads() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := make([]string, len(in.msgs))
	for i, m := range in.msgs {
		out[i] = string(m.Payload)
	}
	return out
}

func (in *inbox) count() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.msgs)
}

func noop(context.Context) error { return nil }

// TestNew_RefusesDuplicateNames is v2's TestRegister_DuplicateName (two
// jobs of one name) and TestModule_DuplicateRelayNameFailsInit (two
// relays of one name, now two workers of one topic).
func TestNew_RefusesDuplicateNames(t *testing.T) {
	d := dbtest.Open(t)
	_, err := New(fastConfig(), d, Periodic("j1", "@every 1h", noop, OverlapSkip), Periodic("j1", "@every 2h", noop, OverlapSkip))
	if err == nil || !strings.Contains(err.Error(), `two periodic jobs are named "j1"`) {
		t.Fatalf("duplicate periodic jobs: %v", err)
	}
	in := &inbox{}
	_, err = New(fastConfig(), d, Worker("dup", in.handle), Worker("dup", in.handle))
	if err == nil || !strings.Contains(err.Error(), `two workers for topic "dup"`) {
		t.Fatalf("duplicate workers: %v", err)
	}
	// A periodic job and a worker may share a name: Stats lists them apart.
	newJobs(t, d, fastConfig(), Periodic("same", "@every 1h", noop, OverlapSkip), Worker("same", in.handle))
}

// TestNew_RefusesBadSpec is v2's TestRegister_InvalidSpec_NoStateWritten:
// the spec is checked at New, and a refused New registers nothing.
func TestNew_RefusesBadSpec(t *testing.T) {
	d := dbtest.Open(t)
	for _, spec := range []string{"not-a-cron-spec", "* * * *", "61 * * * *", "@every nope", "@sometimes", "CRON_TZ=Mars/Olympus 0 * * * *", "CRON_TZ=UTC"} {
		_, err := New(fastConfig(), d, Periodic("bad", spec, noop, OverlapSkip))
		if err == nil || !strings.Contains(err.Error(), `periodic job "bad": spec`) {
			t.Errorf("spec %q: %v", spec, err)
		}
	}
	// Nothing was registered: the schema is still free for a good New.
	newJobs(t, d, fastConfig(), Periodic("good", "0 3 * * *", noop, OverlapSkip))
}

func TestNew_RefusesBadOptions(t *testing.T) {
	d := dbtest.Open(t)
	in := &inbox{}
	if _, err := New(fastConfig(), nil); err == nil || !strings.Contains(err.Error(), "nil *db.DB") {
		t.Errorf("nil db: %v", err)
	}
	for _, c := range []struct {
		opt  Option
		want string
	}{
		{Periodic("", "@hourly", noop, OverlapSkip), "needs a name"},
		{Periodic("nil", "@hourly", nil, OverlapSkip), `periodic job "nil" has a nil function`},
		{Periodic("odd", "@hourly", noop, Overlap(7)), "Overlap(7) is not an Overlap"},
		{Worker("", in.handle), "a topic must be 1 to 200 bytes"},
		{Worker(strings.Repeat("t", 201), in.handle), "a topic must be 1 to 200 bytes"},
		{Worker[jsontext.Value]("nil", nil), `worker "nil" has a nil function`},
		{Observe(nil), "Observe with a nil function"},
	} {
		if _, err := New(fastConfig(), d, c.opt); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	if _, err := New(Config{Enabled: true, BatchSize: 20_000}, d); err == nil || !strings.Contains(err.Error(), "batch_size") {
		t.Errorf("a bad config: %v", err)
	}
	newJobs(t, d, fastConfig(), Worker(strings.Repeat("t", 200), in.handle))
}

// TestConfig_Validate is v2's TestOptions_Validate and, for stop_budget,
// TestModule_InvalidStopBudget_FailsStartup.
func TestConfig_Validate(t *testing.T) {
	base := func() Config {
		return Config{
			Enabled: true, PollInterval: time.Second, BatchSize: 100, SettleWindow: 30 * time.Second,
			CleanupInterval: time.Hour, StopBudget: 15 * time.Second,
		}
	}
	if c := base(); c.Validate() != nil {
		t.Fatalf("valid config refused: %v", c.Validate())
	}
	disabled := Config{BatchSize: -1, StopBudget: -1}
	if err := disabled.Validate(); err != nil {
		t.Fatalf("a disabled section runs nothing and is not checked: %v", err)
	}
	cases := map[string]func(*Config){
		"poll_interval":    func(c *Config) { c.PollInterval = 0 },
		"batch_size low":   func(c *Config) { c.BatchSize = 0 },
		"batch_size high":  func(c *Config) { c.BatchSize = 10_001 },
		"settle_window":    func(c *Config) { c.SettleWindow = 0 },
		"retention":        func(c *Config) { c.Retention = -time.Second },
		"cleanup_interval": func(c *Config) { c.CleanupInterval = 0 },
		"stop_budget":      func(c *Config) { c.StopBudget = -time.Second },
		"max_attempts":     func(c *Config) { c.MaxAttempts = -1 },
		"retry_backoff":    func(c *Config) { c.RetryBackoff = -time.Second },
		"max_backoff":      func(c *Config) { c.MaxBackoff = -time.Second },
		// A zero max_backoff takes its default (5m), which a longer
		// first wait exceeds.
		"retry_backoff above max_backoff": func(c *Config) { c.RetryBackoff = 10 * time.Minute },
	}
	for name, mutate := range cases {
		c := base()
		mutate(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), strings.Fields(name)[0]) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestConfig_LoadsWithDefaults(t *testing.T) {
	type appConfig struct {
		chok.Base
		Jobs Config `yaml:"jobs"`
	}
	// Retention's default comes with the file: a week. (A Config built
	// in code keeps everything: TestNew_DefaultsZeroFields.)
	cfg, err := chok.Load[appConfig]("jbt", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Enabled: true, PollInterval: time.Second, BatchSize: 100, SettleWindow: 30 * time.Second,
		MaxAttempts: 10, RetryBackoff: time.Second, MaxBackoff: 5 * time.Minute,
		Retention: 168 * time.Hour, CleanupInterval: time.Hour, StopBudget: 15 * time.Second,
	}
	if cfg.Jobs != want {
		t.Fatalf("loaded %+v, want %+v", cfg.Jobs, want)
	}
	// A bad value fails the load and names its key.
	t.Setenv("JBT_JOBS_STOP_BUDGET", "-1s")
	if _, err := chok.Load[appConfig]("jbt", chok.Args(nil)); err == nil || !strings.Contains(err.Error(), "stop_budget") {
		t.Fatalf("a negative stop_budget: %v", err)
	}
}

func TestNew_DefaultsZeroFields(t *testing.T) {
	j := newJobs(t, dbtest.Open(t), Config{Enabled: true})
	// Retention has no zero-value default: built in code, 0 keeps the
	// messages forever.
	want := Config{
		Enabled: true, PollInterval: time.Second, BatchSize: 100, SettleWindow: 30 * time.Second,
		MaxAttempts: 10, RetryBackoff: time.Second, MaxBackoff: 5 * time.Minute,
		CleanupInterval: time.Hour, StopBudget: 15 * time.Second,
	}
	if j.cfg != want {
		t.Fatalf("defaults in code: %+v", j.cfg)
	}
	if j.Name() != "jobs" || j.Health(t.Context()) != nil {
		t.Fatal("Name is jobs and Health is nil")
	}
}

// TestEnqueue_OutsideTxIs500: ErrOutsideTx outside a transaction, and
// inside another handle's; a handler that returns it answers the 500
// envelope without the cause.
func TestEnqueue_OutsideTxIs500(t *testing.T) {
	d := dbtest.Open(t)
	j := newJobs(t, d, Config{})
	err := j.Enqueue(t.Context(), "orders", map[string]int{"n": 1})
	if !errors.Is(err, ErrOutsideTx) {
		t.Fatalf("outside a transaction: %v", err)
	}
	other := dbtest.Open(t)
	err = other.RunInTx(t.Context(), func(txCtx context.Context) error { return j.Enqueue(txCtx, "orders", 1) })
	if !errors.Is(err, ErrOutsideTx) {
		t.Fatalf("in another handle's transaction: %v", err)
	}
	wrapped := fmt.Errorf("place order: %w", err)
	if !errors.Is(wrapped, ErrOutsideTx) || !errors.Is(wrapped.(interface{ Unwrap() error }).Unwrap(), ErrOutsideTx) {
		t.Fatal("errors.Is through a wrap")
	}

	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		chok.POST(app.Router(), "/orders", func(ctx context.Context, _ *chok.None) (chok.None, error) {
			return chok.None{}, j.Enqueue(ctx, "orders", 1)
		})
	})
	var body struct {
		Code      int    `json:"code"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if code := c.JSON(http.MethodPost, "/orders", nil, &body); code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", code)
	}
	if body.Code != 500 || body.Reason != "InternalError" || body.Message != "internal server error" || body.RequestID == "" {
		t.Fatalf("envelope %+v", body)
	}
	// Outside an App, chok.WriteError renders the same 500.
	rec := httptest.NewRecorder()
	chok.WriteError(rec, httptest.NewRequest(http.MethodPost, "/", nil), wrapped)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"reason":"InternalError"`) || strings.Contains(rec.Body.String(), "RunInTx") {
		t.Fatalf("WriteError: %d %s", rec.Code, rec.Body)
	}
}

// TestEnqueue_RollbackLeavesNoMessage: the message lives and dies with the
// transaction; the worker sees only what committed.
func TestEnqueue_RollbackLeavesNoMessage(t *testing.T) {
	d := dbtest.Open(t)
	in := &inbox{}
	j := newJobs(t, d, fastConfig(), Worker("orders", in.handle))
	run(t, j)
	rollback := errors.New("the order failed")
	err := d.RunInTx(t.Context(), func(txCtx context.Context) error {
		if err := j.Enqueue(txCtx, "orders", "rolled-back"); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var n int64
	if err := d.Unsafe(t.Context()).Table("chok_jobs_messages").Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("%d messages after the rollback (%v), want none", n, err)
	}
	enqueueTx(t, d, j, "orders", "committed")
	eventually(t, "the committed message", func() bool { return in.count() == 1 })
	// One sweep later the rolled-back message has still not appeared.
	runs := j.Stats().Workers[0].Runs
	eventually(t, "another sweep", func() bool { return j.Stats().Workers[0].Runs > runs+1 })
	if got := in.payloads(); !slices.Equal(got, []string{`"committed"`}) {
		t.Fatalf("delivered %v", got)
	}
}

// TestEnqueue_EncodesThePayloadAsJSON: the worker gets json/v2's encoding
// of the value; a value json/v2 cannot encode is refused at Enqueue.
func TestEnqueue_EncodesThePayloadAsJSON(t *testing.T) {
	d := dbtest.Open(t)
	in := &inbox{}
	j := newJobs(t, d, fastConfig(), Worker("orders", in.handle))
	run(t, j)
	type orderPlaced struct {
		OrderID string   `json:"order_id"`
		Items   []string `json:"items"`
	}
	enqueueTx(t, d, j, "orders", orderPlaced{OrderID: "ord_1"})
	eventually(t, "the delivery", func() bool { return in.count() == 1 })
	m := in.msgs[0]
	if string(m.Payload) != `{"order_id":"ord_1","items":[]}` || m.Topic != "orders" || m.ID == 0 {
		t.Fatalf("message %+v (%s)", m, m.Payload)
	}
	if m.CreatedAt.Location() != time.UTC || time.Since(m.CreatedAt) > time.Minute || time.Since(m.CreatedAt) < -time.Minute {
		t.Fatalf("created_at %v", m.CreatedAt)
	}
	var back orderPlaced
	if err := json.Unmarshal(m.Payload, &back); err != nil || back.OrderID != "ord_1" {
		t.Fatalf("decode %+v: %v", back, err)
	}
	err := d.RunInTx(t.Context(), func(txCtx context.Context) error {
		return j.Enqueue(txCtx, "orders", struct{ Wait time.Duration }{time.Second})
	})
	if err == nil || !strings.Contains(err.Error(), "encode the payload") {
		t.Fatalf("a payload json/v2 cannot encode: %v", err)
	}
}

// TestWorker_EnqueueRoundTripAndDelivery is v2's
// TestModule_EnqueueRoundTripAndDelivery: Start runs the worker, which
// gets what Enqueue committed, in order, and Stats counts it.
func TestWorker_EnqueueRoundTripAndDelivery(t *testing.T) {
	d := dbtest.Open(t)
	in := &inbox{}
	j := newJobs(t, d, fastConfig(), Worker("orders", in.handle))
	run(t, j)
	for i := range 3 {
		enqueueTx(t, d, j, "orders", fmt.Sprintf("o%d", i+1))
	}
	eventually(t, "three deliveries", func() bool { return in.count() == 3 })
	if got := in.payloads(); !slices.Equal(got, []string{`"o1"`, `"o2"`, `"o3"`}) {
		t.Fatalf("delivered %v", got)
	}
	// The sweep that delivered them is counted when it ends.
	eventually(t, "a finished sweep", func() bool { return j.Stats().Workers[0].Runs > 0 })
	st := j.Stats().Workers
	if len(st) != 1 || st[0].Name != "orders" || st[0].Spec != "@every 10ms" || st[0].Messages != 3 || st[0].Failures != 0 || st[0].Next.IsZero() {
		t.Fatalf("worker stats %+v", st)
	}
}

// TestWorker_FailureBlocksOnlyItsTopic: head-of-line — a message the
// function keeps failing on holds back the messages behind it in its
// topic, never the other topics, and nothing is skipped once it heals.
func TestWorker_FailureBlocksOnlyItsTopic(t *testing.T) {
	d := dbtest.Open(t)
	var healed atomic.Bool
	a, b := &inbox{}, &inbox{}
	poison := errors.New("poison")
	j := newJobs(t, d, fastConfig(),
		Worker("a", func(ctx context.Context, m Message[jsontext.Value]) error {
			if string(m.Payload) == `"a1"` && !healed.Load() {
				return poison
			}
			return a.handle(ctx, m)
		}),
		Worker("b", b.handle),
	)
	run(t, j)
	enqueueTx(t, d, j, "a", "a1")
	enqueueTx(t, d, j, "a", "a2")
	enqueueTx(t, d, j, "b", "b1")
	enqueueTx(t, d, j, "b", "b2")
	eventually(t, "topic b flowing", func() bool { return b.count() == 2 })
	eventually(t, "a few failed sweeps of a", func() bool { return j.Stats().Workers[0].Failures >= 3 })
	if a.count() != 0 {
		t.Fatalf("a2 passed the failing a1: %v", a.payloads())
	}
	if st := j.Stats().Workers[0]; !strings.Contains(st.LastError, "poison") || st.Name != "a" {
		t.Fatalf("worker a stats %+v", st)
	}
	healed.Store(true)
	eventually(t, "topic a after healing", func() bool { return a.count() == 2 })
	if got := a.payloads(); !slices.Equal(got, []string{`"a1"`, `"a2"`}) {
		t.Fatalf("delivered %v", got)
	}
}

// TestWorker_DeliversAtLeastOnce_HandlersDedupeByID documents why a
// worker function must be idempotent. A message is delivered again (1)
// after the function fails on it, even if it had done its work before
// failing, and (2) after a crash, when it is younger than the settle
// window. A graceful restart does not deliver it again: the stop saved
// what was delivered. The redeliveries carry the same Message.ID, which
// is what an idempotent function dedupes on; Message.Attempt counts the
// failed deliveries before.
func TestWorker_DeliversAtLeastOnce_HandlersDedupeByID(t *testing.T) {
	cfg := dbtest.Config(t)
	d1 := dbtest.OpenConfig(t, cfg)

	var mu sync.Mutex
	calls := map[uint64]int{} // deliveries per message
	done := map[uint64]int{}  // effects per message: must stay at 1
	var attempts []int        // Message.Attempt of m1's deliveries
	failOnce := true
	worker := func(_ context.Context, m Message[jsontext.Value]) error {
		mu.Lock()
		defer mu.Unlock()
		calls[m.ID]++
		if done[m.ID] == 0 { // the dedupe: work done once per Message.ID
			done[m.ID]++
		}
		if string(m.Payload) == `"m1"` {
			attempts = append(attempts, m.Attempt)
			if failOnce {
				failOnce = false
				return errors.New("the acknowledgement was lost") // after the work was done
			}
		}
		return nil
	}
	snapshot := func() (map[uint64]int, map[uint64]int) {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(calls), maps.Clone(done)
	}
	cfg1 := fastConfig()
	cfg1.SettleWindow = time.Hour // nothing settles: the watermark passes no message
	j1 := newJobs(t, d1, cfg1, Worker("orders", worker))
	stop1 := run(t, j1)
	enqueueTx(t, d1, j1, "orders", "m1")
	enqueueTx(t, d1, j1, "orders", "m2")
	eventually(t, "m1 twice and m2 once", func() bool {
		c, _ := snapshot()
		return len(c) == 2 && c[1] == 2 && c[2] == 1
	})
	if err := stop1(); err != nil {
		t.Fatal(err)
	}

	// A graceful restart: another process on the same database. It has
	// no watermark to go by, but it reads back what the first one
	// delivered, so m3 comes and m1 and m2 do not come again.
	d2 := dbtest.OpenConfig(t, cfg)
	j2 := newJobs(t, d2, cfg1, Worker("orders", worker))
	stop2 := run(t, j2)
	enqueueTx(t, d2, j2, "orders", "m3")
	eventually(t, "m3 after the graceful restart", func() bool { c, _ := snapshot(); return c[3] == 1 })
	if err := stop2(); err != nil {
		t.Fatal(err)
	}
	if c, _ := snapshot(); c[1] != 2 || c[2] != 1 {
		t.Fatalf("deliveries after the graceful restart %v: m1 and m2 came again", c)
	}

	// A crash: the process dies without stopping, so it saves nothing —
	// here the saved set is dropped. Within the settle window all three
	// come again.
	if err := d2.Unsafe(t.Context()).Where("1 = 1").Delete(&deliveredRow{}).Error; err != nil {
		t.Fatal(err)
	}
	d3 := dbtest.OpenConfig(t, cfg)
	j3 := newJobs(t, d3, cfg1, Worker("orders", worker))
	run(t, j3)
	eventually(t, "the replay after the crash", func() bool {
		c, _ := snapshot()
		return c[1] == 3 && c[2] == 2 && c[3] == 2
	})
	c, effects := snapshot()
	if effects[1] != 1 || effects[2] != 1 || effects[3] != 1 || len(effects) != 3 {
		t.Fatalf("deliveries %v, effects %v: the dedupe on Message.ID keeps one effect per message", c, effects)
	}
	// m1 failed once, so its retry was attempt 2; the replay repeats the
	// delivery it repeats, and its number.
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(attempts, []int{1, 2, 2}) {
		t.Fatalf("m1's attempts %v, want [1 2 2]", attempts)
	}
}

// TestCleanup_RunsUnderStart is v2's TestModule_CleanupJobSweeps: with a
// retention, the cleanup deletes what every worker has passed, keeping
// the watermark's own row (strict less-than).
func TestCleanup_RunsUnderStart(t *testing.T) {
	d := dbtest.Open(t)
	in := &inbox{}
	cfg := fastConfig()
	// Contract §5's delivery guarantee assumes every enqueueing transaction
	// commits within settle_window. 1ms does not hold under -race at high
	// load: on 2026-10-25 the MySQL lane once saw no delivery within 15s.
	cfg.SettleWindow, cfg.Retention, cfg.CleanupInterval = 50*time.Millisecond, time.Millisecond, 20*time.Millisecond
	j := newJobs(t, d, cfg, Worker("clean", in.handle))
	run(t, j)
	for i := range 3 {
		enqueueTx(t, d, j, "clean", i)
	}
	eventually(t, "the deliveries", func() bool { return in.count() == 3 })
	count := func() int64 {
		var n int64
		if err := d.Unsafe(t.Context()).Table("chok_jobs_messages").Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	eventually(t, "the cleanup", func() bool { return count() == 1 })
	// The run that deleted them is counted when it ends.
	eventually(t, "the cleanup's stats", func() bool { return j.Stats().Cleanup.Messages == 2 })
	if st := j.Stats().Cleanup; st.Name != "cleanup" || st.Runs == 0 || st.Failures != 0 {
		t.Fatalf("cleanup stats %+v", st)
	}
}

// TestObserve_WorkersAndCleanup: a worker's sweep is a worker event named
// by its topic that counts the messages the function took, a failed sweep
// carries its error, and the cleanup's run counts the rows it deleted;
// neither has a Next. Events go to the Observe functions alongside, not
// instead of, the Stats.
func TestObserve_WorkersAndCleanup(t *testing.T) {
	d := dbtest.Open(t)
	in := &inbox{}
	var mu sync.Mutex
	var events []Event
	observe := func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	// sum adds up Messages over the end events of one job and kind.
	sum := func(job, kind string) (n int, seen bool) {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			if ev.Job == job && ev.Kind == kind {
				n, seen = n+ev.Messages, true
			}
		}
		return n, seen
	}
	var healed atomic.Bool
	cfg := fastConfig()
	cfg.SettleWindow, cfg.Retention, cfg.CleanupInterval = 50*time.Millisecond, time.Millisecond, 20*time.Millisecond
	j := newJobs(t, d, cfg,
		Worker("orders", in.handle),
		Worker("flaky", func(context.Context, Message[jsontext.Value]) error {
			if !healed.Load() {
				return errors.New("not yet")
			}
			return nil
		}),
		Observe(observe))
	run(t, j)
	for i := range 3 {
		enqueueTx(t, d, j, "orders", i)
	}
	enqueueTx(t, d, j, "flaky", "x")
	eventually(t, "the sweeps that took the three orders", func() bool { n, _ := sum("orders", "run.finished"); return n == 3 })
	eventually(t, "a failed sweep of flaky", func() bool { _, seen := sum("flaky", "run.failed"); return seen })
	healed.Store(true)
	// Once flaky has a watermark too, the cleanup deletes all but the
	// floor watermark's own row; its runs' events add up to what it
	// deleted, as the Stats do.
	rows := func() int64 {
		var n int64
		if err := d.Unsafe(t.Context()).Table("chok_jobs_messages").Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	eventually(t, "the cleanup", func() bool { return rows() == 1 })
	eventually(t, "the cleanup's events", func() bool {
		n, _ := sum("cleanup", "run.finished")
		return n == 3 && j.Stats().Cleanup.Messages == 3
	})

	mu.Lock()
	defer mu.Unlock()
	kinds := map[string]bool{}
	for _, ev := range events {
		kinds[ev.Type+" "+ev.Kind] = true
		if !ev.Next.IsZero() || ev.StartedAt.Location() != time.UTC || ev.StartedAt.IsZero() {
			t.Fatalf("event %+v: a worker or cleanup event has no Next and starts in UTC", ev)
		}
		switch ev.Type {
		case "worker":
			if ev.Job != "orders" && ev.Job != "flaky" {
				t.Fatalf("worker event %+v", ev)
			}
			if ev.Kind == "run.failed" && (ev.Job != "flaky" || !strings.Contains(ev.Error, "not yet") || ev.Messages != 0) {
				t.Fatalf("failed sweep %+v", ev)
			}
		case "cleanup":
			if ev.Job != "cleanup" || ev.Kind == "run.failed" {
				t.Fatalf("cleanup event %+v", ev)
			}
		default:
			t.Fatalf("event of type %q: %+v", ev.Type, ev)
		}
		if ev.Kind == "run.started" && (ev.Duration != 0 || ev.Messages != 0 || ev.Error != "") {
			t.Fatalf("a start carries an outcome: %+v", ev)
		}
	}
	for _, k := range []string{"worker run.started", "worker run.finished", "worker run.failed", "cleanup run.started", "cleanup run.finished"} {
		if !kinds[k] {
			t.Fatalf("no %s event among %v", k, slices.Sorted(maps.Keys(kinds)))
		}
	}
}

// TestDisabled_StartRunsNothing is v2's TestModule_Disabled_NotVisible and
// the enqueue-only half of TestModule_RelaysRequireScheduler: with
// enabled false, Start returns at once without starting anything, and
// Enqueue still works.
func TestDisabled_StartRunsNothing(t *testing.T) {
	d := dbtest.Open(t)
	in := &inbox{}
	var ticks atomic.Int32
	j := newJobs(t, d, Config{Enabled: false, PollInterval: 10 * time.Millisecond},
		Worker("orders", in.handle),
		Periodic("tick", "* * * * * *", func(context.Context) error { ticks.Add(1); return nil }, OverlapSkip))
	done := make(chan error, 1)
	go func() { done <- j.Start(t.Context()) }() // a context that does not end before the test
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start blocked: with enabled false it starts nothing")
	}
	if j.started || len(j.sched.inFlight()) != 0 {
		t.Fatal("a disabled Start started the scheduler")
	}
	enqueueTx(t, d, j, "orders", "kept")
	var n int64
	if err := d.Unsafe(t.Context()).Table("chok_jobs_messages").Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("%d messages (%v): Enqueue works while disabled", n, err)
	}
	st := j.Stats()
	if st.Workers[0].Runs != 0 || !st.Workers[0].Next.IsZero() || st.Periodic[0].Runs != 0 || ticks.Load() != 0 || in.count() != 0 {
		t.Fatalf("something ran: %+v", st)
	}
	if err := j.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestStart_StopCancelsInFlightRuns is v2's
// TestModule_StopCancelsInFlightJobWithinBudget: stopping ends the
// context of a run in flight, and Start returns once it has — well inside
// the stop budget.
func TestStart_StopCancelsInFlightRuns(t *testing.T) {
	d := dbtest.Open(t)
	started := make(chan struct{})
	sawCancel := make(chan error, 1)
	var once sync.Once
	j := newJobs(t, d, fastConfig(), Periodic("long", "* * * * * *", func(ctx context.Context) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		sawCancel <- ctx.Err()
		return ctx.Err()
	}, OverlapSkip))
	stop := run(t, j)
	<-started // the schedule fires within a second
	begin := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Start returned %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("the stop took %s; the run returns as soon as its context ends", elapsed)
	}
	if err := <-sawCancel; !errors.Is(err, context.Canceled) {
		t.Fatalf("the run saw %v", err)
	}
	if st := j.Stats().Periodic[0]; st.Runs != 1 || st.Failures != 0 || !st.Next.IsZero() {
		t.Fatalf("a run told to stop is not a failure: %+v", st)
	}
}

// TestClose_ReturnsWithinStopBudget: a run that ignores its context
// cannot hold the stop past the budget; Close and Start both report it.
func TestClose_ReturnsWithinStopBudget(t *testing.T) {
	d := dbtest.Open(t)
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	cfg := fastConfig()
	cfg.StopBudget = 200 * time.Millisecond
	j := newJobs(t, d, cfg, Worker("stuck", func(context.Context, Message[jsontext.Value]) error {
		once.Do(func() { close(started) })
		<-release // ignores its context
		return nil
	}))
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- j.Start(ctx) }()
	enqueueTx(t, d, j, "stuck", 1)
	<-started

	begin := time.Now()
	err := j.Close(t.Context())
	elapsed := time.Since(begin)
	if err == nil || !strings.Contains(err.Error(), "stop_budget (200ms) ran out") || !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("Close: %v", err)
	}
	if elapsed < 150*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("Close returned after %s; the budget is 200ms", elapsed)
	}
	if err := <-startErr; err == nil || !strings.Contains(err.Error(), "stop_budget") {
		t.Fatalf("Start: %v", err)
	}
	if err := j.Start(ctx); err == nil {
		t.Fatal("Start after Close")
	}
	close(release)
	// The stuck run finishes now; wait for it before the database goes.
	if err := j.sched.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestApp_RunsTheJobs is v2's TestModule_PeerRegistersAtInit_CronFires:
// registered with app.Use, the jobs start with the App — the periodic job
// fires, the worker delivers what a handler enqueued — show up in
// /healthz, and stop with it.
func TestApp_RunsTheJobs(t *testing.T) {
	var ticks atomic.Int32
	in := &inbox{}
	var j *Jobs
	var d *db.DB
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		d = choktest.DB(t)
		j = MustNew(fastConfig(), d,
			Periodic("tick", "* * * * * *", func(context.Context) error { ticks.Add(1); return nil }, OverlapSkip),
			Worker("orders", in.handle))
		app.Use(d, j)
		chok.POST(app.Router(), "/orders", func(ctx context.Context, _ *chok.None) (chok.None, error) {
			return chok.None{}, d.RunInTx(ctx, func(txCtx context.Context) error { return j.Enqueue(txCtx, "orders", "o1") })
		})
	})
	if code := c.JSON(http.MethodPost, "/orders", nil, nil); code != http.StatusNoContent {
		t.Fatalf("POST /orders: %d", code)
	}
	eventually(t, "the worker", func() bool { return in.count() == 1 })
	eventually(t, "the periodic job", func() bool { return ticks.Load() > 0 })
	var health struct {
		Status     string `json:"status"`
		Components []struct {
			Component string `json:"component"`
			Status    string `json:"status"`
		} `json:"components"`
	}
	if code := c.GetJSON("/healthz", &health); code != http.StatusOK {
		t.Fatalf("/healthz: %d", code)
	}
	if !slices.ContainsFunc(health.Components, func(c struct {
		Component string `json:"component"`
		Status    string `json:"status"`
	}) bool {
		return c.Component == "jobs" && c.Status == "up"
	}) {
		t.Fatalf("/healthz: %+v", health)
	}
}

// TestStats_ListsEveryEntry: Stats names the periodic jobs in order, the
// workers by topic, and the cleanup only with a retention.
func TestStats_ListsEveryEntry(t *testing.T) {
	d := dbtest.Open(t)
	in := &inbox{}
	j := newJobs(t, d, fastConfig(),
		Periodic("b", "@hourly", noop, OverlapQueue), Periodic("a", "*/5 * * * *", noop, OverlapAllow),
		Worker("orders", in.handle))
	st := j.Stats()
	if len(st.Periodic) != 2 || st.Periodic[0].Name != "b" || st.Periodic[0].Spec != "@hourly" || st.Periodic[1].Name != "a" ||
		len(st.Workers) != 1 || st.Workers[0].Name != "orders" || st.Cleanup != (JobStats{}) {
		t.Fatalf("stats %+v", st)
	}
	cfg := fastConfig()
	cfg.Retention = 24 * time.Hour
	j2 := newJobs(t, dbtest.Open(t), cfg)
	if st := j2.Stats(); st.Cleanup.Name != "cleanup" || st.Cleanup.Spec != "@every 1h0m0s" || len(st.Periodic) != 0 || len(st.Workers) != 0 {
		t.Fatalf("stats %+v", st)
	}
	if OverlapSkip.String() != "skip" || OverlapQueue.String() != "queue" || OverlapAllow.String() != "allow" {
		t.Fatal("Overlap names")
	}
}

// TestMigrations_VersionedUsesTheLedger is v2's
// TestModule_VersionedUsesOwnedLedger: in migrate: versioned, New applies
// the jobs' SQL under the owner "jobs" in the one ledger.
func TestMigrations_VersionedUsesTheLedger(t *testing.T) {
	d := dbtest.OpenConfig(t, versionedConfig(t))
	in := &inbox{}
	j := newJobs(t, d, fastConfig(), Worker("orders", in.handle))
	sts, err := d.MigrationStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, st := range sts {
		got = append(got, st.Owner+" "+st.Version+"_"+st.Name+" "+st.State)
	}
	if !slices.Equal(got, []string{"jobs 20261025000001_create_jobs_tables applied"}) {
		t.Fatalf("ledger:\n%s", strings.Join(got, "\n"))
	}
	// The tables work.
	run(t, j)
	enqueueTx(t, d, j, "orders", "o1")
	eventually(t, "the delivery", func() bool { return in.count() == 1 })
}

// TestMigrations_OffTouchesNothing is v2's
// TestModule_MigrateOff_NoDDL_EnqueueFails: migrate: off creates no table,
// and New refuses a database without them (v2 failed later, at Enqueue).
func TestMigrations_OffTouchesNothing(t *testing.T) {
	cfg := dbtest.Config(t)
	cfg.Migrate = "off"
	d := dbtest.OpenConfig(t, cfg)
	if _, err := New(fastConfig(), d); !errors.Is(err, db.ErrNotMigrated) {
		t.Fatalf("off on a database without the jobs' tables: %v", err)
	}
	m := d.Unsafe(t.Context()).Migrator()
	for _, table := range jobsTables {
		if m.HasTable(table) {
			t.Errorf("migrate: off created %s", table)
		}
	}
}

func TestMigrations_OffRunsOnAMigratedDatabase(t *testing.T) {
	cfg := versionedConfig(t)
	newJobs(t, dbtest.OpenConfig(t, cfg), Config{})
	cfg.Migrate = "off"
	d := dbtest.OpenConfig(t, cfg)
	j := newJobs(t, d, Config{})
	enqueueTx(t, d, j, "orders", "o1")
}

func TestMigrations_AutoCreatesTheTables(t *testing.T) {
	d := dbtest.Open(t)
	newJobs(t, d, Config{})
	m := d.Unsafe(t.Context()).Migrator()
	for _, table := range jobsTables {
		if !m.HasTable(table) {
			t.Errorf("no table %s", table)
		}
	}
	if !m.HasIndex("chok_jobs_messages", "idx_chok_jobs_messages_scan") || !m.HasIndex("chok_jobs_messages", "idx_chok_jobs_messages_topic") {
		t.Fatal("the scan indexes")
	}
	// auto mode records nothing in the ledger: the tables follow the rows.
	if sts, err := d.MigrationStatus(t.Context()); err != nil || len(sts) != 1 || sts[0].State != "pending" {
		t.Fatalf("status in auto mode: %+v %v", sts, err)
	}
	// One Jobs per database handle.
	if _, err := New(Config{}, d); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("a second Jobs on the handle: %v", err)
	}
}

// TestStartAndClose_Concurrently: a Close racing Start either finds
// nothing started, and Start then refuses, or waits for every loop Start
// launched. Both return and nothing is left running.
func TestStartAndClose_Concurrently(t *testing.T) {
	for range 300 {
		e := newEntry(typePeriodic, "tick", "@every 1h", every(time.Hour), OverlapSkip, noop)
		j := &Jobs{cfg: Config{Enabled: true, StopBudget: 5 * time.Second}, logger: testLogger(t),
			sched: &scheduler{logger: testLogger(t), entries: []*entry{e}}}
		started := make(chan error, 1)
		go func() { started <- j.Start(t.Context()) }()
		if err := j.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-started; err != nil && !strings.Contains(err.Error(), "Start after Close") {
			t.Fatal(err)
		}
		if err := j.sched.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !e.snapshot().Next.IsZero() {
			t.Fatal("a loop is still running")
		}
	}
}
