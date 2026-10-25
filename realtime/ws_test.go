package realtime

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/zynthara/chok/v3"
)

// TestWS_SubscribeAndReceive: the topics of the query and of a subscribe
// frame both subscribe; a published event arrives as one JSON frame with
// its ID, topic, type, data and time; unsubscribe stops the topic.
func TestWS_SubscribeAndReceive(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	s := serveHub(t, h)
	ws := s.dialWS("topics=public.a")
	send(t, ws, `{"subscribe":["public.b"]}`)
	eventually(t, "two subscriptions", func() bool { return h.Stats().Topics == 2 })

	before := time.Now().UTC()
	publish(t, h, "public.a", "order.updated", struct { // a struct: a map's keys come out in any order
		ID    string `json:"id"`
		Total int    `json:"total"`
	}{"ord_1", 12})
	ev := readEvent(t, ws)
	if ev.Topic != "public.a" || ev.Type != "order.updated" || string(ev.Data) != `{"id":"ord_1","total":12}` ||
		!strings.HasPrefix(ev.ID, h.origin+"-") || ev.At.Location() != time.UTC || ev.At.Before(before.Add(-time.Second)) {
		t.Fatalf("event %+v (%s)", ev, ev.Data)
	}
	publish(t, h, "public.b", "nothing", nil)
	if ev := readEvent(t, ws); ev.Topic != "public.b" || ev.Data != nil {
		t.Fatalf("an event without data: %+v (%s)", ev, ev.Data)
	}

	send(t, ws, `{"unsubscribe":["public.a"]}`)
	eventually(t, "one subscription left", func() bool { return h.Stats().Topics == 1 })
	publish(t, h, "public.a", "missed", nil)
	publish(t, h, "public.b", "seen", 1)
	if ev := readEvent(t, ws); ev.Type != "seen" {
		t.Fatalf("after unsubscribing public.a got %+v", ev)
	}
}

