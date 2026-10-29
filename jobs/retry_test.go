package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

// The failure handling of the workers: retries after a growing wait, the
// count kept on the message row, parking in the dead letters, and Dead,
// Requeue and Discard.

// TestWorker_BacksOffBetweenTries: a message the worker fails on is tried
// again after RetryBackoff, then after twice as long each time up to
// MaxBackoff — not on every sweep — and the MaxAttempts-th failure parks
// it; the messages behind it wait, then flow. While it waits, Stats show
// its failures, the last one's error and its next try: the sweeps that
// only wait keep LastError. The clock is synctest's, the database the
// lane's.
func TestWorker_BacksOffBetweenTries(t *testing.T) {
	// gorm caches a table's parsed schema for the whole process, with a
	// channel in it: parsed first inside the bubble, the cache would
	// belong to it, and the next test would die receiving from it.
	tables()
	synctest.Test(t, func(t *testing.T) {
		d := dbtest.Open(t)
		start := time.Now()
		var mu sync.Mutex
		var tries []time.Duration // when the failing message came, since start
		var took []int
		cfg := Config{
			Enabled: true, PollInterval: 100 * time.Millisecond,
			MaxAttempts: 5, RetryBackoff: time.Second, MaxBackoff: 3 * time.Second,
		}
		j := newJobs(t, d, cfg, Worker("flaky", func(_ context.Context, m Message[int]) error {
			mu.Lock()
			defer mu.Unlock()
			if m.Payload == 1 {
				tries = append(tries, time.Since(start))
				return errors.New("downstream down")
			}
			took = append(took, m.Payload)
			return nil
		}))
		run(t, j)
		enqueueTx(t, d, j, "flaky", 1)
		enqueueTx(t, d, j, "flaky", 2)

		time.Sleep(2 * time.Second)
		synctest.Wait()
		st := j.Stats().Workers[0]
		if st.HeadAttempts != 2 || !st.NextRetry.Equal(start.Add(3100*time.Millisecond)) || st.Failures != 2 || st.Parked != 0 ||
			!strings.Contains(st.LastError, "downstream down") {
			t.Fatalf("stats while the head waits: %+v", st)
		}

		time.Sleep(10 * time.Second)
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		// The first sweep at 100ms, then 1s, 2s, 3s and 3s later.
		want := []time.Duration{100 * time.Millisecond, 1100 * time.Millisecond, 3100 * time.Millisecond, 6100 * time.Millisecond, 9100 * time.Millisecond}
		if !slices.Equal(tries, want) {
			t.Fatalf("tries at %v, want %v", tries, want)
		}
		if !slices.Equal(took, []int{2}) {
			t.Fatalf("after the park the topic delivered %v, want [2]", took)
		}
		st = j.Stats().Workers[0]
		if st.Parked != 1 || st.HeadAttempts != 0 || !st.NextRetry.IsZero() || st.Failures != 4 || st.Messages != 1 || st.LastError != "" {
			t.Fatalf("stats after the park: %+v", st)
		}
		dead, err := j.Dead(t.Context(), "flaky", 0)
		if err != nil || len(dead) != 1 {
			t.Fatalf("dead letters %+v, %v", dead, err)
		}
		if m := dead[0]; m.Attempts != 5 || m.LastError != "downstream down" || string(m.Payload) != "1" ||
			!m.ParkedAt.Equal(start.Add(9100*time.Millisecond)) {
			t.Fatalf("parked %+v", m)
		}
	})
}

// TestRetryPolicy_Delay: the wait doubles from the backoff and stops at
// the cap, however many failures there were.
func TestRetryPolicy_Delay(t *testing.T) {
	p := retryPolicy{maxAttempts: 10, backoff: time.Second, maxBackoff: 5 * time.Minute}
	for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 9: 256 * time.Second, 10: 5 * time.Minute, 1000: 5 * time.Minute} {
		if got := p.delay(n); got != want {
			t.Errorf("delay(%d) = %s, want %s", n, got, want)
		}
	}
	if got := (retryPolicy{}).delay(3); got != 0 {
		t.Errorf("a zero backoff waits %s", got)
	}
}

