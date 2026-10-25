package realtime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/zynthara/chok/v3"
)

// The two transports.
const (
	kindWS  = "ws"
	kindSSE = "sse"
)

// The codes a connection ends with: the WebSocket close codes, and
// closeDead for a peer that stopped answering, closed without a
// handshake.
const (
	closeNormal    = 1000
	closeGoingAway = 1001
	closePolicy    = 1008
	closeSlow      = 4408
	closeDead      = -1
)

// conn is one WebSocket or SSE connection as the hub sees it: its topics
// and its send queue. The transport's goroutine drains the queue.
type conn struct {
	kind   string
	userID string       // the ticket's principal's, "" when anonymous
	srv    *http.Server // SSE: the server the stream is on, for its shutdown
	log    *slog.Logger // carries the request id
	limit  int          // live events that may wait: Config.SendBuffer

	topics map[string]bool // guarded by Hub.mu

	mu     sync.Mutex
	queue  []queued
	live   int // live events in queue; replays and the hub's own do not count
	wake   chan struct{}
	done   chan struct{} // closed by end
	ended  bool
	code   int
	reason string
	abort  func() // SSE: unblocks a write stuck on a peer that stopped reading
}

type queued struct {
	f    *frame
	live bool
}

func newConn(kind string, c claims, srv *http.Server, log *slog.Logger, limit int) *conn {
	return &conn{
		kind: kind, userID: c.userID(), srv: srv, log: log, limit: limit,
		topics: map[string]bool{},
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// What push did with an event.
const (
	queuedOK = iota // on the queue
	full            // refused: the connection is a slow consumer
	gone            // dropped: the connection has ended
)

// push queues f. A live event that finds limit events waiting is refused:
// the connection is a slow consumer. An ended connection takes nothing.
func (c *conn) push(f *frame, live bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return gone
	}
	if live && c.live >= c.limit {
		return full
	}
	c.queue = append(c.queue, queued{f: f, live: live})
	if live {
		c.live++
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return queuedOK
}

// pop takes the oldest queued event, unless the connection has ended.
func (c *conn) pop() (*frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended || len(c.queue) == 0 {
		return nil, false
	}
	q := c.queue[0]
	c.queue[0] = queued{}
	c.queue = c.queue[1:]
	if q.live {
		c.live--
	}
	return q.f, true
}

// end makes the transport close the connection with code and reason; the
// first call wins and reports true.
func (c *conn) end(code int, reason string) bool {
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return false
	}
	c.ended, c.code, c.reason = true, code, reason
	c.queue = nil
	abort := c.abort
	c.mu.Unlock()
	close(c.done)
	if abort != nil {
		abort()
	}
	return true
}

// ending returns the code and reason end was called with.
func (c *conn) ending() (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code, c.reason
}

// register adds c to the hub, unless the hub is closed.
func (h *Hub) register(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c] = struct{}{}
	if c.userID != "" {
		h.users[c.userID]++
	}
	h.sessions.Add(1)
	return true
}

// unregister removes c and its subscriptions once its transport is done.
func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	h.detachLocked(c, nil)
	delete(h.conns, c)
	if c.userID != "" {
		if h.users[c.userID]--; h.users[c.userID] <= 0 {
			delete(h.users, c.userID)
		}
	}
	h.mu.Unlock()
	h.sessions.Done()
}