// TestWS_RefusedSubscriptionGetsARefusedEvent: a refused topic is a
// refused event naming the topic — the Authorize error's reason and
// message when it is a *chok.Error or a chok.Coder, Forbidden otherwise —
// and the connection stays, with its other topics.
func TestWS_RefusedSubscriptionGetsARefusedEvent(t *testing.T) {
	var logs syncBuffer
	h, err := New(testConfig(t), Logger(slogTo(&logs)), Authorize(func(ctx context.Context, topic string) error {
		switch topic {
		case "public.ok":
			return nil
		case "coded":
			return coded{}
		case "plain":
			return errors.New("database is down")
		}
		return chok.ErrNotFound.Msgf("no room %s", topic)
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := serveHub(t, h)
	ws := s.dialWS("")
	send(t, ws, `{"subscribe":["room:9","coded","plain","public.ok"]}`)
	want := map[string][2]string{
		"room:9": {"NotFound", "no room room:9"},
		"coded":  {"Conflict", "the room is full"},
		"plain":  {"Forbidden", "subscribing to plain is not allowed"},
	}
	for range len(want) {
		ev := readEvent(t, ws)
		var data struct{ Reason, Message string }
		if err := json.Unmarshal(ev.Data, &data, json.MatchCaseInsensitiveNames(true)); err != nil {
			t.Fatal(err)
		}
		w, ok := want[ev.Topic]
		if !ok || ev.Type != "refused" || ev.ID != "" || data.Reason != w[0] || data.Message != w[1] {
			t.Fatalf("refused event %+v %+v", ev, data)
		}
		delete(want, ev.Topic)
	}
	publish(t, h, "public.ok", "still.here", nil)
	if ev := readEvent(t, ws); ev.Type != "still.here" {
		t.Fatalf("the accepted topic after the refusals: %+v", ev)
	}
	if out := logs.String(); !strings.Contains(out, "database is down") || strings.Contains(out, "the room is full") {
		t.Fatalf("only the error that names no response is logged:\n%s", out)
	}
}

// coded is an error that is a chok.Coder.
type coded struct{}

func (coded) Error() string         { return "full" }
func (coded) APIError() *chok.Error { return chok.ErrConflict.Msgf("the room is full") }

// slogTo is a logger that writes every level to w.
func slogTo(w *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestWS_ResumesAfterAnEvent: a subscribe frame with after gets the
// events of its topics published since that event, in order, then the
// live ones; the other topics' events are not replayed.
func TestWS_ResumesAfterAnEvent(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	s := serveHub(t, h)
	publish(t, h, "public.a", "a1", nil) // seq 1
	publish(t, h, "public.b", "b1", nil)
	publish(t, h, "public.a", "a2", nil)
	publish(t, h, "public.a", "a3", nil)
	ws := s.dialWS("")
	send(t, ws, `{"subscribe":["public.a"],"after":"`+h.origin+`-1"}`)
	for _, want := range []string{"a2", "a3"} {
		if ev := readEvent(t, ws); ev.Type != want || ev.Topic != "public.a" {
			t.Fatalf("replayed %+v, want %s", ev, want)
		}
	}
	publish(t, h, "public.a", "a4", nil)
	if ev := readEvent(t, ws); ev.Type != "a4" || ev.ID != h.origin+"-5" {
		t.Fatalf("live after the replay: %+v", ev)
	}
	if st := h.Stats(); st.Delivered != 3 {
		t.Fatalf("delivered %d, want 2 replayed and 1 live", st.Delivered)
	}
}

// TestWS_ResetWhenTheEventIsGone: resuming after an event the replay
// buffer no longer holds — evicted, unknown, or resuming turned off —
// gets one reset event per topic subscribed, then the live events.
func TestWS_ResetWhenTheEventIsGone(t *testing.T) {
	for _, c := range []struct {
		name   string
		replay int
		after  func(h *Hub) string
	}{
		{"evicted", 2, func(h *Hub) string { return h.origin + "-1" }},
		{"unknown", 256, func(*Hub) string { return "feedfacefeed-7" }},
		{"replay off", 0, func(h *Hub) string { return h.origin + "-3" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Replay = c.replay
			h := newHub(t, cfg, Authorize(openTopics))
			s := serveHub(t, h)
			for range 3 {
				publish(t, h, "public.a", "old", nil)
			}
			ws := s.dialWS("")
			send(t, ws, `{"subscribe":["public.a","public.b"],"after":"`+c.after(h)+`"}`)
			resets := map[string]bool{}
			for range 2 {
				ev := readEvent(t, ws)
				if ev.Type != "reset" || ev.ID != "" || ev.Data != nil {
					t.Fatalf("want a reset, got %+v", ev)
				}
				resets[ev.Topic] = true
			}
			if !resets["public.a"] || !resets["public.b"] {
				t.Fatalf("resets for %v", resets)
			}
			publish(t, h, "public.b", "new", nil)
			if ev := readEvent(t, ws); ev.Type != "new" {
				t.Fatalf("after the resets: %+v", ev)
			}
		})
	}
}

// TestWS_SlowConsumerClosedWith4408: a connection with SendBuffer events
// waiting is dropped on the next one — close code 4408, Stats.Dropped —
// and the publisher never waits for it. In a synctest bubble on an
// in-memory pipe, which buffers nothing: the client not reading holds the
// first event in the server's write.
func TestWS_SlowConsumerClosedWith4408(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t)
		cfg.SendBuffer = 2
		h := newHub(t, cfg, Authorize(openTopics))
		s, stop := servePipe(t, h)
		defer stop()
		ws := s.dialWS("topics=public.a")
		synctest.Wait()
		for i := range 4 {
			publish(t, h, "public.a", "tick", i) // 0 is in the write, 1 and 2 wait, 3 overflows
			synctest.Wait()
		}
		if st := h.Stats(); st.Dropped != 1 || st.Delivered != 3 || st.Topics != 0 {
			t.Fatalf("stats after the overflow: %+v", st)
		}
		if ev := readEvent(t, ws); string(ev.Data) != "0" {
			t.Fatalf("the event in flight: %+v", ev)
		}
		_, _, err := ws.Read(t.Context())
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != 4408 || ce.Reason != "slow consumer" {
			t.Fatalf("closed with %v, want 4408 slow consumer", err)
		}
		synctest.Wait()
		if st := h.Stats(); st.Connections != 0 {
			t.Fatalf("the dropped connection is still counted: %+v", st)
		}
	})
}

// TestWS_FramesCannotOverfillTheQueue: what a subscribe frame queues —
// replays, refused and reset events — does not count against SendBuffer,
// but everything waiting is capped (SendBuffer + Replay + two per
// allowed topic), so a client that sends frames and never reads is
// dropped as a slow consumer rather than growing the queue without end.
func TestWS_FramesCannotOverfillTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t)
		cfg.SendBuffer, cfg.Replay, cfg.MaxSubscriptions = 2, 4, 2 // a cap of 10
		h := newHub(t, cfg, Authorize(openTopics))
		s, stop := servePipe(t, h)
		defer stop()
		for range 4 {
			publish(t, h, "public.a", "old", nil)
		}
		ws := s.dialWS("") // never reads: the first event stays in the server's write
		after := `"after":"` + h.origin + `-1"`
		for round := 1; h.Stats().Dropped == 0; round++ {
			if round > 10 {
				t.Fatalf("still connected after %d rounds: %+v", round, h.Stats())
			}
			send(t, ws, `{"subscribe":["public.a"],`+after+`}`) // three replays a round
			send(t, ws, `{"unsubscribe":["public.a"]}`)
			synctest.Wait()
		}
		if st := h.Stats(); st.Dropped != 1 || st.Topics != 0 || st.Connections != 1 {
			t.Fatalf("stats %+v", st)
		}
		// Its writer is stuck on the first event, so the 4408 cannot go
		// out: the connection is cut once closeGrace has passed.
		time.Sleep(closeGrace)
		synctest.Wait()
		if n := h.Stats().Connections; n != 0 {
			t.Fatalf("%d connections %s after the drop", n, closeGrace)
		}
	})
}

