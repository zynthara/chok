package realtime

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/zynthara/chok/v3"
)

// serveSSE serves GET {Path}/sse?ticket=…&topics=a,b: the topics are
// checked and the ticket redeemed before anything is streamed, so a bad
// request still gets its error envelope — 400 for the topics, 401 for the
// ticket, 403 when every topic is refused. Then the stream opens with
// retry: 3000, an error event per refused topic, the events to replay or
// a reset per topic when the client resumes (Last-Event-ID, or
// ?last_event_id= for a new EventSource), and the live events, with a
// comment line every PingInterval. It ends when the client goes, at
// http.request_timeout when one is set, when the hub drops a slow
// consumer, and when the server shuts down.
func (h *Hub) serveSSE(w http.ResponseWriter, r *http.Request) {
	if h.isClosed() {
		chok.WriteError(w, r, errClosing)
		return
	}
	q := r.URL.Query()
	topics, err := h.streamTopics(q["topics"])
	if err != nil {
		chok.WriteError(w, r, err)
		return
	}
	cl, err := h.redeem(r)
	if err != nil {
		chok.WriteError(w, r, err)
		return
	}
	ctx := withClaims(r.Context(), cl)
	c := h.newConn(kindSSE, cl, h.connLogger(ctx))
	ok, refused := h.check(ctx, c, topics)
	if len(ok) == 0 {
		chok.WriteError(w, r, allRefused(refused))
		return
	}
	after := r.Header.Get("Last-Event-ID")
	if after == "" {
		after = q.Get("last_event_id")
	}

	// The stream outlives http.write_timeout: the deadline the server put
	// on the connection is cleared, and each write gets one of its own
	// (sseStream.write) instead. A read deadline does not end it: the
	// server clears that when it starts watching for the client to go.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	c.srv = h.watchServer(r)
	c.abort = func() { _ = rc.SetWriteDeadline(time.Now()) }
	if !h.register(c) {
		chok.WriteError(w, r, errClosing)
		return
	}
	defer h.unregister(c)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Subscribed before the first line goes out: a client that has read
	// it misses nothing published after.
	h.attach(c, ok, refused, after)
	s := &sseStream{w: w, rc: rc, c: c, wait: 2 * h.cfg.PingInterval}
	if !s.write([]byte("retry: 3000\n\n")) || !s.drain() {
		return
	}
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c.done:
			return
		case <-c.wake:
			if !s.drain() {
				return
			}
		case <-ping.C:
			if !s.write([]byte(": ping\n\n")) {
				return
			}
		}
	}
}

// streamTopics reads and checks the topics of a stream request: at least
// one, each a valid name, at most MaxSubscriptions after dropping
// duplicates.
func (h *Hub) streamTopics(values []string) ([]string, error) {
	var topics []string
	for _, t := range splitTopics(values) {
		if !validName(t) {
			return nil, chok.ErrBadRequest.Msgf("topic %q is not %s", t, nameRule).With("field", "topics")
		}
		if !slices.Contains(topics, t) {
			topics = append(topics, t)
		}
	}
	switch {
	case len(topics) == 0:
		return nil, chok.ErrBadRequest.Msgf("topics is required: topics=a,b").With("field", "topics")
	case len(topics) > h.cfg.MaxSubscriptions:
		return nil, chok.ErrBadRequest.Msgf("%d topics is more than the %d a connection may have", len(topics), h.cfg.MaxSubscriptions).
			With("field", "topics")
	}
	return topics, nil
}

// allRefused is the 403 of a stream whose every topic was refused: the
// refusal's message when there is one topic, each topic's reason in the
// metadata.
func allRefused(refused []refusal) error {
	reasons := make(map[string]string, len(refused))
	var names []string
	for _, r := range refused {
		reasons[r.topic] = r.err.Reason
		names = append(names, r.topic)
	}
	msg := "no topic may be subscribed: " + strings.Join(names, ", ")
	if len(refused) == 1 {
		msg = refused[0].err.Message
	}
	return chok.ErrForbidden.Msgf("%s", msg).With("topics", reasons)
}

// watchServer registers, once per server, a shutdown hook that ends the
// server's streams: http.Server.Shutdown waits for open requests, and a
// stream would keep it waiting until http.shutdown_timeout.
func (h *Hub) watchServer(r *http.Request) *http.Server {
	srv, _ := r.Context().Value(http.ServerContextKey).(*http.Server)
	if srv == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.servers[srv] {
		h.servers[srv] = true
		srv.RegisterOnShutdown(func() { h.endStreams(srv) })
	}
	return srv
}

// endStreams finishes the streams served by srv.
func (h *Hub) endStreams(srv *http.Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if c.kind == kindSSE && c.srv == srv {
			c.end(closeGoingAway, "the server is shutting down")
		}
	}
}

// sseStream writes a connection's events as server-sent events.
type sseStream struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	c    *conn
	wait time.Duration // how long a write may block: two ping intervals
	buf  bytes.Buffer
}

// drain writes the queued events and flushes them.
func (s *sseStream) drain() bool {
	s.buf.Reset()
	for {
		f, ok := s.c.pop()
		if !ok {
			break
		}
		if f.id != "" {
			fmt.Fprintf(&s.buf, "id: %s\n", f.id)
		}
		fmt.Fprintf(&s.buf, "event: %s\ndata: %s\n\n", f.typ, f.json)
	}
	if s.buf.Len() == 0 {
		return true
	}
	return s.write(s.buf.Bytes())
}

// write writes and flushes b. Each write gets a deadline of its own, so a
// peer that stopped reading cannot hold the handler forever.
func (s *sseStream) write(b []byte) bool {
	select {
	case <-s.c.done:
		return false
	default:
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.wait))
	if _, err := s.w.Write(b); err != nil {
		return false
	}
	return s.rc.Flush() == nil
}
