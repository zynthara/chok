package realtime

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/internal/conf"
)

// TestConfig_Validate: the path's shape, the origin patterns and every
// bound, all problems in one error.
func TestConfig_Validate(t *testing.T) {
	good := testConfig(t)
	if err := good.Validate(); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	bad := Config{Path: "realtime/", AllowedOrigins: []string{"", "[bad"}, SendBuffer: -1, PingInterval: -time.Second, Replay: -1}
	err := bad.Validate()
	for _, want := range []string{
		`path must start with /`, `allowed_origins[0] is empty`, `allowed_origins[1] "[bad" is not a valid pattern`,
		`send_buffer must be at least 1`, `max_subscriptions must be at least 1`, `ping_interval must be positive`,
		`ticket_ttl must be positive`, `replay must not be negative`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	for _, p := range []string{"/", "/rt/", "/{x}", "/a b", ""} {
		c := testConfig(t)
		c.Path = p
		if c.Validate() == nil {
			t.Fatalf("path %q accepted", p)
		}
	}
}

// TestConfig_LoadsWithDefaults: the section's defaults and environment
// keys through chok.Load, and a bad value failing the load.
func TestConfig_LoadsWithDefaults(t *testing.T) {
	type appConfig struct {
		chok.Base
		Realtime Config `yaml:"realtime"`
	}
	t.Setenv("RTAPP_REALTIME_ALLOWED_ORIGINS", "app.example.com,*.example.org")
	cfg, err := chok.Load[appConfig]("rtapp", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Path: "/realtime", AllowedOrigins: []string{"app.example.com", "*.example.org"}, SendBuffer: 64,
		PingInterval: 25 * time.Second, TicketTTL: time.Minute, Replay: 256, MaxSubscriptions: 64, Channel: "chok:realtime",
	}
	if got := cfg.Realtime; !slices.Equal(got.AllowedOrigins, want.AllowedOrigins) || got.Path != want.Path ||
		got.SendBuffer != want.SendBuffer || got.PingInterval != want.PingInterval || got.TicketTTL != want.TicketTTL ||
		got.Replay != want.Replay || got.MaxSubscriptions != want.MaxSubscriptions || got.Channel != want.Channel {
		t.Fatalf("loaded %+v, want %+v", got, want)
	}
	t.Setenv("RTAPP_REALTIME_SEND_BUFFER", "-5")
	if _, err := chok.Load[appConfig]("rtapp", chok.Args(nil)); err == nil || !strings.Contains(err.Error(), "send_buffer must be at least 1") {
		t.Fatalf("a negative send_buffer: %v", err)
	}
}

// TestNew_DefaultsAndOptions: New fills the zero SendBuffer,
// PingInterval, TicketTTL and MaxSubscriptions (not Path, Replay or
// Channel), checks the options before anything runs, and takes a nil
// Redis client as no Redis.
func TestNew_DefaultsAndOptions(t *testing.T) {
	h, err := New(Config{Path: "/rt"})
	if err != nil {
		t.Fatal(err)
	}
	if c := h.cfg; c.SendBuffer != 64 || c.PingInterval != 25*time.Second || c.TicketTTL != time.Minute || c.MaxSubscriptions != 64 ||
		c.Replay != 0 || c.Channel != "" || h.logger != slog.Default() {
		t.Fatalf("defaults %+v", c)
	}
	if st := h.Stats(); st.Backplane != "" || h.Health(t.Context()) != nil || h.Name() != "realtime" {
		t.Fatalf("without Redis: %+v %v", st, h.Health(t.Context()))
	}
	if err := h.Start(t.Context()); err != nil {
		t.Fatalf("Start without Redis returns at once: %v", err)
	}
	for _, c := range []struct {
		cfg  Config
		opts []Option
		want string
	}{
		{Config{}, nil, "path must start with /"},
		{Config{Path: "/rt"}, []Option{Authn(nil)}, "Authn with a nil middleware"},
		{Config{Path: "/rt"}, []Option{Authorize(nil)}, "Authorize with a nil function"},
		{Config{Path: "/rt"}, []Option{Redis(goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"}))}, "channel is empty"},
	} {
		if _, err := New(c.cfg, c.opts...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	if h, err := New(Config{Path: "/rt"}, Redis(nil)); err != nil || h.redis != nil || h.Stats().Backplane != "" {
		t.Fatalf("Redis(nil): %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustNew did not panic on a bad config")
		}
	}()
	MustNew(Config{})
}

// TestPublish_Checks: a bad topic or type, the hub's own types and data
// that does not encode are errors; no subscriber is not; durations encode
// as chok's handlers write them.
func TestPublish_Checks(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	for _, c := range []struct{ topic, typ string }{
		{"", "t"}, {strings.Repeat("a", 129), "t"}, {"a b", "t"}, {"a/b", "t"}, {"ok", ""}, {"ok", "line\nbreak"},
		{"ok", "refused"}, {"ok", "reset"},
	} {
		if err := h.Publish(t.Context(), c.topic, c.typ, nil); err == nil {
			t.Errorf("Publish(%q, %q) accepted", c.topic, c.typ)
		}
	}
	if err := h.Publish(t.Context(), "ok", "t", func() {}); err == nil || !strings.Contains(err.Error(), "encode the data") {
		t.Fatalf("data that does not encode: %v", err)
	}
	if err := h.Publish(t.Context(), strings.Repeat("a", 128), "nobody.listens", nil); err != nil {
		t.Fatal(err)
	}
	s := serveHub(t, h)
	ws := s.dialWS("topics=public.d")
	eventually(t, "the subscription", func() bool { return h.Stats().Topics == 1 })
	publish(t, h, "public.d", "took", map[string]time.Duration{"took": 1500 * time.Millisecond})
	if ev := readEvent(t, ws); string(ev.Data) != `{"took":"1.5s"}` {
		t.Fatalf("a duration: %s", ev.Data)
	}
	if st := h.Stats(); st.Published != 2 {
		t.Fatalf("published %d, want the two that went out", st.Published)
	}
}

// TestAuthorize_DefaultRuleAndUserTopics: without Authorize only the
// connection's own user: topic is open; with it, user: topics still
// follow that rule and never reach the function.
func TestAuthorize_DefaultRuleAndUserTopics(t *testing.T) {
	h := newHub(t, testConfig(t), Authn(fakeAuthn))
	s := serveHub(t, h)
	ws := s.dialWS("", "X-User", "usr_ada")
	send(t, ws, `{"subscribe":["user:usr_ada","user:usr_bob","news"]}`)
	refused := map[string]bool{}
	for range 2 {
		if ev := readEvent(t, ws); ev.Type == "refused" {
			refused[ev.Topic] = true
		}
	}
	if !refused["user:usr_bob"] || !refused["news"] {
		t.Fatalf("refused %v", refused)
	}
	if err := h.Send(t.Context(), "usr_ada", "private", nil); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ws); ev.Type != "private" || ev.Topic != "user:usr_ada" {
		t.Fatalf("%+v", ev)
	}

	var asked []string
	h2 := newHub(t, testConfig(t), Authn(fakeAuthn), Authorize(func(_ context.Context, topic string) error {
		asked = append(asked, topic)
		return nil
	}))
	s2 := serveHub(t, h2)
	ws2 := s2.dialWS("", "X-User", "usr_ada")
	send(t, ws2, `{"subscribe":["user:usr_bob","user:usr_ada","news"]}`)
	if ev := readEvent(t, ws2); ev.Type != "refused" || ev.Topic != "user:usr_bob" {
		t.Fatalf("another user's topic with Authorize: %+v", ev)
	}
	eventually(t, "the subscriptions", func() bool { return h2.Stats().Topics == 2 })
	if !slices.Equal(asked, []string{"news"}) {
		t.Fatalf("Authorize was asked about %v", asked)
	}
}

// TestStats_Counts: connections, distinct users, topics with a
// subscriber, publishes, deliveries; the backplane is "" without Redis.
func TestStats_Counts(t *testing.T) {
	h := newHub(t, testConfig(t), Authn(fakeAuthn), Authorize(openTopics))
	s := serveHub(t, h)
	a1 := s.dialWS("topics=public.x", "X-User", "usr_a")
	a2 := s.dialWS("topics=public.x,public.y", "X-User", "usr_a")
	s.dialWS("topics=user:usr_b", "X-User", "usr_b")
	eventually(t, "the subscriptions", func() bool {
		return subscribers(h, "public.x") == 2 && subscribers(h, "public.y") == 1 && subscribers(h, "user:usr_b") == 1
	})
	publish(t, h, "public.x", "one", nil)
	publish(t, h, "public.y", "two", nil)
	if err := h.Send(t.Context(), "usr_c", "nobody", nil); err != nil {
		t.Fatal(err)
	}
	readEvent(t, a1)
	readEvent(t, a2)
	readEvent(t, a2)
	want := Stats{Connections: 3, Users: 2, Topics: 3, Published: 3, Delivered: 3}
	if st := h.Stats(); st != want {
		t.Fatalf("stats %+v, want %+v", st, want)
	}
	_ = a2.CloseNow()
	eventually(t, "a2 gone", func() bool { return h.Stats().Connections == 2 })
	if st := h.Stats(); st.Users != 2 || st.Topics != 2 {
		t.Fatalf("after a2 left: %+v", st)
	}
	b, err := json.Marshal(h.Stats())
	if err != nil || !bytes.Contains(b, []byte(`"connections":2`)) || !bytes.Contains(b, []byte(`"backplane":""`)) {
		t.Fatalf("json %s %v", b, err)
	}
}

// TestMount_OpenAPIAndAccessLog: the three endpoints are in the OpenAPI
// document — the ticket behind bearer auth with Authn and public without,
// ws and sse public with their ticket parameter — and the access log has
// one line per connection, when it ends: 101 for a WebSocket, 200 for a
// stream, with the connection's duration.
func TestMount_OpenAPIAndAccessLog(t *testing.T) {
	for _, withAuthn := range []bool{true, false} {
		var opts []Option
		if withAuthn {
			opts = append(opts, Authn(fakeAuthn))
		}
		h := newHub(t, testConfig(t), opts...)
		var base chok.Base
		if err := conf.Defaults(&base); err != nil {
			t.Fatal(err)
		}
		base.HTTP.Enabled = false
		var logs syncBuffer
		app := chok.New(base, chok.Logger(slog.New(slog.NewJSONHandler(&logs, nil))))
		app.Use(h)
		ts := httptest.NewServer(app.Handler())
		t.Cleanup(ts.Close)
		t.Cleanup(func() { _ = h.Close(context.WithoutCancel(t.Context())) })
		s := &server{t: t, hub: h, url: ts.URL, client: ts.Client()}

		_, body := s.do(http.MethodGet, "/openapi.json")
		var doc struct {
			Paths map[string]map[string]openAPIOp `json:"paths"`
		}
		if err := json.Unmarshal(body, &doc, json.MatchCaseInsensitiveNames(true)); err != nil {
			t.Fatal(err)
		}
		ticket, ws, sse := doc.Paths["/realtime/ticket"]["post"], doc.Paths["/realtime/ws"]["get"], doc.Paths["/realtime/sse"]["get"]
		if ticket.Security == nil || (len(*ticket.Security) == 1) != withAuthn {
			t.Fatalf("authn %v: ticket security %v", withAuthn, ticket.Security)
		}
		for name, op := range map[string]openAPIOp{"ws": ws, "sse": sse} {
			if op.Security == nil || len(*op.Security) != 0 || !strings.Contains(op.Summary, "needs a ticket") ||
				len(op.Parameters) == 0 || op.Parameters[0].Name != "ticket" {
				t.Fatalf("%s: %+v", name, op)
			}
		}
		if _, ok := ws.Responses["101"]; !ok {
			t.Fatalf("ws responses %v", ws.Responses)
		}
		if len(sse.Parameters) != 4 {
			t.Fatalf("sse parameters %+v", sse.Parameters)
		}
		if withAuthn {
			continue
		}
		// The access log, without Authn: open, hold, close.
		h.authorize = openTopics
		conn := s.dialWS("topics=public.a")
		stream, code, _ := s.openSSE("ticket=" + s.ticket() + "&topics=public.a")
		if stream == nil {
			t.Fatal(code)
		}
		eventually(t, "both open", func() bool { return h.Stats().Connections == 2 })
		time.Sleep(150 * time.Millisecond)
		_ = conn.CloseNow()
		_ = stream.resp.Body.Close()
		eventually(t, "both gone", func() bool { return h.Stats().Connections == 0 })
		eventually(t, "two access lines", func() bool {
			found := 0
			for line := range strings.Lines(logs.String()) {
				var e struct {
					Msg, Route string
					Status     int
					Duration   int64 // slog's JSON writes nanoseconds: the logger is injected
				}
				if json.Unmarshal([]byte(line), &e, json.MatchCaseInsensitiveNames(true)) != nil || e.Msg != "access" {
					continue
				}
				d := time.Duration(e.Duration)
				switch {
				case e.Route == "/realtime/ws" && e.Status == 101 && d >= 150*time.Millisecond,
					e.Route == "/realtime/sse" && e.Status == 200 && d >= 150*time.Millisecond:
					found++
				}
			}
			return found == 2
		})
	}
}

// openAPIOp is the part of an OpenAPI operation the tests read.
type openAPIOp struct {
	Summary    string                 `json:"summary"`
	Security   *[]map[string][]string `json:"security"`
	Parameters []struct {
		Name string `json:"name"`
		In   string `json:"in"`
	} `json:"parameters"`
	Responses map[string]any `json:"responses"`
}