// TestWS_MissedPongsDisconnect: the hub pings every PingInterval; a
// client that does not answer within two intervals is cut off, one that
// answers stays.
func TestWS_MissedPongsDisconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t)
		cfg.PingInterval = 10 * time.Second
		h := newHub(t, cfg, Authorize(openTopics))
		s, stop := servePipe(t, h)
		defer stop()
		alive := s.dialWS("")
		deaf := s.dialWSWith(&websocket.DialOptions{OnPingReceived: func(context.Context, []byte) bool { return false }}, "")
		for _, ws := range []*websocket.Conn{alive, deaf} {
			go func() { _, _, _ = ws.Read(t.Context()) }() // a reading client gets the pings
		}
		synctest.Wait()
		if n := h.Stats().Connections; n != 2 {
			t.Fatalf("%d connections", n)
		}
		// The first ping goes out at 10s and waits for its pong until 30s.
		time.Sleep(29 * time.Second)
		synctest.Wait()
		if n := h.Stats().Connections; n != 2 {
			t.Fatalf("%d connections at 29s, want both", n)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n := h.Stats().Connections; n != 1 {
			t.Fatalf("%d connections at 31s, want the one that answers", n)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if n := h.Stats().Connections; n != 1 {
			t.Fatalf("%d connections after 5 minutes of pings, want the one that answers", n)
		}
	})
}

// TestWS_CloseSaysGoingAway: Close ends every WebSocket with 1001 and
// waits for them; afterwards the endpoints answer 503.
func TestWS_CloseSaysGoingAway(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	s := serveHub(t, h)
	ws := s.dialWS("topics=public.a")
	eventually(t, "the subscription", func() bool { return h.Stats().Topics == 1 })
	closed := make(chan error, 1)
	go func() { closed <- h.Close(t.Context()) }()
	if code := closeStatus(t, ws); code != websocket.StatusGoingAway {
		t.Fatalf("closed with %d, want 1001", code)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if st := h.Stats(); st.Connections != 0 || st.Topics != 0 {
		t.Fatalf("after Close: %+v", st)
	}
	_, resp, err := websocket.Dial(t.Context(), s.wsURL("ticket="+s.ticket()), &websocket.DialOptions{HTTPClient: s.client})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial after Close: %v %v", err, resp)
	}
}

