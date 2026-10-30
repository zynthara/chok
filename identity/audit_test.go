package identity

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/choktest"
	"github.com/zynthara/chok/internal/tracectx"
)

// auditOn returns an Identity with the audit log on.
func auditOn(t testing.TB, c AuditConfig) *Identity {
	t.Helper()
	c.Enabled = true
	return newIdentity(t, Config{Audit: c})
}

// detachedAudit is an Audit on idn's database whose writer is not
// running yet: start it with a.start, or never, to hold the queue full.
func detachedAudit(idn *Identity, buffer int, dropOnFull bool) *Audit {
	a := &Audit{
		enabled: true, d: idn.d, logger: slog.New(slog.DiscardHandler), dropOnFull: dropOnFull,
		batchSize: auditBatchSize, flushEvery: auditFlushEvery, purgeEvery: auditPurgeEvery, now: time.Now,
	}
	a.in = make(chan Event, buffer)
	a.done = make(chan struct{})
	return a
}

// startWriter starts a detached audit's writer.
func (a *Audit) startWriter(t testing.TB) {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.run(ctx)
	t.Cleanup(func() { _ = a.close(context.Background()) })
}

func count(t testing.TB, idn *Identity, where string, args ...any) int64 {
	t.Helper()
	var n int64
	q := idn.d.Unsafe(context.Background()).Model(&auditRow{})
	if where != "" {
		q = q.Where(where, args...)
	}
	if err := q.Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func waitRows(t testing.TB, idn *Identity, want int64, within time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := count(t, idn, "")
		if n >= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAudit_Table(t *testing.T) {
	idn := newIdentity(t, Config{})
	m := idn.d.Unsafe(t.Context()).Migrator()
	if !m.HasTable("audit_logs") || m.HasColumn(&auditRow{}, "rid") || m.HasColumn(&auditRow{}, "version") || m.HasColumn(&auditRow{}, "updated_at") {
		t.Fatal("audit_logs is created with its own columns only: append-only, no version, no updated_at")
	}
	for _, col := range []string{"before_data", "after_data", "metadata", "occurred_at", "request_id"} {
		if !m.HasColumn(&auditRow{}, col) {
			t.Errorf("no column %s", col)
		}
	}
}

func TestAudit_EnumsAreStable(t *testing.T) {
	for got, want := range map[string]string{
		ResultSuccess: "success", ResultFailure: "failure", ResultDenied: "denied",
		ActorUser: "user", ActorSystem: "system", ActorAPIKey: "api_key",
	} {
		if got != want {
			t.Errorf("%q changed to %q: stored values never change", want, got)
		}
	}
}

func TestAudit_DisabledByDefault(t *testing.T) {
	idn := newIdentity(t, Config{})
	a := idn.Audit()
	a.Log(t.Context(), Event{Action: "x"})
	if err := a.LogSync(t.Context(), Event{Action: "x"}); err != nil {
		t.Fatal(err)
	}
	if a.Enabled() || count(t, idn, "") != 0 {
		t.Fatal("off, the log records nothing")
	}
	// Nothing is mounted either.
	c := serve(t, idn)
	if code := c.GetJSON("/audit/logs", nil); code != http.StatusNotFound {
		t.Fatalf("/audit/logs with the log off = %d", code)
	}
}

func TestAudit_LogSyncRoundTrip(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	at := time.Date(2026, 10, 24, 8, 30, 0, 0, time.FixedZone("CST", 8*3600))
	err := idn.Audit().LogSync(t.Context(), Event{
		Action: "post.publish", Resource: "post", ResourceID: "pst_1", ActorID: "usr_alice", OccurredAt: at,
		Before: map[string]any{"status": "draft"}, After: map[string]any{"status": "published"},
		Metadata: map[string]any{"campaign": "fall"}, Reason: strings.Repeat("r", 600), TraceID: "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := idn.Audit().ListFromQuery(t.Context(), url.Values{"action": {"post.publish"}})
	if err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("list = %+v %v", page, err)
	}
	e := page.Items[0]
	if !strings.HasPrefix(e.ID, "aud_") || !e.OccurredAt.Equal(at) || e.OccurredAt.Location() != time.UTC || e.Result != ResultSuccess || e.ActorType != ActorUser {
		t.Fatalf("defaults and the time: %+v", e)
	}
	if e.Before.(map[string]any)["status"] != "draft" || e.After.(map[string]any)["status"] != "published" || e.Metadata["campaign"] != "fall" {
		t.Fatalf("the JSON documents: %+v", e)
	}
	if len(e.Reason) != auditReasonMax || e.TraceID != "0af7651916cd43dd8448eb211c80319c" || e.ResourceID != "pst_1" {
		t.Fatalf("reason cut to %d, trace %q", len(e.Reason), e.TraceID)
	}
}

func TestAudit_RefusesAnEventWithoutAction(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	if err := idn.Audit().LogSync(t.Context(), Event{Resource: "post"}); err == nil || !strings.Contains(err.Error(), "Action") {
		t.Fatalf("no action: %v", err)
	}
	// The async path drops it and counts the failure.
	a := detachedAudit(idn, 4, false)
	a.startWriter(t)
	a.Log(t.Context(), Event{Resource: "post"})
	if err := a.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.failed.Load() != 1 || count(t, idn, "") != 0 {
		t.Fatalf("failed=%d rows=%d", a.failed.Load(), count(t, idn, ""))
	}
}

func TestAudit_LogFillsTheActorFromTheRequest(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	c := serve(t, idn, func(app *chok.App) {
		chok.POST(app.Group("/api", idn.Authn()), "/publish", func(ctx context.Context, _ *chok.None) (chok.None, error) {
			return chok.None{}, idn.Audit().LogSync(ctx, Event{Action: "post.publish"})
		})
	})
	tok := register(t, c, "alice@test.com", "password123")
	resp := c.WithBearer(tok).Do(httptestRequest(http.MethodPost, "/api/publish", nil))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("publish = %d", resp.StatusCode)
	}
	page, err := idn.Audit().ListFromQuery(t.Context(), url.Values{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	e := page.Items[0]
	if e.ActorID != userByEmail(t, idn, "alice@test.com").RID || e.ActorType != ActorUser || e.ActorIP != "192.0.2.1" || e.RequestID != resp.Header.Get("X-Request-Id") {
		t.Fatalf("the request's actor, address and id: %+v", e)
	}
	// Outside a request there is nothing to fill: the event stays anonymous.
	if got := mergeContext(context.Background(), Event{Action: "x"}); got.ActorID != "" || got.ActorIP != "" || got.RequestID != "" {
		t.Fatalf("an empty context: %+v", got)
	}
}

func TestAudit_ExplicitFieldsWin(t *testing.T) {
	ctx := chok.WithPrincipal(context.Background(), chok.Principal{UserID: "usr_ctx"})
	got := mergeContext(ctx, Event{Action: "key.rotate", ActorID: "key_1", ActorType: ActorAPIKey, ActorIP: "10.0.0.1", RequestID: "req-explicit"})
	if got.ActorID != "key_1" || got.ActorType != ActorAPIKey || got.ActorIP != "10.0.0.1" || got.RequestID != "req-explicit" {
		t.Fatalf("explicit fields are kept: %+v", got)
	}
	got = mergeContext(ctx, Event{Action: "job.run", ActorType: ActorSystem})
	if got.ActorID != "usr_ctx" || got.ActorType != ActorSystem {
		t.Fatalf("a blank actor fills in, the explicit type stays: %+v", got)
	}
}

func TestAudit_CloseDrainsTheQueue(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := detachedAudit(idn, 64, false)
	a.startWriter(t)
	const n = 25
	for range n {
		a.Log(t.Context(), Event{ActorID: "usr_alice", Action: "task.create"})
	}
	if err := a.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := count(t, idn, ""); got != n || a.pending.Load() != 0 || a.written.Load() != n {
		t.Fatalf("after close: rows=%d pending=%d written=%d", got, a.pending.Load(), a.written.Load())
	}
	// After close, Log drops instead of panicking.
	a.Log(t.Context(), Event{Action: "late"})
	if a.dropped.Load() != 1 {
		t.Fatalf("dropped = %d", a.dropped.Load())
	}
}

func TestAudit_FlushesOnTheTimer(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := detachedAudit(idn, 64, false)
	a.batchSize, a.flushEvery = 1000, 50*time.Millisecond
	a.startWriter(t)
	a.Log(t.Context(), Event{Action: "task.create"})
	if got := waitRows(t, idn, 1, 2*time.Second); got != 1 {
		t.Fatalf("a lone event never reached the table: %d rows", got)
	}
}

func TestAudit_DropOnFull(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := detachedAudit(idn, 1, true) // the writer is not running: the queue holds one
	for range 50 {
		a.Log(t.Context(), Event{Action: "task.create"})
	}
	if a.pending.Load() != 1 || a.dropped.Load() != 49 {
		t.Fatalf("pending=%d dropped=%d", a.pending.Load(), a.dropped.Load())
	}
	a.startWriter(t)
	if err := a.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if count(t, idn, "") != 1 {
		t.Fatal("the queued event is written")
	}
}

func TestAudit_BlockOnFullRespectsTheContext(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := detachedAudit(idn, 1, false)
	a.Log(t.Context(), Event{Action: "first"}) // fills the queue
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	begin := time.Now()
	a.Log(ctx, Event{Action: "second"}) // waits for room until ctx ends
	if waited := time.Since(begin); waited < 15*time.Millisecond || waited > 2*time.Second || a.dropped.Load() != 1 {
		t.Fatalf("waited %v, dropped %d", waited, a.dropped.Load())
	}
	a.startWriter(t)
}

func TestAudit_CountersAreConsistent(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := detachedAudit(idn, 32, false)
	a.startWriter(t)
	for range 10 {
		a.Log(t.Context(), Event{ActorID: "u", Action: "x"})
	}
	if err := a.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.pending.Load() != 0 || a.written.Load() != 10 || a.failed.Load() != 0 || a.dropped.Load() != 0 {
		t.Fatalf("pending=%d written=%d failed=%d dropped=%d", a.pending.Load(), a.written.Load(), a.failed.Load(), a.dropped.Load())
	}
}

func TestAudit_ConcurrentLogs(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := detachedAudit(idn, 256, true)
	a.startWriter(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				a.Log(context.Background(), Event{ActorID: "u", Action: "x"})
				_ = a.health()
			}
		})
	}
	wg.Wait()
	if err := a.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := uint64(count(t, idn, "")); got != a.written.Load() || got+a.dropped.Load() != 400 {
		t.Fatalf("rows=%d written=%d dropped=%d", got, a.written.Load(), a.dropped.Load())
	}
}

func TestAudit_ListFromQuery(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := idn.Audit()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range 12 {
		actor := "usr_alice"
		if i >= 8 {
			actor = "usr_bob"
		}
		if err := a.LogSync(t.Context(), Event{ActorID: actor, Action: "task.create", Resource: "task", ResourceID: "tsk", OccurredAt: base.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := a.ListFromQuery(t.Context(), url.Values{"actor_id": {"usr_alice"}, "page": {"1"}, "size": {"5"}})
	if err != nil || page.Total != 8 || len(page.Items) != 5 || !page.HasMore || page.Page != 1 || page.Size != 5 {
		t.Fatalf("page 1: %+v %v", page, err)
	}
	if !page.Items[0].OccurredAt.After(page.Items[4].OccurredAt) {
		t.Fatal("newest first")
	}
	page, _ = a.ListFromQuery(t.Context(), url.Values{"actor_id": {"usr_alice"}, "page": {"2"}, "size": {"5"}})
	if len(page.Items) != 3 || page.HasMore {
		t.Fatalf("page 2: %+v", page)
	}
	// from and to bound occurred_at, inclusive; order flips the time.
	page, _ = a.ListFromQuery(t.Context(), url.Values{
		"from": {base.Add(2 * time.Hour).Format(time.RFC3339)}, "to": {base.Add(4 * time.Hour).Format(time.RFC3339)}, "order": {"occurred_at:asc"},
	})
	if page.Total != 3 || !page.Items[0].OccurredAt.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("from/to: %+v", page)
	}
	for name, q := range map[string]url.Values{
		"unknown":      {"nope": {"1"}},
		"bad size":     {"size": {"501"}},
		"zero page":    {"page": {"0"}},
		"bad order":    {"order": {"reason:asc"}},
		"bad time":     {"from": {"yesterday"}},
		"given twice":  {"action": {"a", "b"}},
		"bad dir":      {"order": {"occurred_at:up"}},
		"not a number": {"page": {"one"}},
	} {
		if _, err := a.ListFromQuery(t.Context(), q); err == nil {
			t.Errorf("%s: %v passed", name, q)
		} else if e, ok := err.(*chok.Error); !ok || e.Code != http.StatusBadRequest {
			t.Errorf("%s: want a 400, got %v", name, err)
		}
	}
}

func TestAudit_RetentionPurges(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := idn.Audit()
	now := time.Now().UTC()
	for i := range 5 {
		if err := a.LogSync(t.Context(), Event{Action: "old", OccurredAt: now.Add(-time.Duration(48+i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.LogSync(t.Context(), Event{Action: "fresh", OccurredAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	n, err := a.purge(t.Context(), now.Add(-24*time.Hour))
	if err != nil || n != 5 || count(t, idn, "") != 1 || count(t, idn, "action = ?", "fresh") != 1 {
		t.Fatalf("purged %d (%v); left %d", n, err, count(t, idn, ""))
	}
	// The writer sweeps on its own: a short purge period and retention.
	b := detachedAudit(idn, 8, false)
	b.retention, b.purgeEvery = time.Minute, 20*time.Millisecond
	if err := b.LogSync(t.Context(), Event{Action: "stale", OccurredAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	b.startWriter(t)
	deadline := time.Now().Add(2 * time.Second)
	for count(t, idn, "action = ?", "stale") > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count(t, idn, "action = ?", "stale") != 0 || count(t, idn, "action = ?", "fresh") != 0 {
		t.Fatal("the writer's sweep removes records past the retention (fresh is an hour old too)")
	}
}

func TestAudit_HealthReportsWriteFailures(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	a := detachedAudit(idn, 4, false)
	if err := a.health(); err != nil {
		t.Fatal(err)
	}
	a.fail(context.DeadlineExceeded)
	if err := a.health(); err == nil || !strings.Contains(err.Error(), "a write failed") {
		t.Fatalf("health after a failure: %v", err)
	}
	a.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if err := a.health(); err != nil {
		t.Fatalf("a minute later the failure is old news: %v", err)
	}
}

func TestAudit_AdminAPI(t *testing.T) {
	idn := auditOn(t, AuditConfig{AdminAPI: true})
	c := serve(t, idn)
	tok := register(t, c, "alice@test.com", "password123")
	if err := idn.Audit().LogSync(t.Context(), Event{Action: "post.publish"}); err != nil {
		t.Fatal(err)
	}
	if code := c.GetJSON("/audit/logs", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", code)
	}
	var e envelope
	if code := c.WithBearer(tok).GetJSON("/audit/logs", &e); code != http.StatusForbidden || e.Reason != "Forbidden" {
		t.Fatalf("a user without the admin role = %d %+v", code, e)
	}
	u := userByEmail(t, idn, "alice@test.com")
	if err := idn.Users().SetRoles(t.Context(), u.RID, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	_, tok = login(t, c, "alice@test.com", "password123")
	var page chok.Page[Event]
	if code := c.WithBearer(tok).GetJSON("/audit/logs?action=post.publish", &page); code != http.StatusOK || page.Total != 1 || page.Items[0].Action != "post.publish" {
		t.Fatalf("an admin = %d %+v", code, page)
	}
	// Without admin_api the route is not mounted.
	c2 := choktest.App(t, chok.Base{}, func(app *chok.App) { app.Use(auditOn(t, AuditConfig{AdminAPI: false})) })
	if code := c2.GetJSON("/audit/logs", nil); code != http.StatusNotFound {
		t.Fatalf("admin_api off = %d", code)
	}
}

// TestAudit_FillsTheTraceIDFromTheContext: with the trace hook set, as
// package telemetry sets it when the binary links it, Log and LogSync take
// the trace id of the context; an explicit TraceID stays; without the
// hook nothing is filled.
func TestAudit_FillsTheTraceIDFromTheContext(t *testing.T) {
	before := tracectx.TraceID
	t.Cleanup(func() { tracectx.TraceID = before })
	type traceKey struct{}
	ctx := context.WithValue(t.Context(), traceKey{}, "4bf92f3577b34da6a3ce929d0e0e4736")

	tracectx.TraceID = nil
	if got := mergeContext(ctx, Event{Action: "x"}); got.TraceID != "" {
		t.Fatalf("without the hook: %q", got.TraceID)
	}
	tracectx.TraceID = func(ctx context.Context) string {
		id, _ := ctx.Value(traceKey{}).(string)
		return id
	}
	if got := mergeContext(ctx, Event{Action: "x", TraceID: "explicit"}); got.TraceID != "explicit" {
		t.Fatalf("an explicit trace id stays: %q", got.TraceID)
	}
	idn := auditOn(t, AuditConfig{})
	if err := idn.Audit().LogSync(ctx, Event{Action: "post.publish"}); err != nil {
		t.Fatal(err)
	}
	idn.Audit().Log(ctx, Event{Action: "post.archive"})
	waitRows(t, idn, 2, 5*time.Second)
	page, err := idn.Audit().ListFromQuery(t.Context(), url.Values{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("%+v %v", page, err)
	}
	for _, e := range page.Items {
		if e.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Fatalf("%s: trace id %q", e.Action, e.TraceID)
		}
	}
}
