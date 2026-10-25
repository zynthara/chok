package realtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/internal/conf"
)

// The helpers serve a hub through a chok App — its default middleware,
// its router, its OpenAPI document — over real HTTP: httptest for most
// tests, and an in-memory listener inside synctest bubbles, where a fake
// clock drives the heartbeats, the ticket expiry and the slow consumer.

func testLogger(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// testConfig is a Config as chok.Load would give it.
func testConfig(t testing.TB) Config {
	t.Helper()
	var c Config
	if err := conf.Defaults(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// newHub builds a hub on cfg with opts, a test logger unless opts set one.
func newHub(t testing.TB, cfg Config, opts ...Option) *Hub {
	t.Helper()
	h, err := New(cfg, append([]Option{Logger(testLogger(t))}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// appHandler mounts h on an App with every default and returns the App's
// complete handler. adjust may change the App's configuration.
func appHandler(t testing.TB, h *Hub, adjust ...func(*chok.Base)) http.Handler {
	t.Helper()
	var base chok.Base
	if err := conf.Defaults(&base); err != nil {
		t.Fatal(err)
	}
	base.HTTP.Enabled = false
	for _, f := range adjust {
		f(&base)
	}
	app := chok.New(base, chok.Logger(testLogger(t)))
	app.Use(h)
	return app.Handler()
}

// fakeAuthn admits a request with an X-User header as that user, and
// answers 401 otherwise.
func fakeAuthn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-User")
		if id == "" {
			chok.WriteError(w, r, chok.ErrUnauthorized.Msgf("no X-User"))
			return
		}
		next.ServeHTTP(w, r.WithContext(chok.WithPrincipal(r.Context(), chok.Principal{UserID: id, Email: id + "@example.com"})))
	})
}

// openTopics is an Authorize that admits every topic starting with
// public., and refuses the others with a Forbidden of its own.
func openTopics(_ context.Context, topic string) error {
	if strings.HasPrefix(topic, "public.") {
		return nil
	}
	return chok.ErrForbidden.Msgf("%s is not public", topic)
}

// server is a hub served over HTTP.
type server struct {
	t      testing.TB
	hub    *Hub
	url    string // http://host
	client *http.Client
}

// serveHub serves h through an App on an httptest server, closed when
// the test ends (after the hub, so that open streams end first).
func serveHub(t testing.TB, h *Hub, adjust ...func(*chok.Base)) *server {
	t.Helper()
	ts := httptest.NewServer(appHandler(t, h, adjust...))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = h.Close(context.WithoutCancel(t.Context())) })
	return &server{t: t, hub: h, url: ts.URL, client: ts.Client()}
}

// pipeListener is a net.Listener of in-memory connections, which a
// synctest bubble can wait on.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	s, c := net.Pipe()
	select {
	case l.conns <- s:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// servePipe serves h through an App on an in-memory listener, for tests
// inside a synctest bubble. The caller ends it with the returned stop
// before the bubble ends.
func servePipe(t testing.TB, h *Hub) (s *server, stop func()) {
	t.Helper()
	l := &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	srv := &http.Server{Handler: appHandler(t, h)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(l)
	}()
	tr := &http.Transport{DialContext: l.dial}
	stop = func() {
		_ = h.Close(context.WithoutCancel(t.Context()))
		_ = srv.Close()
		<-done
		tr.CloseIdleConnections()
	}
	return &server{t: t, hub: h, url: "http://pipe", client: &http.Client{Transport: tr}}, stop
}

// ticket gets a ticket, sending the header pairs.
func (s *server) ticket(headers ...string) string {
	s.t.Helper()
	code, body := s.do(http.MethodPost, "/realtime/ticket", headers...)
	if code != http.StatusOK {
		s.t.Fatalf("ticket: %d %s", code, body)
	}
	var tr ticketResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		s.t.Fatal(err)
	}
	return tr.Ticket
}

// do sends a request without a body and returns the status and body.
func (s *server) do(method, path string, headers ...string) (int, []byte) {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), method, s.url+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// envelope is chok's error body.
type envelope struct {
	Code     int            `json:"code"`
	Reason   string         `json:"reason"`
	Message  string         `json:"message"`
	Metadata map[string]any `json:"metadata"`
}

func decodeEnvelope(t testing.TB, body []byte) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an envelope: %s: %v", body, err)
	}
	return e
}

