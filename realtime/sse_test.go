package realtime

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/zynthara/chok/v3"
)

// TestSSE_StreamsEventsWithItsHeaders: a stream answers 200 with
// text/event-stream, no-cache and X-Accel-Buffering: no, opens with
// retry: 3000, and writes each event as id, event and a data line holding
// the whole Event.
func TestSSE_StreamsEventsWithItsHeaders(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	s := serveHub(t, h)
	c, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=public.a,public.b")
	if c == nil {
		t.Fatalf("%d %s", code, body)
	}
	for k, v := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if got := c.resp.Header.Get(k); got != v {
			t.Fatalf("%s: %q, want %q", k, got, v)
		}
	}
	if b, err := c.next(); err != nil || b != (sseBlock{retry: "3000"}) {
		t.Fatalf("first block %+v %v, want retry: 3000", b, err)
	}
	eventually(t, "the subscriptions", func() bool { return h.Stats().Topics == 2 })
	publish(t, h, "public.b", "order.updated", map[string]int{"total": 3})
	b, ev := c.nextEvent(t)
	if b.id != ev.ID || b.event != "order.updated" || ev.Topic != "public.b" || ev.Type != "order.updated" ||
		string(ev.Data) != `{"total":3}` || !strings.HasPrefix(ev.ID, h.origin+"-") {
		t.Fatalf("block %+v event %+v", b, ev)
	}
	if st := h.Stats(); st.Connections != 1 || st.Delivered != 1 || st.Published != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// TestSSE_Resumes: the Last-Event-ID header — what EventSource sends when
// it reconnects — or ?last_event_id= for a new EventSource resumes after
// that event; the header wins when both are there.
func TestSSE_Resumes(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	s := serveHub(t, h)
	publish(t, h, "public.a", "a1", nil)
	publish(t, h, "public.b", "b1", nil)
	publish(t, h, "public.a", "a2", nil)
	publish(t, h, "public.a", "a3", nil)
	for _, c := range []struct {
		query   string
		headers []string
		want    []string
	}{
		{"&last_event_id=" + h.origin + "-1", nil, []string{"a2", "a3"}},
		{"", []string{"Last-Event-ID", h.origin + "-3"}, []string{"a3"}},
		{"&last_event_id=" + h.origin + "-1", []string{"Last-Event-ID", h.origin + "-3"}, []string{"a3"}},
	} {
		stream, code, body := s.openSSE("ticket="+s.ticket()+"&topics=public.a"+c.query, c.headers...)
		if stream == nil {
			t.Fatalf("%d %s", code, body)
		}
		for _, want := range c.want {
			if b, ev := stream.nextEvent(t); ev.Type != want || b.id != ev.ID {
				t.Fatalf("%q %v: replayed %+v, want %s", c.query, c.headers, ev, want)
			}
		}
		publish(t, h, "public.a", "live", nil)
		if _, ev := stream.nextEvent(t); ev.Type != "live" {
			t.Fatalf("%q %v: after the replay %+v", c.query, c.headers, ev)
		}
	}
}

// TestSSE_ResetWhenTheEventIsGone: resuming after an event no longer kept
// is one reset event per topic, without an id line, so the browser keeps
// its last event ID.
func TestSSE_ResetWhenTheEventIsGone(t *testing.T) {
	cfg := testConfig(t)
	cfg.Replay = 1
	h := newHub(t, cfg, Authorize(openTopics))
	s := serveHub(t, h)
	publish(t, h, "public.a", "a1", nil)
	publish(t, h, "public.a", "a2", nil)
	stream, code, body := s.openSSE("ticket="+s.ticket()+"&topics=public.a,public.b", "Last-Event-ID", h.origin+"-1")
	if stream == nil {
		t.Fatalf("%d %s", code, body)
	}
	resets := map[string]bool{}
	for range 2 {
		b, ev := stream.nextEvent(t)
		if b.event != "reset" || b.id != "" || ev.Type != "reset" || ev.ID != "" {
			t.Fatalf("want a reset, got %+v %+v", b, ev)
		}
		resets[ev.Topic] = true
	}
	if !resets["public.a"] || !resets["public.b"] {
		t.Fatalf("resets %v", resets)
	}
}

