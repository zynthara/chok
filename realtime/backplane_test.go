package realtime

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/choktest"
)

// redisClient is a client of mr, closed when the test ends.
func redisClient(t testing.TB, mr *miniredis.Miniredis) *goredis.Client {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: time.Second})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// startHub runs h's Start until the test ends.
func startHub(t testing.TB, h *Hub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- h.Start(ctx) }()
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Start: %v", err)
			}
		})
	})
}

// replica is a hub on mr, started and served.
func replica(t testing.TB, mr *miniredis.Miniredis, cfg Config, opts ...Option) *server {
	t.Helper()
	h := newHub(t, cfg, append([]Option{Redis(redisClient(t, mr)), Authorize(openTopics)}, opts...)...)
	s := serveHub(t, h)
	startHub(t, h)
	eventually(t, "the backplane", func() bool { return h.Health(t.Context()) == nil })
	return s
}

// TestBackplane_TwoHubsShareEvents: what one replica publishes reaches
// the other's subscribers, with its ID, and its own subscribers once —
// the echo from Redis is skipped; the other replica keeps it for resuming.
func TestBackplane_TwoHubsShareEvents(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b := replica(t, mr, testConfig(t)), replica(t, mr, testConfig(t))
	onA := a.dialWS("topics=public.a")
	onB, code, body := b.openSSE("ticket=" + b.ticket() + "&topics=public.a")
	if onB == nil {
		t.Fatalf("%d %s", code, body)
	}
	eventually(t, "the subscriptions", func() bool { return a.hub.Stats().Topics == 1 && b.hub.Stats().Topics == 1 })

	publish(t, a.hub, "public.a", "from.a", 1)
	fromA := readEvent(t, onA)
	if _, ev := onB.nextEvent(t); ev.ID != fromA.ID || ev.Type != "from.a" || string(ev.Data) != "1" || !strings.HasPrefix(ev.ID, a.hub.origin+"-") {
		t.Fatalf("B got %+v, A had %+v", ev, fromA)
	}
	publish(t, b.hub, "public.a", "from.b", 2)
	// A's subscriber gets B's event next: its own did not come back.
	if ev := readEvent(t, onA); ev.Type != "from.b" || !strings.HasPrefix(ev.ID, b.hub.origin+"-") {
		t.Fatalf("A got %+v after its own event", ev)
	}
	if _, ev := onB.nextEvent(t); ev.Type != "from.b" {
		t.Fatalf("B got %+v", ev)
	}
	if sa, sb := a.hub.Stats(), b.hub.Stats(); sa.Published != 1 || sb.Published != 1 || sa.Delivered != 2 || sb.Delivered != 2 ||
		sa.Backplane != "connected" || sb.Backplane != "connected" {
		t.Fatalf("stats A %+v B %+v", sa, sb)
	}
	// A client that moves to B resumes after A's event: B kept it.
	moved, code, body := b.openSSE("ticket="+b.ticket()+"&topics=public.a", "Last-Event-ID", fromA.ID)
	if moved == nil {
		t.Fatalf("%d %s", code, body)
	}
	if _, ev := moved.nextEvent(t); ev.Type != "from.b" {
		t.Fatalf("resumed on B: %+v", ev)
	}
}

// TestBackplane_TicketsAcrossReplicas: with Redis a ticket lives under
// {Channel}:ticket:<ticket> for TicketTTL, any replica redeems it, and
// only once; Redis expires it.
func TestBackplane_TicketsAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := testConfig(t)
	cfg.Channel = "test:rt"
	a, b := replica(t, mr, cfg), replica(t, mr, cfg)
	ticket := a.ticket()
	if !mr.Exists("test:rt:ticket:"+ticket) || mr.TTL("test:rt:ticket:"+ticket) != time.Minute {
		t.Fatalf("keys %v, ttl %s", mr.Keys(), mr.TTL("test:rt:ticket:"+ticket))
	}
	if stream, code, body := b.openSSE("ticket=" + ticket + "&topics=public.a"); stream == nil {
		t.Fatalf("redeemed on the other replica: %d %s", code, body)
	}
	if _, code, _ := a.openSSE("ticket=" + ticket + "&topics=public.a"); code != http.StatusUnauthorized {
		t.Fatalf("redeemed twice: %d", code)
	}
	if mr.Exists("test:rt:ticket:" + ticket) {
		t.Fatal("a redeemed ticket is still in Redis")
	}
	expiring := a.ticket()
	mr.FastForward(time.Minute + time.Second)
	if _, code, _ := b.openSSE("ticket=" + expiring + "&topics=public.a"); code != http.StatusUnauthorized {
		t.Fatalf("an expired ticket: %d", code)
	}
}

