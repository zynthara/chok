package realtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/internal/jsonx"
)

// Hub is the realtime battery: the topics, the connections subscribed to
// them over WebSocket or server-sent events, the tickets that open them,
// and, with Redis, the backplane to the other replicas. Register it with
// app.Use: it mounts its endpoints, Start runs the backplane, Close ends
// the connections.
type Hub struct {
	cfg       Config
	authn     chok.Middleware
	authorize func(ctx context.Context, topic string) error
	redis     *goredis.Client
	logger    *slog.Logger
	origin    string // the random half of every event ID this Hub publishes
	anyOrigin bool   // AllowedOrigins holds "*"
	tickets   tickets

	mu       sync.Mutex
	seq      uint64
	conns    map[*conn]struct{}
	topics   map[string]map[*conn]bool
	users    map[string]int // connections per user
	replay   *replay
	servers  map[*http.Server]bool // with a shutdown hook for their streams
	closed   bool
	started  bool
	stopRun  context.CancelFunc // ends Start's backplane loop
	runDone  chan struct{}      // closed when Start returns
	sessions sync.WaitGroup     // the connections' handlers

	published, delivered, dropped atomic.Uint64
	bp                            backplane
}

// Option configures New.
type Option func(*options)

type options struct {
	authn        chok.Middleware
	authnSet     bool
	authorize    func(ctx context.Context, topic string) error
	authorizeSet bool
	redis        *goredis.Client
	logger       *slog.Logger
}

// Authn guards {Path}/ticket with mw — identity's Authn(), typically — so
// that a ticket carries the principal mw puts in the request, and the
// connection it opens acts for that user. Without Authn the ticket
// endpoint is public and its tickets are anonymous, for pages that serve
// guests.
func Authn(mw chok.Middleware) Option {
	return func(o *options) { o.authn, o.authnSet = mw, true }
}

// Authorize decides which topics a connection may subscribe to: fn runs
// once per subscription, with a context that carries the ticket's
// principal (none for an anonymous ticket), and refuses the subscription
// by returning an error. The client then gets a refused event, with the
// reason and message of the error when it is a *chok.Error or a
// chok.Coder, and Forbidden otherwise (that error is logged at Warn); its
// other subscriptions stand. Without Authorize every topic is refused
// except the connection's own user:<UserID>. Topics starting with user:
// always follow that rule: fn never sees them.
func Authorize(fn func(ctx context.Context, topic string) error) Option {
	return func(o *options) { o.authorize, o.authorizeSet = fn, true }
}

// Redis makes the hub one of several replicas sharing client: tickets
// live in Redis, so any replica redeems a ticket and only once, and every
// Publish also reaches the other replicas' subscribers, through PUBLISH
// on Config.Channel. A nil client is the same as no Redis option; the
// client of cache.Redis() can be passed as it is. The hub does not close
// the client.
func Redis(client *goredis.Client) Option {
	return func(o *options) { o.redis = client }
}

// Logger is where the hub logs; the default is slog.Default().
func Logger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// New builds a hub from cfg after checking cfg and every option; nothing
// runs until Start, and nothing is served until Mount.
func New(cfg Config, opts ...Option) (*Hub, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("realtime: %w", err)
	}
	switch {
	case o.authnSet && o.authn == nil:
		return nil, errors.New("realtime: Authn with a nil middleware")
	case o.authorizeSet && o.authorize == nil:
		return nil, errors.New("realtime: Authorize with a nil function")
	case o.redis != nil && cfg.Channel == "":
		return nil, errors.New("realtime: channel is empty; the Redis backplane needs one")
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	var id [6]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read never fails
	h := &Hub{
		cfg: cfg, authn: o.authn, authorize: o.authorize, redis: o.redis, logger: o.logger,
		origin:    hex.EncodeToString(id[:]),
		anyOrigin: slices.Contains(cfg.AllowedOrigins, "*"),
		tickets:   newMemTickets(),
		conns:     map[*conn]struct{}{},
		topics:    map[string]map[*conn]bool{},
		users:     map[string]int{},
		replay:    newReplay(cfg.Replay),
		servers:   map[*http.Server]bool{},
	}
	if o.redis != nil {
		h.tickets = &redisTickets{client: o.redis, prefix: cfg.Channel + ":ticket:"}
		h.bp.state.Store(bpConnecting)
	}
	return h, nil
}

// MustNew is New that panics on error.
func MustNew(cfg Config, opts ...Option) *Hub {
	h, err := New(cfg, opts...)
	if err != nil {
		panic(err)
	}
	return h
}