// TestClose_WithinItsContext: Close says 1001 to every connection and
// waits for them within its context — app.shutdown_timeout in an App — so
// a client that stops reading cannot hold the stop: Close returns at the
// deadline naming what is still closing, and the connection goes when
// its close frame gives up (5 seconds, coder/websocket's limit).
func TestClose_WithinItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, testConfig(t), Authorize(openTopics))
		s, stop := servePipe(t, h)
		defer stop()
		reading := s.dialWS("topics=public.a")
		code := make(chan websocket.StatusCode, 1)
		go func() { code <- closeStatus(t, reading) }()
		s.dialWS("topics=public.a") // never reads: the close frame cannot be written
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		begin := time.Now()
		err := h.Close(ctx)
		if err == nil || !strings.Contains(err.Error(), "with 1 connections still closing") || time.Since(begin) != 2*time.Second {
			t.Fatalf("Close returned %v after %s", err, time.Since(begin))
		}
		if c := <-code; c != websocket.StatusGoingAway {
			t.Fatalf("the reading client got %d, want 1001", c)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if n := h.Stats().Connections; n != 0 {
			t.Fatalf("%d connections 5s after Close", n)
		}
	})
}

// TestWS_OriginAllowlist: a browser's Origin must be the server's own
// host or match AllowedOrigins, else the upgrade is a 403 envelope and
// the ticket stays good; "*" admits any origin; a client without Origin
// is not a browser and passes.
func TestWS_OriginAllowlist(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowedOrigins = []string{"app.example.com", "https://*.trusted.dev"}
	s := serveHub(t, newHub(t, cfg))
	dial := func(origin, ticket string) (*http.Response, error) {
		hdr := http.Header{}
		if origin != "" {
			hdr.Set("Origin", origin)
		}
		ws, resp, err := websocket.Dial(t.Context(), s.wsURL("ticket="+ticket), &websocket.DialOptions{HTTPClient: s.client, HTTPHeader: hdr})
		if err == nil {
			_ = ws.Close(websocket.StatusNormalClosure, "")
		}
		return resp, err
	}
	for _, origin := range []string{"", s.url, "http://app.example.com", "https://APP.example.com", "https://x.trusted.dev"} {
		if _, err := dial(origin, s.ticket()); err != nil {
			t.Fatalf("origin %q refused: %v", origin, err)
		}
	}
	for _, origin := range []string{"http://evil.example", "http://x.trusted.dev", "https://app.example.com.evil"} {
		ticket := s.ticket()
		resp, err := dial(origin, ticket)
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: %v %v", origin, err, resp)
		}
		var body [512]byte
		n, _ := resp.Body.Read(body[:])
		if e := decodeEnvelope(t, body[:n]); e.Reason != "Forbidden" || !strings.Contains(e.Message, origin) {
			t.Fatalf("origin %q: %+v", origin, e)
		}
		if _, err := dial("", ticket); err != nil {
			t.Fatalf("the ticket of a refused origin was spent: %v", err)
		}
	}

	cfg.AllowedOrigins = []string{"*"}
	any := serveHub(t, newHub(t, cfg))
	ws, _, err := websocket.Dial(t.Context(), any.wsURL("ticket="+any.ticket()),
		&websocket.DialOptions{HTTPClient: any.client, HTTPHeader: http.Header{"Origin": {"http://anywhere.example"}}})
	if err != nil {
		t.Fatalf(`"*" refused an origin: %v`, err)
	}
	_ = ws.CloseNow()
}

