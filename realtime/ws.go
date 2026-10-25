package realtime

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zynthara/chok/v3"
)

// serveWS serves GET {Path}/ws?ticket=…[&topics=a,b]. Before the upgrade
// a request answers 403 for an origin outside AllowedOrigins and 401 for
// a bad ticket. After it, the client sends JSON text frames —
// {"subscribe":["a","b"],"after":"<event id>"} and {"unsubscribe":["a"]}
// — and receives one Event per frame. The topics of the query subscribe
// as a first frame would. Anything else, or more than MaxSubscriptions
// topics, closes the connection with 1008; a slow consumer is closed with
// 4408, a client that misses two pings is cut off, and Close says 1001.
func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	switch {
	case h.isClosed():
		chok.WriteError(w, r, errClosing)
		return
	case !upgrading(r):
		chok.WriteError(w, r, errNoUpgrade)
		return
	case !h.originAllowed(r):
		chok.WriteError(w, r, chok.ErrForbidden.Msgf("origin %s may not open a WebSocket here", r.Header.Get("Origin")))
		return
	}
	cl, err := h.redeem(r)
	if err != nil {
		chok.WriteError(w, r, err)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:     h.cfg.AllowedOrigins,
		InsecureSkipVerify: h.anyOrigin, // "*": every origin, which the patterns would spell badly
	})
	if err != nil { // Accept answered the request
		h.connLogger(r.Context()).DebugContext(r.Context(), "realtime: WebSocket upgrade failed", "error", err)
		return
	}
	// The connection outlives the request's deadlines (it is hijacked)
	// and keeps its values: the request id, the logger.
	ctx, cancel := context.WithCancel(withClaims(context.WithoutCancel(r.Context()), cl))
	defer cancel()
	c := h.newConn(kindWS, cl, h.connLogger(ctx))
	if !h.register(c) {
		_ = ws.Close(closeGoingAway, "the server is shutting down")
		return
	}
	defer h.unregister(c)
	var wg sync.WaitGroup
	written := make(chan struct{}) // the writer has closed the connection
	wg.Go(func() { defer close(written); h.wsWrite(ctx, ws, c) })
	wg.Go(func() { h.wsHeartbeat(ctx, ws, c, written) })
	if topics := splitTopics(r.URL.Query()["topics"]); len(topics) > 0 {
		if reason := h.wsSubscribe(ctx, c, topics, ""); reason != "" {
			c.end(closePolicy, reason)
		}
	}
	h.wsRead(ctx, ws, c)
	c.end(closeNormal, "")
	wg.Wait()
}

// upgrading reports whether r asks for a WebSocket.
func upgrading(r *http.Request) bool {
	for _, v := range r.Header.Values("Upgrade") {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "websocket") {
				return true
			}
		}
	}
	return false
}

// originAllowed applies the rule websocket.Accept applies, so that a
// refused origin gets the error envelope: no Origin (not a browser), the
// request's own host, or a match of AllowedOrigins.
func (h *Hub) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || h.anyOrigin {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.EqualFold(r.Host, u.Host) {
		return true
	}
	for _, p := range h.cfg.AllowedOrigins {
		target := u.Host
		if strings.Contains(p, "://") {
			target = u.Scheme + "://" + u.Host
		}
		if ok, _ := path.Match(strings.ToLower(p), strings.ToLower(target)); ok {
			return true
		}
	}
	return false
}

// clientFrame is what a client sends: one of subscribe and unsubscribe.
type clientFrame struct {
	Subscribe   []string `json:"subscribe"`
	Unsubscribe []string `json:"unsubscribe"`
	After       *string  `json:"after"`
}

// The reasons a connection closes with 1008; a close reason is at most
// 123 bytes.
const (
	reasonFrame    = "a frame is a JSON text: subscribe or unsubscribe"
	reasonTopic    = "a topic is 1 to 128 of A-Z a-z 0-9 _ . : -"
	reasonTooMany  = "too many subscriptions"
	reasonNoAfter  = "after goes with subscribe"
	reasonBadFrame = "unreadable frame"
)