// fresh returns topics without duplicates and without those c already
// has, and how many more c may take.
func (h *Hub) fresh(c *conn, topics []string) ([]string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, t := range topics {
		if !c.topics[t] && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out, h.cfg.MaxSubscriptions - len(c.topics)
}

// refusal is a subscription Authorize, or the user: rule, turned down.
type refusal struct {
	topic string
	err   *chok.Error
}

// check sorts topics into those the connection may subscribe to and the
// refused ones. It runs Authorize outside the hub's lock: it may query a
// database. ctx carries the ticket's principal.
func (h *Hub) check(ctx context.Context, c *conn, topics []string) (ok []string, refused []refusal) {
	for _, t := range topics {
		if err := h.allowed(ctx, c, t); err != nil {
			refused = append(refused, refusal{topic: t, err: err})
			continue
		}
		ok = append(ok, t)
	}
	return ok, refused
}

// allowed applies the rules to one topic: user:<id> belongs to that user
// alone; any other topic is Authorize's call, refused without one.
func (h *Hub) allowed(ctx context.Context, c *conn, topic string) *chok.Error {
	if id, ok := strings.CutPrefix(topic, "user:"); ok {
		if c.userID != "" && id == c.userID {
			return nil
		}
		return chok.ErrForbidden.Msgf("%s is another user's topic", topic)
	}
	if h.authorize == nil {
		return chok.ErrForbidden.Msgf("subscribing to %s is not allowed", topic)
	}
	err := h.authorize(ctx, topic)
	if err == nil {
		return nil
	}
	if e, ok := errors.AsType[*chok.Error](err); ok {
		return e
	}
	if cd, ok := errors.AsType[interface {
		error
		chok.Coder
	}](err); ok {
		if e := cd.APIError(); e != nil {
			return e
		}
	}
	c.log.WarnContext(ctx, "realtime: Authorize refused a subscription with an error that names no response; the client sees Forbidden",
		"topic", topic, "error", err)
	return chok.ErrForbidden.Msgf("subscribing to %s is not allowed", topic)
}

// attach subscribes c to topics and queues, in one step under the lock
// so that nothing published meanwhile is lost or doubled: an error event
// per refusal, then — when the client resumes after an event — the
// events of those topics since then, or a reset event per topic when the
// event is no longer kept.
func (h *Hub) attach(c *conn, topics []string, refused []refusal, after string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range refused {
		c.push(h.ownEvent(r.topic, typeError, refusalData{Reason: r.err.Reason, Message: r.err.Message}), false)
	}
	if len(topics) == 0 {
		return
	}
	for _, t := range topics {
		c.topics[t] = true
		subs := h.topics[t]
		if subs == nil {
			subs = map[*conn]bool{}
			h.topics[t] = subs
		}
		subs[c] = true
	}
	if after == "" {
		return
	}
	frames, kept := h.replay.since(after, topics)
	if !kept {
		for _, t := range topics {
			c.push(h.ownEvent(t, typeReset, nil), false)
		}
		return
	}
	for _, f := range frames {
		if c.push(f, false) == queuedOK {
			h.delivered.Add(1)
		}
	}
}

// refusalData is the data of an error event.
type refusalData struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// detach unsubscribes c from topics.
func (h *Hub) detach(c *conn, topics []string) {
	h.mu.Lock()
	h.detachLocked(c, topics)
	h.mu.Unlock()
}

// detachLocked unsubscribes c from topics, from every topic when nil.
func (h *Hub) detachLocked(c *conn, topics []string) {
	if topics == nil {
		for t := range c.topics {
			topics = append(topics, t)
		}
	}
	for _, t := range topics {
		if !c.topics[t] {
			continue
		}
		delete(c.topics, t)
		if subs := h.topics[t]; subs != nil {
			delete(subs, c)
			if len(subs) == 0 {
				delete(h.topics, t)
			}
		}
	}
}

// deliverLocked queues f on its topic's subscribers and returns those
// that could not take it: they are dropped as slow consumers, and the
// caller logs them once the lock is released.
func (h *Hub) deliverLocked(f *frame) []*conn {
	var slow []*conn
	for c := range h.topics[f.topic] {
		switch c.push(f, true) {
		case queuedOK:
			h.delivered.Add(1)
			continue
		case gone:
			continue
		}
		h.detachLocked(c, nil)
		h.dropped.Add(1)
		c.end(closeSlow, "slow consumer")
		slow = append(slow, c)
	}
	return slow
}

// logDropped reports the slow consumers deliverLocked dropped.
func (h *Hub) logDropped(slow []*conn, topic string) {
	for _, c := range slow {
		c.log.Info("realtime: dropped a slow consumer", "transport", c.kind, "user_id", c.userID,
			"topic", topic, "send_buffer", c.limit)
	}
}
