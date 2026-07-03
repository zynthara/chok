// Package event is the chok v2 business event bus: typed pub/sub with
// per-subscriber bounded queues. It is layer two of the lifecycle
// split (SPEC §3.5): asynchronous, no error propagation, no veto —
// anything that must be able to abort a phase belongs to component
// Init or the reload callback, never here.
package event

import (
	"context"
	"reflect"
	"sync"
	"time"
)

// Logger is the bus's consumer-side logging contract (overflow warns,
// subscriber panics). The chok log.Logger satisfies it structurally —
// defined here so this package stays a leaf under the kernel.
type Logger interface {
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}

type nopLogger struct{}

func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// Bus routes published values to subscribers registered for the
// value's concrete type. The zero value is not usable; construct with
// NewBus.
type Bus struct {
	logger Logger

	mu     sync.RWMutex
	subs   map[reflect.Type][]*subscription
	closed bool
}

// BusOpt configures NewBus.
type BusOpt func(*Bus)

// WithLogger routes overflow warnings and subscriber panics to l.
// Without it the bus stays silent (events are still counted).
func WithLogger(l Logger) BusOpt {
	return func(b *Bus) { b.logger = l }
}

// NewBus constructs an empty bus.
func NewBus(opts ...BusOpt) *Bus {
	b := &Bus{subs: make(map[reflect.Type][]*subscription)}
	for _, o := range opts {
		o(b)
	}
	if b.logger == nil {
		b.logger = nopLogger{}
	}
	return b
}

// overflow policies (mini-SPEC §2).
const (
	dropOldest = iota // default: never backpressure the publisher
	block             // opt-in: wait for space, bounded by publish ctx
)

// defaultQueueSize bounds each subscriber's queue unless overridden
// (mini-SPEC §2: lifecycle events are low-rate; heavy business streams
// opt into bigger queues).
const defaultQueueSize = 64

type subscription struct {
	bus    *Bus
	evType reflect.Type
	fn     func(context.Context, any)

	sync     bool
	policy   int
	capacity int

	mu       sync.Mutex
	queue    []queued
	dropped  uint64
	lastWarn time.Time
	closed   bool

	dataReady chan struct{} // cap 1: producer → consumer nudge
	spaceFree chan struct{} // cap 1: consumer → blocked producer nudge
	done      chan struct{} // closed when the consumer goroutine exits
	stop      chan struct{} // closed by cancel/Close to stop the consumer
	stopOnce  sync.Once
}

type queued struct {
	ctx context.Context
	ev  any
}

// SubOpt configures a subscription.
type SubOpt func(*subscription)

// WithQueueSize overrides the bounded queue capacity (min 1).
func WithQueueSize(n int) SubOpt {
	return func(s *subscription) {
		if n > 0 {
			s.capacity = n
		}
	}
}

// WithBlock makes Publish wait for queue space instead of dropping the
// oldest event. The wait is bounded by the publish context — on
// ctx.Done the event is dropped (counted), never deadlocked.
func WithBlock() SubOpt {
	return func(s *subscription) { s.policy = block }
}

// WithDropOldest is the explicit spelling of the default policy.
func WithDropOldest() SubOpt {
	return func(s *subscription) { s.policy = dropOldest }
}

// WithSync delivers in the publisher's goroutine. Ordering only —
// there is still no error propagation, and a panicking subscriber is
// recovered and logged exactly like the async path.
func WithSync() SubOpt {
	return func(s *subscription) { s.sync = true }
}

// Publish delivers ev to every subscriber registered for T's concrete
// type. Never returns an error and never panics (subscriber panics are
// recovered); after Close it is a silent no-op.
func Publish[T any](ctx context.Context, b *Bus, ev T) {
	if b == nil {
		return
	}
	t := reflect.TypeOf(ev)
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}
	subs := b.subs[t]
	b.mu.RUnlock()

	for _, s := range subs {
		s.deliver(ctx, ev)
	}
}