// TestRelay_FailureCountSurvivesARestart: a failure is counted on the
// message row, so a restarted worker goes on from the count — its next
// delivery is numbered after it and the count still parks at
// MaxAttempts — and a panic counts as a failure.
func TestRelay_FailureCountSurvivesARestart(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	enqueue(t, c, "count", "m1")
	enqueue(t, c, "count", "m2")
	var seen []int // m1's Message.Attempt, delivery by delivery
	var m1 uint64
	first, err := newRelay("count", func(_ context.Context, m rawMessage) error {
		if string(m.Payload) == "m1" {
			m1 = m.ID
			seen = append(seen, m.Attempt)
			if m.Attempt == 1 {
				panic("boom")
			}
			return errors.New("still down")
		}
		return nil
	}, c, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	first.retry = retryPolicy{maxAttempts: 3} // no backoff: the next sweep tries again
	if err := first.run(ctx); err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("the sweep of the panic: %v", err)
	}
	if row := messageRow(t, c, m1); row.Attempts != 1 || row.LastError != "panic: boom" || row.RetryAt != nil {
		t.Fatalf("after the panic the row is %+v", row)
	}
	if err := first.run(ctx); err == nil || !strings.Contains(err.Error(), "attempt 2 of 3") {
		t.Fatalf("the second failing sweep: %v", err)
	}

	// The restart: a new relay, with nothing in memory.
	var got []string
	second, err := newRelay("count", func(_ context.Context, m rawMessage) error {
		if string(m.Payload) == "m1" {
			seen = append(seen, m.Attempt)
			return errors.New("still down")
		}
		got = append(got, string(m.Payload))
		return nil
	}, c, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	second.retry = retryPolicy{maxAttempts: 3}
	if err := second.run(ctx); err != nil {
		t.Fatalf("the sweep that parks m1: %v", err)
	}
	if !slices.Equal(seen, []int{1, 2, 3}) || !slices.Equal(got, []string{"m2"}) {
		t.Fatalf("m1's attempts %v, then delivered %v", seen, got)
	}
	if row := messageRow(t, c, m1); row.Attempts != 3 || row.LastError != "still down" {
		t.Fatalf("the parked message's row %+v", row)
	}
	var dead deadRow
	if err := c.d.Unsafe(ctx).Take(&dead, m1).Error; err != nil || dead.Attempts != 3 || dead.Topic != "count" || string(dead.Payload) != "m1" {
		t.Fatalf("the dead letter %+v, %v", dead, err)
	}
}

// TestRelay_ParkedMessageIsNotRetriedAfterACrash: a crash after a worker
// parked a message, before its watermark passed it, replays the settle
// window; the parked message is found in the dead letters and not
// delivered again, while the delivered one is (at least once).
func TestRelay_ParkedMessageIsNotRetriedAfterACrash(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	enqueue(t, c, "parks", "m1")
	enqueue(t, c, "parks", "m2")
	calls := map[string]int{}
	handler := func(_ context.Context, m rawMessage) error {
		calls[string(m.Payload)]++
		if string(m.Payload) == "m1" {
			return errors.New("down")
		}
		return nil
	}
	for range 2 { // the first process, then the one after the crash
		r, err := newRelay("parks", handler, c, time.Hour, 10) // nothing settles
		if err != nil {
			t.Fatal(err)
		}
		r.retry = retryPolicy{maxAttempts: 1}
		if err := r.run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls["m1"] != 1 || calls["m2"] != 2 {
		t.Fatalf("deliveries %v: the parked m1 once, the delivered m2 again after the crash", calls)
	}
	var n int64
	if err := c.d.Unsafe(ctx).Model(&deadRow{}).Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("dead letters %d, %v", n, err)
	}
}

// TestWorker_ParksAtMaxAttempts: the MaxAttempts-th failure parks the
// message — copied into the dead letters with its count and last error —
// and the topic moves on; Observe gets message.parked and Stats count it.
func TestWorker_ParksAtMaxAttempts(t *testing.T) {
	d := dbtest.Open(t)
	var mu sync.Mutex
	var took []string
	var parked []Event
	cfg := fastConfig()
	cfg.MaxAttempts = 2
	j := newJobs(t, d, cfg,
		Worker("orders", func(_ context.Context, m Message[string]) error {
			if m.Payload == "poison" {
				return fmt.Errorf("attempt %d failed", m.Attempt)
			}
			mu.Lock()
			took = append(took, m.Payload)
			mu.Unlock()
			return nil
		}),
		Observe(func(ev Event) {
			if ev.Kind == "message.parked" {
				mu.Lock()
				parked = append(parked, ev)
				mu.Unlock()
			}
		}))
	run(t, j)
	enqueueTx(t, d, j, "orders", "poison")
	enqueueTx(t, d, j, "orders", "next")
	eventually(t, "the message after the poison", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(took) == 1
	})
	mu.Lock()
	if len(parked) != 1 || parked[0].Job != "orders" || parked[0].Type != "worker" || parked[0].Error != "attempt 2 failed" ||
		parked[0].StartedAt.IsZero() || parked[0].StartedAt.Location() != time.UTC || took[0] != "next" {
		t.Fatalf("parked events %+v, delivered %v", parked, took)
	}
	mu.Unlock()
	// The head clears when the sweep that parked it ends.
	eventually(t, "the stats of that sweep", func() bool {
		st := j.Stats().Workers[0]
		return st.Parked == 1 && st.Messages == 1 && st.HeadAttempts == 0 && st.NextRetry.IsZero()
	})
	dead, err := j.Dead(t.Context(), "orders", 0)
	if err != nil || len(dead) != 1 {
		t.Fatalf("dead letters %+v, %v", dead, err)
	}
	if m := dead[0]; m.Topic != "orders" || string(m.Payload) != `"poison"` || m.Attempts != 2 || m.LastError != "attempt 2 failed" ||
		m.CreatedAt.IsZero() || m.ParkedAt.Before(m.CreatedAt) {
		t.Fatalf("parked %+v", m)
	}
}