// TestWS_PolicyViolationsClose1008: frames other than subscribe and
// unsubscribe, bad topics, after without subscribe, and more topics than
// MaxSubscriptions close the connection with 1008 and a reason.
func TestWS_PolicyViolationsClose1008(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxSubscriptions = 2
	h := newHub(t, cfg, Authorize(openTopics))
	s := serveHub(t, h)
	for _, c := range []struct {
		query, frame string
		binary       bool
		reason       string
	}{
		{frame: `{"subscribe":["public.a","public.b","public.c"]}`, reason: reasonTooMany},
		{query: "topics=public.a,public.b,public.c", reason: reasonTooMany},
		{query: "topics=public.a", frame: `{"subscribe":["public.a","public.b","public.c"]}`, reason: reasonTooMany},
		{frame: `{"subscribe":["bad topic"]}`, reason: reasonTopic},
		{frame: `{"unsubscribe":["bad/topic"]}`, reason: reasonTopic},
		{frame: `{"unsubscribe":["public.a"],"after":"x-1"}`, reason: reasonNoAfter},
		{frame: `{"subscribe":["public.a"],"unsubscribe":["public.b"]}`, reason: reasonFrame},
		{frame: `{"subscribe":[]}`, reason: reasonFrame},
		{frame: `{}`, reason: reasonFrame},
		{frame: `{"subscribe":["public.a"],"extra":1}`, reason: reasonBadFrame},
		{frame: `not json`, reason: reasonBadFrame},
		{frame: `{"subscribe":["public.a"]}`, binary: true, reason: reasonFrame},
	} {
		ws := s.dialWS(c.query)
		if c.frame != "" {
			typ := websocket.MessageText
			if c.binary {
				typ = websocket.MessageBinary
			}
			if err := ws.Write(t.Context(), typ, []byte(c.frame)); err != nil {
				t.Fatal(err)
			}
		}
		_, _, err := ws.Read(t.Context())
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != c.reason {
			t.Fatalf("%q %q: closed with %v, want 1008 %q", c.query, c.frame, err, c.reason)
		}
	}
	// Re-subscribing to a topic held does not count twice.
	ws := s.dialWS("topics=public.a,public.b")
	send(t, ws, `{"subscribe":["public.a","public.b"]}`)
	eventually(t, "the subscriptions", func() bool { return subscribers(h, "public.b") == 1 })
	publish(t, h, "public.b", "fine", nil)
	if ev := readEvent(t, ws); ev.Type != "fine" {
		t.Fatalf("%+v", ev)
	}
}

// TestWS_RequestErrors: before the upgrade, a closed hub is a 503, a
// request that is not an upgrade a 400 that leaves the ticket unspent, and
// a missing, malformed or spent ticket a 401 — all error envelopes.
func TestWS_RequestErrors(t *testing.T) {
	s := serveHub(t, newHub(t, testConfig(t)))
	ticket := s.ticket()
	if code, body := s.do(http.MethodGet, "/realtime/ws?ticket="+ticket); code != http.StatusBadRequest ||
		decodeEnvelope(t, body).Message != "a WebSocket upgrade request is required" {
		t.Fatalf("plain GET: %d %s", code, body)
	}
	dial := func(query string) (int, envelope) {
		ws, resp, err := websocket.Dial(t.Context(), s.wsURL(query), &websocket.DialOptions{HTTPClient: s.client})
		if err == nil {
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return http.StatusSwitchingProtocols, envelope{}
		}
		var body [512]byte
		n, _ := resp.Body.Read(body[:])
		return resp.StatusCode, decodeEnvelope(t, body[:n])
	}
	if code, _ := dial("ticket=" + ticket); code != http.StatusSwitchingProtocols {
		t.Fatalf("the ticket after the plain GET: %d", code)
	}
	for _, q := range []string{"", "ticket=", "ticket=nope", "ticket=" + ticket, "ticket=" + strings.Repeat("A", 43)} {
		if code, e := dial(q); code != http.StatusUnauthorized || e.Reason != "Unauthorized" {
			t.Fatalf("%q: %d %+v", q, code, e)
		}
	}
}