// TestSSE_HeartbeatComments: an idle stream writes a comment line every
// PingInterval.
func TestSSE_HeartbeatComments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t)
		cfg.PingInterval = 10 * time.Second
		h := newHub(t, cfg, Authorize(openTopics))
		s, stop := servePipe(t, h)
		defer stop()
		t0 := time.Now()
		stream, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=public.a")
		if stream == nil {
			t.Fatalf("%d %s", code, body)
		}
		if b, err := stream.next(); err != nil || b.retry != "3000" {
			t.Fatalf("%+v %v", b, err)
		}
		for i := 1; i <= 3; i++ {
			b, err := stream.next()
			if err != nil || b.comment != ": ping" || time.Since(t0) != time.Duration(i)*10*time.Second {
				t.Fatalf("block %+v %v at %s, want a ping at %ds", b, err, time.Since(t0), i*10)
			}
		}
		_ = stream.resp.Body.Close()
		synctest.Wait()
	})
}

// TestSSE_RefusedTopics: refused topics are error events at the start of
// the stream and the others stream on; a stream whose every topic is
// refused does not start: 403, the refusal's message for one topic, every
// topic's reason in the metadata.
func TestSSE_RefusedTopics(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	s := serveHub(t, h)
	stream, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=secret,public.a,hidden")
	if stream == nil {
		t.Fatalf("%d %s", code, body)
	}
	refused := map[string]string{}
	for range 2 {
		b, ev := stream.nextEvent(t)
		var data struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil || b.event != "error" || b.id != "" {
			t.Fatalf("want an error event, got %+v %+v %v", b, ev, err)
		}
		refused[ev.Topic] = data.Reason + ": " + data.Message
	}
	if refused["secret"] != "Forbidden: secret is not public" || refused["hidden"] != "Forbidden: hidden is not public" {
		t.Fatalf("refusals %v", refused)
	}
	publish(t, h, "public.a", "live", nil)
	if _, ev := stream.nextEvent(t); ev.Type != "live" {
		t.Fatalf("%+v", ev)
	}

	_, code, body = s.openSSE("ticket=" + s.ticket() + "&topics=secret")
	if e := decodeEnvelope(t, body); code != http.StatusForbidden || e.Message != "secret is not public" {
		t.Fatalf("one topic, refused: %d %+v", code, e)
	}
	_, code, body = s.openSSE("ticket=" + s.ticket() + "&topics=secret,hidden")
	e := decodeEnvelope(t, body)
	topics, _ := e.Metadata["topics"].(map[string]any)
	if code != http.StatusForbidden || e.Reason != "Forbidden" || topics["secret"] != "Forbidden" || topics["hidden"] != "Forbidden" {
		t.Fatalf("every topic refused: %d %+v", code, e)
	}
}

// TestSSE_RequestErrors: no topics, a bad topic or too many topics is a
// 400 that leaves the ticket unspent; a missing, malformed or spent
// ticket a 401.
func TestSSE_RequestErrors(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxSubscriptions = 2
	s := serveHub(t, newHub(t, cfg, Authorize(openTopics)))
	ticket := s.ticket()
	for _, q := range []string{"", "&topics=", "&topics=a,b%20c", "&topics=public.a,public.b,public.c", "&topics=public.a&topics=public.b&topics=public.c"} {
		_, code, body := s.openSSE("ticket=" + ticket + q)
		if e := decodeEnvelope(t, body); code != http.StatusBadRequest || e.Metadata["field"] != "topics" {
			t.Fatalf("%q: %d %+v", q, code, e)
		}
	}
	// Duplicates count once.
	stream, code, body := s.openSSE("ticket=" + ticket + "&topics=public.a,public.b,public.a")
	if stream == nil {
		t.Fatalf("the unspent ticket: %d %s", code, body)
	}
	for _, q := range []string{"topics=public.a", "ticket=&topics=public.a", "ticket=bad&topics=public.a", "ticket=" + ticket + "&topics=public.a"} {
		_, code, body := s.openSSE(q)
		if e := decodeEnvelope(t, body); code != http.StatusUnauthorized || e.Reason != "Unauthorized" {
			t.Fatalf("%q: %d %+v", q, code, e)
		}
	}
}