// TestWorker_UndecodablePayloadParksAtOnce: a payload that does not
// decode into the worker's type is parked on its first delivery, without
// calling the function; the next message flows, decoded.
func TestWorker_UndecodablePayloadParksAtOnce(t *testing.T) {
	type order struct {
		N int `json:"n"`
	}
	d := dbtest.Open(t)
	var mu sync.Mutex
	var got []Message[order]
	j := newJobs(t, d, fastConfig(), Worker("orders", func(_ context.Context, m Message[order]) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, m)
		return nil
	}))
	run(t, j)
	enqueueTx(t, d, j, "orders", "not an order")
	enqueueTx(t, d, j, "orders", order{N: 7})
	eventually(t, "the order", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	if got[0].Payload.N != 7 || got[0].Attempt != 1 || got[0].Topic != "orders" {
		t.Fatalf("delivered %+v", got)
	}
	mu.Unlock()
	dead, err := j.Dead(t.Context(), "", 0)
	if err != nil || len(dead) != 1 {
		t.Fatalf("dead letters %+v, %v", dead, err)
	}
	if m := dead[0]; m.Attempts != 1 || !strings.HasPrefix(m.LastError, "decode: ") || string(m.Payload) != `"not an order"` {
		t.Fatalf("parked %+v", m)
	}
	if st := j.Stats().Workers[0]; st.Parked != 1 || st.Failures != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// TestDeadLetters_RequeueAndDiscard: Dead lists the parked messages,
// Requeue enqueues one again as a new message its worker gets, Discard
// drops one; an id that is not parked fails either call, naming it, as a
// 404, and nothing changes. A process with the jobs disabled can do all
// three.
func TestDeadLetters_RequeueAndDiscard(t *testing.T) {
	cfg := dbtest.Config(t)
	d := dbtest.OpenConfig(t, cfg)
	var healed atomic.Bool
	var mu sync.Mutex
	var got []Message[string]
	wcfg := fastConfig()
	wcfg.MaxAttempts = 1
	j := newJobs(t, d, wcfg, Worker("orders", func(_ context.Context, m Message[string]) error {
		if !healed.Load() {
			return errors.New("down")
		}
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		return nil
	}))
	run(t, j)
	enqueueTx(t, d, j, "orders", "a")
	enqueueTx(t, d, j, "orders", "b")
	eventually(t, "both parked", func() bool { return j.Stats().Workers[0].Parked == 2 })

	// The admin side: an enqueue-only process on the same database.
	admin := newJobs(t, dbtest.OpenConfig(t, cfg), Config{})
	ctx := t.Context()
	payloads := func() []string {
		t.Helper()
		dead, err := admin.Dead(ctx, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range dead {
			out = append(out, string(m.Payload))
		}
		return out
	}
	dead, err := admin.Dead(ctx, "", 0)
	if err != nil || len(dead) != 2 || string(dead[0].Payload) != `"a"` || string(dead[1].Payload) != `"b"` {
		t.Fatalf("Dead: %+v, %v", dead, err)
	}
	if other, err := admin.Dead(ctx, "other", 0); err != nil || len(other) != 0 {
		t.Fatalf("Dead of another topic: %+v, %v", other, err)
	}
	if one, err := admin.Dead(ctx, "", 1); err != nil || len(one) != 1 || one[0].ID != dead[0].ID {
		t.Fatalf("Dead with limit 1: %+v, %v", one, err)
	}
	answers := func(err error, code int) bool {
		c, ok := errors.AsType[interface {
			error
			chok.Coder
		}](err)
		return ok && c.APIError().Code == code
	}
	for _, limit := range []int{-1, 1001} {
		if _, err := admin.Dead(ctx, "", limit); !answers(err, 400) || !strings.Contains(err.Error(), "jobs: Dead: limit") {
			t.Fatalf("Dead with limit %d: %v", limit, err)
		}
	}
	a, b := dead[0], dead[1]
	is404 := func(err error, id uint64) bool {
		return answers(err, 404) && strings.Contains(err.Error(), fmt.Sprint(id))
	}

	if err := admin.Requeue(ctx, a.ID, 4242); !is404(err, 4242) || strings.Contains(err.Error(), fmt.Sprint(a.ID)+",") {
		t.Fatalf("Requeue with an id that is not parked: %v", err)
	}
	if p := payloads(); !slices.Equal(p, []string{`"a"`, `"b"`}) {
		t.Fatalf("a failed Requeue changed the dead letters: %v", p)
	}
	healed.Store(true)
	if err := admin.Requeue(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the requeued message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	if m := got[0]; m.Payload != "a" || m.ID == a.ID || m.Attempt != 1 || !m.CreatedAt.After(a.CreatedAt) {
		t.Fatalf("requeued as %+v; parked as %+v", m, a)
	}
	mu.Unlock()
	if p := payloads(); !slices.Equal(p, []string{`"b"`}) {
		t.Fatalf("after Requeue: %v", p)
	}

	if err := admin.Discard(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if p := payloads(); len(p) != 0 {
		t.Fatalf("after Discard: %v", p)
	}
	if err := admin.Discard(ctx, b.ID); !is404(err, b.ID) {
		t.Fatalf("Discard of a discarded message: %v", err)
	}
	if err := admin.Requeue(ctx); err != nil {
		t.Fatalf("Requeue of nothing: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("the discarded message was delivered: %+v", got)
	}
}

// messageRow reads a message row back.
func messageRow(t *testing.T, c *core, id uint64) message {
	t.Helper()
	var row message
	if err := c.d.Unsafe(t.Context()).Take(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	return row
}
