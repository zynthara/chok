package realtime

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	// go-redis reports its own trouble through a process-wide logger;
	// internal/redislog routes it to slog.
	_ "github.com/zynthara/chok/v3/internal/redislog"
)

// The states of the backplane.
const (
	bpNone       int32 = iota // no Redis
	bpConnecting              // not subscribed yet
	bpUp                      // subscribed
	bpDown                    // lost, reconnecting
)

// The backoff between reconnect attempts.
const (
	minBackoff = 50 * time.Millisecond
	maxBackoff = 5 * time.Second
)

// backplane is the state of the Redis subscription, for Health and
// Stats, and of the publishing side, for its log lines.
type backplane struct {
	state      atomic.Int32
	mu         sync.Mutex
	lastErr    error
	everUp     bool
	pubFailing atomic.Bool
}

func (b *backplane) status() string {
	switch b.state.Load() {
	case bpNone:
		return ""
	case bpUp:
		return "connected"
	}
	return "disconnected"
}

func (b *backplane) health() error {
	switch b.state.Load() {
	case bpNone, bpUp:
		return nil
	case bpConnecting:
		return errors.New("realtime: the Redis backplane has not subscribed yet")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Errorf("realtime: the Redis backplane is disconnected: %v", b.lastErr)
}

// up records a confirmed subscription and reports whether it came back
// after a loss.
func (b *backplane) up() (back bool) {
	b.mu.Lock()
	back, b.everUp, b.lastErr = b.everUp, true, nil
	b.mu.Unlock()
	b.state.Store(bpUp)
	return back
}

// down records a lost or failed subscription and reports whether it is
// news: the loss of a live subscription, or the first failure to get one.
func (b *backplane) down(err error) (news bool) {
	b.mu.Lock()
	b.lastErr = err
	b.mu.Unlock()
	old := b.state.Swap(bpDown)
	return old == bpUp || old == bpConnecting
}

// runBackplane keeps the subscription to Config.Channel until ctx ends,
// one connection at a time, backing off between attempts.
func (h *Hub) runBackplane(ctx context.Context) {
	backoff := minBackoff
	for {
		err := h.subscribeOnce(ctx, &backoff)
		if ctx.Err() != nil {
			return
		}
		if h.bp.down(err) {
			h.logger.WarnContext(ctx, "realtime: the Redis backplane is disconnected; reconnecting",
				"channel", h.cfg.Channel, "error", err)
		} else {
			h.logger.DebugContext(ctx, "realtime: the Redis backplane is still disconnected", "error", err, "retry_in", backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// subscribeOnce subscribes on one connection and delivers what arrives
// until the connection fails — or stays silent through a ping — or ctx
// ends.
func (h *Hub) subscribeOnce(ctx context.Context, backoff *time.Duration) error {
	ps := h.redis.Subscribe(ctx, h.cfg.Channel)
	defer func() { _ = ps.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = ps.Close() }) // unblocks a read
	defer stop()
	pinged := false
	for {
		msg, err := ps.ReceiveTimeout(ctx, h.cfg.PingInterval)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				return err
			}
			if pinged {
				return errors.New("redis did not answer a ping")
			}
			if err := ps.Ping(ctx); err != nil {
				return err
			}
			pinged = true
			continue
		}
		pinged = false
		switch m := msg.(type) {
		case *goredis.Subscription:
			if m.Kind == "subscribe" && m.Channel == h.cfg.Channel {
				*backoff = minBackoff
				if h.bp.up() {
					h.logger.InfoContext(ctx, "realtime: the Redis backplane is connected again", "channel", h.cfg.Channel)
				} else {
					h.logger.DebugContext(ctx, "realtime: subscribed to the Redis backplane", "channel", h.cfg.Channel)
				}
			}
		case *goredis.Message:
			h.receive(ctx, m.Payload)
		}
	}
}

// receive delivers an event another replica published, after checking
// it, and keeps it for resuming; this hub's own events come back too and
// are skipped.
func (h *Hub) receive(ctx context.Context, payload string) {
	var ev Event
	err := json.Unmarshal([]byte(payload), &ev)
	origin, seq, ok := strings.Cut(ev.ID, "-")
	if err != nil || !ok || origin == "" || seq == "" || !validName(ev.ID) || !validName(ev.Topic) ||
		!validName(ev.Type) || ev.Type == typeError || ev.Type == typeReset {
		h.logger.WarnContext(ctx, "realtime: ignored a malformed message on the Redis backplane", "channel", h.cfg.Channel, "error", err)
		return
	}
	if origin == h.origin {
		return
	}
	h.mu.Lock()
	f := h.newFrame(ev)
	h.replay.add(f)
	slow := h.deliverLocked(f)
	h.mu.Unlock()
	h.logDropped(slow, ev.Topic)
}

// broadcast hands an event this replica published to the others. A
// failure is logged at Warn once until a publish succeeds again: the
// local subscribers have the event already.
func (h *Hub) broadcast(ctx context.Context, f *frame) {
	if err := h.redis.Publish(ctx, h.cfg.Channel, f.json).Err(); err != nil {
		if h.bp.pubFailing.CompareAndSwap(false, true) {
			h.logger.WarnContext(ctx, "realtime: publishing to the Redis backplane failed; the other replicas miss events until it works again",
				"channel", h.cfg.Channel, "error", err)
		}
		return
	}
	if h.bp.pubFailing.CompareAndSwap(true, false) {
		h.logger.InfoContext(ctx, "realtime: publishing to the Redis backplane works again", "channel", h.cfg.Channel)
	}
}