// wsRead handles the client's frames until the connection ends or breaks
// the protocol.
func (h *Hub) wsRead(ctx context.Context, ws *websocket.Conn, c *conn) {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			c.end(closePolicy, reasonFrame)
			return
		}
		var f clientFrame
		if err := json.Unmarshal(data, &f, json.RejectUnknownMembers(true)); err != nil {
			c.end(closePolicy, reasonBadFrame)
			return
		}
		reason := ""
		switch {
		case len(f.Subscribe) > 0 && f.Unsubscribe == nil:
			after := ""
			if f.After != nil {
				after = *f.After
			}
			reason = h.wsSubscribe(ctx, c, f.Subscribe, after)
		case len(f.Unsubscribe) > 0 && f.Subscribe == nil:
			switch {
			case f.After != nil:
				reason = reasonNoAfter
			case !allValid(f.Unsubscribe):
				reason = reasonTopic
			default:
				h.detach(c, f.Unsubscribe)
			}
		default:
			reason = reasonFrame
		}
		if reason != "" {
			c.end(closePolicy, reason)
			return
		}
	}
}

func allValid(topics []string) bool {
	for _, t := range topics {
		if !validName(t) {
			return false
		}
	}
	return true
}

// wsSubscribe subscribes c to topics, resuming after an event when after
// is set, and returns the reason to close with when the request breaks a
// rule.
func (h *Hub) wsSubscribe(ctx context.Context, c *conn, topics []string, after string) string {
	if !allValid(topics) {
		return reasonTopic
	}
	fresh, room := h.fresh(c, topics)
	if len(fresh) > room {
		return reasonTooMany
	}
	ok, refused := h.check(ctx, c, fresh)
	h.attach(c, ok, refused, after)
	return ""
}

// closeGrace is how long an ended WebSocket may take to close: a write
// stuck on a client that stopped reading holds the writer, which would
// send the close frame after it, so the heartbeat cuts the connection
// once the grace has passed (coder/websocket gives its close frame as
// long).
const closeGrace = 5 * time.Second

// wsWrite sends the queued events until the connection ends, then closes
// it with the code it ended with. A write may block for two ping
// intervals, the time a client has to answer a ping.
func (h *Hub) wsWrite(ctx context.Context, ws *websocket.Conn, c *conn) {
	for {
		select {
		case <-c.done:
			code, reason := c.ending()
			if code == closeDead {
				_ = ws.CloseNow()
				return
			}
			_ = ws.Close(websocket.StatusCode(code), reason)
			return
		case <-c.wake:
		}
		for {
			f, ok := c.pop()
			if !ok {
				break
			}
			wctx, cancel := context.WithTimeout(ctx, 2*h.cfg.PingInterval)
			err := ws.Write(wctx, websocket.MessageText, f.json)
			cancel()
			if err != nil {
				c.end(closeDead, "")
				_ = ws.CloseNow()
				return
			}
		}
	}
}

// wsHeartbeat pings the client every PingInterval; one that does not
// answer within two intervals is cut off without a close handshake. Once
// the connection has ended, it gives the writer closeGrace to close it.
func (h *Hub) wsHeartbeat(ctx context.Context, ws *websocket.Conn, c *conn, written <-chan struct{}) {
	ticker := time.NewTicker(h.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			grace := time.NewTimer(closeGrace)
			defer grace.Stop()
			select {
			case <-written:
			case <-grace.C:
				_ = ws.CloseNow()
			}
			return
		case <-ticker.C:
		}
		pctx, cancel := context.WithTimeout(ctx, 2*h.cfg.PingInterval)
		err := ws.Ping(pctx)
		cancel()
		if err != nil {
			if c.end(closeDead, "") {
				c.log.DebugContext(ctx, "realtime: a WebSocket missed its pong; disconnected", "error", err)
			}
			_ = ws.CloseNow()
			return
		}
	}
}