// TestSSE_OutlivesTheServerTimeouts: the stream clears the deadline that
// http.write_timeout puts on the connection, so it lasts past it — and
// past http.read_timeout, which the server lifts once the request is read.
func TestSSE_OutlivesTheServerTimeouts(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	ts := httptest.NewUnstartedServer(appHandler(t, h))
	ts.Config.ReadTimeout, ts.Config.WriteTimeout = 200*time.Millisecond, 200*time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = h.Close(context.WithoutCancel(t.Context())) })
	s := &server{t: t, hub: h, url: ts.URL, client: ts.Client()}
	stream, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=public.a")
	if stream == nil {
		t.Fatalf("%d %s", code, body)
	}
	for i := range 3 {
		time.Sleep(300 * time.Millisecond)
		publish(t, h, "public.a", "tick", i)
		if _, ev := stream.nextEvent(t); string(ev.Data) != string(rune('0'+i)) {
			t.Fatalf("event %d: %+v", i, ev)
		}
	}
}

// TestHTTP_RequestTimeout: with http.request_timeout set, a stream ends
// at it — the client reconnects and resumes — while a WebSocket, hijacked
// from the request, is not bound by it.
func TestHTTP_RequestTimeout(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	s := serveHub(t, h, func(b *chok.Base) { b.HTTP.RequestTimeout = 300 * time.Millisecond })
	ws := s.dialWS("topics=public.a")
	stream, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=public.a")
	if stream == nil {
		t.Fatalf("%d %s", code, body)
	}
	begin := time.Now()
	for {
		if _, err := stream.next(); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("the stream ended with %v", err)
			}
			break
		}
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("the stream lasted %s past a 300ms request timeout", d)
	}
	time.Sleep(300 * time.Millisecond)
	publish(t, h, "public.a", "still.open", nil)
	if ev := readEvent(t, ws); ev.Type != "still.open" {
		t.Fatalf("%+v", ev)
	}
}

// TestSSE_EndsWhenTheServerShutsDown: http.Server.Shutdown waits for open
// requests; the hub's shutdown hook ends the server's streams so that it
// does not wait for them until its timeout, while WebSockets — hijacked,
// outside Shutdown — wait for Close and its 1001.
func TestSSE_EndsWhenTheServerShutsDown(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	ts := httptest.NewServer(appHandler(t, h))
	t.Cleanup(ts.Close)
	s := &server{t: t, hub: h, url: ts.URL, client: ts.Client()}
	ws := s.dialWS("topics=public.a")
	stream, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=public.a")
	if stream == nil {
		t.Fatalf("%d %s", code, body)
	}
	eventually(t, "both connections", func() bool { return h.Stats().Connections == 2 })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	begin := time.Now()
	if err := ts.Config.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("shutdown took %s", d)
	}
	for {
		if _, err := stream.next(); err != nil {
			break
		}
	}
	publish(t, h, "public.a", "after.shutdown", nil)
	if ev := readEvent(t, ws); ev.Type != "after.shutdown" {
		t.Fatalf("%+v", ev)
	}
	closed := make(chan error, 1)
	go func() { closed <- h.Close(t.Context()) }() // its close handshake needs the client reading
	if code := closeStatus(t, ws); code != websocket.StatusGoingAway {
		t.Fatalf("closed with %d", code)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// TestSSE_SlowConsumerDropped: a stream that falls SendBuffer events
// behind ends, its blocked write cut short, and counts in Dropped.
func TestSSE_SlowConsumerDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t)
		cfg.SendBuffer = 2
		h := newHub(t, cfg, Authorize(openTopics))
		s, stop := servePipe(t, h)
		defer stop()
		stream, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=public.a")
		if stream == nil {
			t.Fatalf("%d %s", code, body)
		}
		if b, err := stream.next(); err != nil || b.retry != "3000" {
			t.Fatalf("%+v %v", b, err)
		}
		// The client stops reading: a large event fills what the pipe and
		// the server's buffers take, the next ones wait, one more overflows.
		big := strings.Repeat("x", 64<<10)
		for i := 0; h.Stats().Dropped == 0 && i < 10; i++ {
			publish(t, h, "public.a", "big", big)
			synctest.Wait()
		}
		synctest.Wait()
		if st := h.Stats(); st.Dropped != 1 || st.Connections != 0 {
			t.Fatalf("stats %+v", st)
		}
	})
}