// Publish sends an event of type typ, with data encoded as JSON (nil
// sends none), to the subscribers of topic — this replica's at once and,
// with Redis, the other replicas' through the backplane. A topic is 1 to
// 128 of A-Z a-z 0-9 _ . : -, and so is typ, which must not be refused
// or reset, the hub's own, nor error, the name of an EventSource's
// connection errors. Publish fails only on a bad topic or type, or
// data that does not encode; no subscriber is not an error, and a
// backplane that does not take the event is logged, not returned: the
// local subscribers have it. It does not block on slow connections: one
// that has Config.SendBuffer events waiting is dropped.
func (h *Hub) Publish(ctx context.Context, topic, typ string, data any) error {
	if !validName(topic) {
		return fmt.Errorf("realtime: publish: topic %q is not %s", topic, nameRule)
	}
	if !validName(typ) || reserved(typ) {
		return fmt.Errorf("realtime: publish to %s: type %q is not %s, or is refused, reset or error, which the hub keeps for itself", topic, typ, nameRule)
	}
	var raw jsontext.Value
	if data != nil {
		b, err := json.Marshal(data, jsonx.Options)
		if err != nil {
			return fmt.Errorf("realtime: publish %s to %s: encode the data: %w", typ, topic, err)
		}
		raw = b
	}
	h.mu.Lock()
	h.seq++
	f := h.newFrame(Event{ID: h.origin + "-" + strconv.FormatUint(h.seq, 10), Topic: topic, Type: typ, Data: raw, At: time.Now().UTC()})
	h.replay.add(f)
	slow := h.deliverLocked(f)
	h.mu.Unlock()
	h.published.Add(1)
	h.logDropped(slow, topic)
	if h.redis != nil {
		h.broadcast(ctx, f)
	}
	return nil
}

// Send publishes to userID's own topic: Publish(ctx, "user:"+userID, typ,
// data). Only that user's connections may subscribe to it.
func (h *Hub) Send(ctx context.Context, userID, typ string, data any) error {
	return h.Publish(ctx, "user:"+userID, typ, data)
}

// newFrame encodes ev once for every connection.
func (h *Hub) newFrame(ev Event) *frame {
	b, err := json.Marshal(ev)
	if err != nil { // an Event of valid names and JSON data always encodes
		panic(fmt.Sprintf("realtime: encode an event: %v", err))
	}
	return &frame{id: ev.ID, topic: ev.Topic, typ: ev.Type, json: b}
}

// ownEvent is one of the hub's own events, refused or reset: it has no
// ID, so a client's last event ID does not move.
func (h *Hub) ownEvent(topic, typ string, data any) *frame {
	ev := Event{Topic: topic, Type: typ, At: time.Now().UTC()}
	if data != nil {
		ev.Data, _ = json.Marshal(data) // a struct of strings
	}
	return h.newFrame(ev)
}

// Stats returns the hub's connections and counters.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	st := Stats{Connections: len(h.conns), Users: len(h.users), Topics: len(h.topics)}
	h.mu.Unlock()
	st.Published, st.Delivered, st.Dropped = h.published.Load(), h.delivered.Load(), h.dropped.Load()
	st.Backplane = h.bp.status()
	return st
}

// Start runs the Redis backplane until ctx ends: it subscribes to
// Config.Channel, delivers what the other replicas publish, and
// reconnects after a disconnect, logging it at Warn. Without Redis there
// is nothing to run and Start returns nil at once.
func (h *Hub) Start(ctx context.Context) error {
	if h.redis == nil {
		return nil
	}
	h.mu.Lock()
	switch {
	case h.closed:
		h.mu.Unlock()
		return errors.New("realtime: Start after Close")
	case h.started:
		h.mu.Unlock()
		return errors.New("realtime: Start called twice")
	}
	runCtx, cancel := context.WithCancel(ctx)
	h.started, h.stopRun, h.runDone = true, cancel, make(chan struct{})
	done := h.runDone
	h.mu.Unlock()
	defer close(done)
	defer cancel()
	h.runBackplane(runCtx)
	return nil
}

// Close ends every connection — a WebSocket with 1001, a stream by
// finishing it — and stops the backplane, then waits within ctx for the
// connections' handlers to return. The App calls it on stop, bounded by
// app.shutdown_timeout. Streams already ended when the HTTP server began
// its shutdown, which waits for open requests.
func (h *Hub) Close(ctx context.Context) error {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		for c := range h.conns {
			c.end(closeGoingAway, "the server is shutting down")
		}
	}
	stop, runDone := h.stopRun, h.runDone
	h.mu.Unlock()
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		h.sessions.Wait()
		if runDone != nil {
			<-runDone
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("realtime: close: %w with %d connections still closing", ctx.Err(), h.Stats().Connections)
	}
}

// Health reports the backplane: an error while it has not subscribed yet
// or has lost its connection, nil once subscribed, and nil without Redis.
// The App waits for it at start, so with Redis an instance takes traffic
// once it hears the other replicas.
func (h *Hub) Health(context.Context) error { return h.bp.health() }

// Name is "realtime", its name in the health report.
func (h *Hub) Name() string { return "realtime" }

func (h *Hub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// connLogger is the hub's logger with the request id of the connection.
func (h *Hub) connLogger(ctx context.Context) *slog.Logger {
	if id := chok.RequestID(ctx); id != "" {
		return h.logger.With("request_id", id)
	}
	return h.logger
}