// wsURL is the ws endpoint's URL with query.
func (s *server) wsURL(query string) string {
	return "ws" + strings.TrimPrefix(s.url, "http") + "/realtime/ws?" + query
}

// dialWS opens a WebSocket with a fresh ticket of the header pairs and
// the extra query, closed when the test ends.
func (s *server) dialWS(query string, headers ...string) *websocket.Conn {
	s.t.Helper()
	return s.dialWSWith(&websocket.DialOptions{}, query, headers...)
}

// dialWSWith is dialWS with the client's options.
func (s *server) dialWSWith(opts *websocket.DialOptions, query string, headers ...string) *websocket.Conn {
	s.t.Helper()
	q := "ticket=" + s.ticket(headers...)
	if query != "" {
		q += "&" + query
	}
	opts.HTTPClient = s.client
	ws, resp, err := websocket.Dial(s.t.Context(), s.wsURL(q), opts)
	if err != nil {
		s.t.Fatalf("dial: %v (%v)", err, resp)
	}
	s.t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

// send writes a client frame.
func send(t testing.TB, ws *websocket.Conn, frame string) {
	t.Helper()
	if err := ws.Write(t.Context(), websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("send %s: %v", frame, err)
	}
}

// readEvent reads one event frame.
func readEvent(t testing.TB, ws *websocket.Conn) Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	typ, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read an event: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("a %v frame", typ)
	}
	var ev Event
	if err := json.Unmarshal(data, &ev, json.RejectUnknownMembers(true)); err != nil {
		t.Fatalf("frame %s: %v", data, err)
	}
	return ev
}

// closeStatus reads until the connection closes and returns its close
// code.
func closeStatus(t testing.TB, ws *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// publish publishes or fails the test.
func publish(t testing.TB, h *Hub, topic, typ string, data any) {
	t.Helper()
	if err := h.Publish(t.Context(), topic, typ, data); err != nil {
		t.Fatal(err)
	}
}

// sseClient reads a stream.
type sseClient struct {
	resp *http.Response
	r    *bufio.Reader
}

// sseBlock is one block of a stream: an event, a comment or the retry
// field.
type sseBlock struct {
	id, event, data, comment, retry string
}

// openSSE opens the sse endpoint with query and header pairs; the stream
// is closed when the test ends.
func (s *server) openSSE(query string, headers ...string) (*sseClient, int, []byte) {
	s.t.Helper()
	ctx, cancel := context.WithCancel(s.t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/realtime/sse?"+query, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close(); cancel() }()
		body, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, body
	}
	s.t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return &sseClient{resp: resp, r: bufio.NewReader(resp.Body)}, resp.StatusCode, nil
}

// next reads the next block.
func (c *sseClient) next() (sseBlock, error) {
	var b sseBlock
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return b, err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if b != (sseBlock{}) {
				return b, nil
			}
		case strings.HasPrefix(line, ":"):
			b.comment = line
		case strings.HasPrefix(line, "id: "):
			b.id = line[len("id: "):]
		case strings.HasPrefix(line, "event: "):
			b.event = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			b.data = line[len("data: "):]
		case strings.HasPrefix(line, "retry: "):
			b.retry = line[len("retry: "):]
		default:
			return b, errors.New("unexpected line " + line)
		}
	}
}

// nextEvent reads the next event, skipping comments, and decodes its
// data.
func (c *sseClient) nextEvent(t testing.TB) (sseBlock, Event) {
	t.Helper()
	for {
		b, err := c.next()
		if err != nil {
			t.Fatalf("read the stream: %v", err)
		}
		if b.comment != "" || b.retry != "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(b.data), &ev, json.RejectUnknownMembers(true)); err != nil {
			t.Fatalf("data %s: %v", b.data, err)
		}
		return b, ev
	}
}

// syncBuffer collects log output from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// subscribers counts the connections subscribed to topic. A WebSocket
// subscribes after its upgrade, so a test that publishes right after
// dialing waits for it.
func subscribers(h *Hub, topic string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.topics[topic])
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