// TestBackplane_HealthAndReconnect: Health is nil without Redis, an
// error until the subscription is up, nil while it is, and an error
// while Redis is gone; the hub reconnects by itself, logging the loss at
// Warn, and the replicas hear each other again.
func TestBackplane_HealthAndReconnect(t *testing.T) {
	if err := newHub(t, testConfig(t)).Health(t.Context()); err != nil {
		t.Fatalf("without Redis: %v", err)
	}
	mr := miniredis.RunT(t)
	var logs syncBuffer
	h := newHub(t, testConfig(t), Redis(redisClient(t, mr)), Authorize(openTopics), Logger(slogTo(&logs)))
	if err := h.Health(t.Context()); err == nil || !strings.Contains(err.Error(), "not subscribed yet") || h.Stats().Backplane != "disconnected" {
		t.Fatalf("before Start: %v %+v", err, h.Stats())
	}
	s := serveHub(t, h)
	startHub(t, h)
	eventually(t, "subscribed", func() bool { return h.Health(t.Context()) == nil && h.Stats().Backplane == "connected" })
	other := replica(t, mr, testConfig(t))
	ws := s.dialWS("topics=public.a")
	eventually(t, "the subscription", func() bool { return h.Stats().Topics == 1 })

	mr.Close()
	eventually(t, "the loss noticed", func() bool {
		err := h.Health(t.Context())
		return err != nil && strings.Contains(err.Error(), "disconnected") && h.Stats().Backplane == "disconnected"
	})
	// The state changes first, the line follows.
	eventually(t, "the Warn about the loss", func() bool {
		return strings.Contains(logs.String(), "level=WARN msg=\"realtime: the Redis backplane is disconnected; reconnecting\"")
	})
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reconnected", func() bool {
		return h.Health(t.Context()) == nil && other.hub.Health(t.Context()) == nil
	})
	eventually(t, "the line about the reconnect", func() bool {
		return strings.Contains(logs.String(), "realtime: the Redis backplane is connected again")
	})
	publish(t, other.hub, "public.a", "after.restart", nil)
	if ev := readEvent(t, ws); ev.Type != "after.restart" {
		t.Fatalf("%+v", ev)
	}
}

// TestBackplane_IgnoresMalformedMessages: what is not an event of another
// replica on the channel is logged and dropped.
func TestBackplane_IgnoresMalformedMessages(t *testing.T) {
	mr := miniredis.RunT(t)
	var logs syncBuffer
	h := newHub(t, testConfig(t), Redis(redisClient(t, mr)), Authorize(openTopics), Logger(slogTo(&logs)))
	s := serveHub(t, h)
	startHub(t, h)
	eventually(t, "subscribed", func() bool { return h.Health(t.Context()) == nil })
	ws := s.dialWS("topics=public.a")
	eventually(t, "the subscription", func() bool { return h.Stats().Topics == 1 })
	c := redisClient(t, mr)
	for _, payload := range []string{
		"not json",
		`{"id":"","topic":"public.a","type":"x","at":"2026-01-01T00:00:00Z"}`,
		`{"id":"abc-1","topic":"bad topic","type":"x","at":"2026-01-01T00:00:00Z"}`,
		`{"id":"abc-1","topic":"public.a","type":"reset","at":"2026-01-01T00:00:00Z"}`,
		`{"id":"abc-1","topic":"public.a","type":"refused","at":"2026-01-01T00:00:00Z"}`,
		`{"id":"abc-1","topic":"public.a","type":"error","at":"2026-01-01T00:00:00Z"}`,
	} {
		if err := c.Publish(t.Context(), "chok:realtime", payload).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Publish(t.Context(), "chok:realtime", `{"id":"abc-1","topic":"public.a","type":"good","at":"2026-01-01T00:00:00Z"}`).Err(); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ws); ev.Type != "good" || ev.ID != "abc-1" {
		t.Fatalf("%+v", ev)
	}
	if n := strings.Count(logs.String(), "ignored a malformed message"); n != 6 {
		t.Fatalf("%d malformed messages logged:\n%s", n, logs.String())
	}
}

// TestApp_StartsOnceTheBackplaneIsUp: an App with the hub waits, in its
// start phase, for the subscription, and stops it at the end.
func TestApp_StartsOnceTheBackplaneIsUp(t *testing.T) {
	mr := miniredis.RunT(t)
	h := newHub(t, testConfig(t), Redis(redisClient(t, mr)))
	choktest.App(t, chok.Base{}, func(app *chok.App) { app.Use(h) })
	if err := h.Health(t.Context()); err != nil || len(mr.PubSubChannels("")) != 1 {
		t.Fatalf("the App is ready: %v, channels %v", err, mr.PubSubChannels(""))
	}
}