// Subscribe registers fn for events of T's concrete type and returns
// an idempotent cancel. Async subscribers (the default) get their own
// goroutine and bounded queue.
func Subscribe[T any](b *Bus, fn func(context.Context, T), opts ...SubOpt) (cancel func()) {
	var zero T
	t := reflect.TypeOf(zero)
	s := &subscription{
		bus:      b,
		evType:   t,
		capacity: defaultQueueSize,
		fn: func(ctx context.Context, ev any) {
			fn(ctx, ev.(T))
		},
		dataReady: make(chan struct{}, 1),
		spaceFree: make(chan struct{}, 1),
		done:      make(chan struct{}),
		stop:      make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.done)
		return func() {}
	}
	b.subs[t] = append(b.subs[t], s)
	b.mu.Unlock()

	if !s.sync {
		go s.run()
	} else {
		close(s.done)
	}

	return func() { s.cancel() }
}

// deliver enqueues (async) or invokes (sync) one event.
func (s *subscription) deliver(ctx context.Context, ev any) {
	if s.sync {
		s.invoke(ctx, ev)
		return
	}

	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		if len(s.queue) < s.capacity {
			s.queue = append(s.queue, queued{ctx: context.WithoutCancel(ctx), ev: ev})
			s.mu.Unlock()
			s.nudge(s.dataReady)
			return
		}
		if s.policy == dropOldest {
			s.queue = s.queue[1:]
			s.queue = append(s.queue, queued{ctx: context.WithoutCancel(ctx), ev: ev})
			s.dropped++
			s.maybeWarnLocked()
			s.mu.Unlock()
			s.nudge(s.dataReady)
			return
		}
		// block policy: wait for space, bounded by the publish ctx.
		s.mu.Unlock()
		select {
		case <-s.spaceFree:
		case <-ctx.Done():
			s.mu.Lock()
			s.dropped++
			s.maybeWarnLocked()
			s.mu.Unlock()
			return
		case <-s.stop:
			return
		}
	}
}

// maybeWarnLocked rate-limits overflow warnings to one per second.
func (s *subscription) maybeWarnLocked() {
	now := time.Now()
	if now.Sub(s.lastWarn) < time.Second {
		return
	}
	s.lastWarn = now
	s.bus.logger.Warn("event: subscriber queue overflow, events dropped",
		"event_type", s.evType.String(), "dropped_total", s.dropped)
}

// run is the async consumer loop.
func (s *subscription) run() {
	defer close(s.done)
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			item := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			s.nudge(s.spaceFree)
			s.invoke(item.ctx, item.ev)
			continue
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		select {
		case <-s.dataReady:
		case <-s.stop:
			// Drain what is already queued, then exit (the loop above
			// keeps consuming until empty because closed is set by
			// cancel/Close before stop closes).
		}
	}
}

// invoke calls the subscriber with panic isolation: one bad event
// never kills the subscription (mini-SPEC §2).
func (s *subscription) invoke(ctx context.Context, ev any) {
	defer func() {
		if p := recover(); p != nil {
			s.bus.logger.Error("event: subscriber panicked",
				"event_type", s.evType.String(), "panic", p)
		}
	}()
	s.fn(ctx, ev)
}

func (s *subscription) nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// cancel detaches the subscription and stops its goroutine after the
// queued backlog is delivered. Idempotent.
func (s *subscription) cancel() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.bus.detach(s)
	s.stopOnce.Do(func() { close(s.stop) })
	s.nudge(s.dataReady)
}

func (b *Bus) detach(target *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.subs[target.evType]
	for i, s := range list {
		if s == target {
			b.subs[target.evType] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
}

// Close stops the bus: subsequent Publish calls become silent no-ops,
// every async subscriber drains its backlog within the ctx budget
// (mini-SPEC §2: default 5s from the kernel), then the goroutines
// stop. Called by the kernel after the last component Close so
// subscribers observe the complete lifecycle stream.
func (b *Bus) Close(ctx context.Context) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	var all []*subscription
	for _, list := range b.subs {
		all = append(all, list...)
	}
	b.subs = make(map[reflect.Type][]*subscription)
	b.mu.Unlock()

	for _, s := range all {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.stopOnce.Do(func() { close(s.stop) })
		s.nudge(s.dataReady)
	}
	for _, s := range all {
		select {
		case <-s.done:
		case <-ctx.Done():
			b.logger.Warn("event: bus close budget exceeded, abandoning subscriber drain",
				"event_type", s.evType.String())
			return
		}
	}
}
