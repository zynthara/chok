package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/realtime"
)

// TestChatter_FullPath runs the app the way main does — chok.yaml, a real
// HTTP server on a free port, the start and stop phases — on the lane's
// database, and walks README.md's path through HTTP, a WebSocket and a
// server-sent event stream: three users sign up, alice opens a room that
// bob joins and carol does not; bob's WebSocket follows the room while
// carol's is refused; alice's message reaches bob at once and, since it
// mentions @bot, the bot's answer follows from the worker; alice's event
// stream shows the worker's run; bob resumes after a reconnect; the stop
// says goodbye to both connections.
func TestChatter_FullPath(t *testing.T) {
	t.Setenv("CHATTER_AUTH_SIGNING_KEY", strings.Repeat("k", 32))
	cfg, err := chok.Load[Config]("chatter", chok.ConfigFile("chok.yaml"), chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTP.Addr, cfg.HTTP.DrainDelay = "127.0.0.1:0", 0
	cfg.Jobs.PollInterval = 20 * time.Millisecond
	app := chok.New(cfg.Base, chok.Logger(slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn}))))
	setup(app, cfg, choktest.DB(t, tables()...))
	ctx, stop := context.WithCancel(context.WithoutCancel(t.Context()))
	defer stop()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	c := &client{t: t}
	deadline := time.Now().Add(20 * time.Second)
	for c.base = ""; c.base == "" || c.call(http.MethodGet, "/readyz", "", nil, nil) != http.StatusOK; {
		if time.Now().After(deadline) {
			t.Fatal("the app did not start")
		}
		time.Sleep(10 * time.Millisecond)
		if addr := app.Addr(); addr != "" {
			c.base = "http://" + addr
		}
	}

	// The page and the health of every component.
	if code, body := c.get("/"); code != http.StatusOK || !strings.Contains(body, "<title>chatter") {
		t.Fatalf("GET / = %d", code)
	}
	var health struct {
		Status     string `json:"status"`
		Components []struct {
			Component string `json:"component"`
		} `json:"components"`
	}
	if code := c.call(http.MethodGet, "/healthz", "", nil, &health); code != http.StatusOK || health.Status != "up" || len(health.Components) != 4 {
		t.Fatalf("healthz = %d %+v", code, health)
	}

	// Three users; alice opens a room, bob joins it, carol does not.
	alice, bob, carol := c.signUp("alice"), c.signUp("bob"), c.signUp("carol")
	var lobby Room
	if code := c.call(http.MethodPost, "/api/rooms", alice, map[string]string{"name": "lobby"}, &lobby); code != http.StatusCreated || !strings.HasPrefix(lobby.RID, "room_") {
		t.Fatalf("create a room = %d %+v", code, lobby)
	}
	if code := c.call(http.MethodPost, "/api/rooms/"+lobby.RID+"/join", bob, nil, nil); code != http.StatusNoContent {
		t.Fatalf("bob joins = %d", code)
	}
	var e struct {
		Message string `json:"message"`
	}
	if code := c.call(http.MethodPost, "/api/rooms/"+lobby.RID+"/messages", carol, map[string]string{"text": "hi"}, &e); code != http.StatusForbidden || e.Message != "join the room first" {
		t.Fatalf("carol posts = %d %+v", code, e)
	}

	// The jobs panel: alice's stream; the chat: bob's and carol's sockets.
	panel := c.stream("/realtime/sse?topics=jobs&ticket=" + c.ticket(alice))
	bobWS, _ := c.dial(bob, "room:"+lobby.RID, "")
	carolWS, refused := c.dial(carol, "room:"+lobby.RID, "")
	if len(refused) != 1 || refused[0].Type != "refused" || refused[0].Topic != "room:"+lobby.RID || !strings.Contains(string(refused[0].Data), "join the room first") {
		t.Fatalf("carol's subscription: %+v", refused)
	}
	_ = carolWS.CloseNow() // a socket nobody reads would hold the stop for its close handshake

	// alice speaks: bob gets her message, published once it commits, and
	// the bot's answer, which the worker writes and publishes after that
	// commit — almost always second, but nothing orders the two.
	var said Message
	if code := c.call(http.MethodPost, "/api/rooms/"+lobby.RID+"/messages", alice, map[string]string{"text": "hello @bot, how are you"}, &said); code != http.StatusCreated {
		t.Fatalf("alice posts = %d", code)
	}
	first, second := read(t, bobWS), read(t, bobWS)
	var fromAlice, fromBot bool
	for _, ev := range []realtime.Event{first, second} {
		var m Message
		if err := json.Unmarshal(ev.Data, &m); err != nil || ev.Type != "message.created" || ev.Topic != "room:"+lobby.RID {
			t.Fatalf("bob got %+v (%v)", ev, err)
		}
		switch {
		case m.RID == said.RID && m.Author == "alice" && m.Text == "hello @bot, how are you":
			fromAlice = true
		case m.ReplyTo == said.RID && m.Author == "bot" && m.Text == "alice, that was 5 words. Noted.":
			fromBot = true
		}
	}
	if !fromAlice || !fromBot {
		t.Fatalf("bob got %+v and %+v, want alice's message and the bot's answer", first, second)
	}

	// alice's panel shows the worker's run that answered.
	for {
		ev := panel.next(t)
		var run struct {
			Kind, Job, Type string
			Messages        int
		}
		if err := json.Unmarshal(ev.Data, &run, json.MatchCaseInsensitiveNames(true)); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "run.finished" && run.Type == "worker" && run.Job == "bot.reply" && run.Messages == 1 {
			break
		}
	}

	// The history, for members only.
	var history []Message
	if code := c.call(http.MethodGet, "/api/rooms/"+lobby.RID+"/messages", bob, nil, &history); code != http.StatusOK || len(history) != 2 ||
		history[0].Author != "alice" || history[1].Author != "bot" {
		t.Fatalf("history = %d %+v", code, history)
	}
	if code := c.call(http.MethodGet, "/api/rooms/"+lobby.RID+"/messages", carol, nil, nil); code != http.StatusForbidden {
		t.Fatalf("carol reads the history = %d", code)
	}

	// bob's connection drops; he comes back with a new ticket, resuming
	// after the first event he saw: the second is replayed.
	_ = bobWS.CloseNow()
	bobWS, replayed := c.dial(bob, "room:"+lobby.RID, first.ID)
	if len(replayed) != 1 || replayed[0].ID != second.ID {
		t.Fatalf("resumed with %+v, want %s again", replayed, second.ID)
	}

	// The stop: the WebSocket hears 1001, the stream ends, Run returns nil.
	stop()
	if _, _, err := bobWS.Read(t.Context()); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("bob's socket at the stop: %v", err)
	}
	panel.drain(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the app did not stop")
	}
}

// TestChatter_StaysSmall holds the example to its size (contract §10):
// at most 400 lines of application code — the non-test Go files without
// blank and comment lines, counted as hack/apibudget.sh counts
// examples/blog — importing at most 7 chok packages.
func TestChatter_StaysSmall(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	lines, pkgs := 0, map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(src)) {
			if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "//") {
				lines++
			}
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "github.com/zynthara/chok/v3" || strings.HasPrefix(path, "github.com/zynthara/chok/v3/") {
				pkgs[path] = true
			}
		}
	}
	t.Logf("%d lines of application code, %d chok packages", lines, len(pkgs))
	if lines > 400 || len(pkgs) > 7 {
		t.Fatalf("%d lines (at most 400) importing %d chok packages (at most 7): %v", lines, len(pkgs), pkgs)
	}
}

// client speaks to the running app.
type client struct {
	t    *testing.T
	base string
}

// call sends a JSON request with a bearer token, decodes the response
// into out, and returns the status.
func (c *client) call(method, path, token string, in, out any) int {
	c.t.Helper()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			c.t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: %d %s: %v", method, path, resp.StatusCode, raw, err)
		}
	}
	return resp.StatusCode
}

func (c *client) get(path string) (int, string) {
	c.t.Helper()
	resp, err := http.Get(c.base + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// signUp registers name and returns its token.
func (c *client) signUp(name string) string {
	c.t.Helper()
	var tok struct {
		Token string `json:"token"`
	}
	in := map[string]string{"email": name + "@example.com", "password": "password123", "name": name}
	if code := c.call(http.MethodPost, "/auth/register", "", in, &tok); code != http.StatusCreated || tok.Token == "" {
		c.t.Fatalf("register %s = %d", name, code)
	}
	return tok.Token
}

// ticket asks the hub for a ticket as the token's user.
func (c *client) ticket(token string) string {
	c.t.Helper()
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if code := c.call(http.MethodPost, "/realtime/ticket", token, nil, &tk); code != http.StatusOK || tk.Ticket == "" {
		c.t.Fatalf("ticket = %d", code)
	}
	return tk.Ticket
}

// dial opens a WebSocket as the token's user and subscribes to topic,
// resuming after the event after when it is set. The hub does not
// acknowledge a subscription, but it handles a connection's frames in
// order: dial follows with a subscription the app refuses, and once its
// refused event is in, the first frame has been handled. It returns the
// events that came before that refusal: the first subscription's own.
func (c *client) dial(token, topic, after string) (*websocket.Conn, []realtime.Event) {
	c.t.Helper()
	url := "ws" + strings.TrimPrefix(c.base, "http") + "/realtime/ws?ticket=" + c.ticket(token)
	ws, _, err := websocket.Dial(c.t.Context(), url, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = ws.CloseNow() })
	frame := map[string]any{"subscribe": []string{topic}}
	if after != "" {
		frame["after"] = after
	}
	for _, f := range []any{frame, map[string]any{"subscribe": []string{"barrier"}}} {
		b, _ := json.Marshal(f)
		if err := ws.Write(c.t.Context(), websocket.MessageText, b); err != nil {
			c.t.Fatal(err)
		}
	}
	var before []realtime.Event
	for {
		ev := read(c.t, ws)
		if ev.Topic == "barrier" && ev.Type == "refused" {
			return ws, before
		}
		before = append(before, ev)
	}
}

func read(t *testing.T, ws *websocket.Conn) realtime.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	_, b, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read the socket: %v", err)
	}
	var ev realtime.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// sse reads a server-sent event stream.
type sse struct {
	resp *http.Response
	r    *bufio.Reader
}

func (c *client) stream(path string) *sse {
	c.t.Helper()
	// A stream that stops delivering fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(c.t.Context(), time.Minute)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		c.t.Fatalf("open %s: %v %v", path, err, resp)
	}
	c.t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return &sse{resp: resp, r: bufio.NewReader(resp.Body)}
}

// next returns the next event's data line, decoded.
func (s *sse) next(t *testing.T) realtime.Event {
	t.Helper()
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			t.Fatalf("read the stream: %v", err)
		}
		if data, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "data: "); ok {
			var ev realtime.Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatal(err)
			}
			return ev
		}
	}
}

// drain reads the stream to its end, which must come.
func (s *sse) drain(t *testing.T) {
	t.Helper()
	if _, err := io.Copy(io.Discard, s.r); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("the stream ended with %v", err)
	}
}
