package event

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type evA struct{ N int }
type evB struct{ S string }

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timeout: " + msg)
}

func TestBus_TypedRouting(t *testing.T) {
	b := NewBus()
	defer b.Close(context.Background())

	var gotA, gotB atomic.Int64
	Subscribe(b, func(_ context.Context, e evA) { gotA.Add(int64(e.N)) })
	Subscribe(b, func(_ context.Context, e evB) { gotB.Add(int64(len(e.S))) })

	Publish(context.Background(), b, evA{N: 2})
	Publish(context.Background(), b, evA{N: 3})
	Publish(context.Background(), b, evB{S: "xy"})

	waitFor(t, func() bool { return gotA.Load() == 5 && gotB.Load() == 2 },
		"typed delivery")
}

func TestBus_AsyncDoesNotBlockPublisher(t *testing.T) {
	b := NewBus()
	defer b.Close(context.Background())

	release := make(chan struct{})
	var seen atomic.Int64
	Subscribe(b, func(_ context.Context, _ evA) {
		<-release
		seen.Add(1)
	})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			Publish(context.Background(), b, evA{N: i})
		}
		close(done)
	}()
	select {
	case <-done: // publisher returned while the subscriber is stuck
	case <-time.After(2 * time.Second):
		t.Fatal("Publish must not block on a slow async subscriber")
	}
	close(release)
	waitFor(t, func() bool { return seen.Load() == 10 }, "backlog delivery")
}

func TestBus_DropOldestOverflow(t *testing.T) {
	b := NewBus()
	defer b.Close(context.Background())

	release := make(chan struct{})
	var mu sync.Mutex
	var got []int
	Subscribe(b, func(_ context.Context, e evA) {
		<-release
		mu.Lock()
		got = append(got, e.N)
		mu.Unlock()
	}, WithQueueSize(2))

	// Queue capacity 2; publish 4 while the consumer is blocked on the
	// FIRST event it dequeued... The consumer may have already pulled
	// event 0 into invoke, so events 1..3 fight for 2 slots — the
	// oldest queued are dropped, the newest survive.
	for i := 0; i < 4; i++ {
		Publish(context.Background(), b, evA{N: i})
	}
	close(release)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2
	}, "post-overflow delivery")

	time.Sleep(20 * time.Millisecond) // let any stragglers land
	mu.Lock()
	defer mu.Unlock()
	last := got[len(got)-1]
	if last != 3 {
		t.Fatalf("newest event must survive DropOldest, got %v", got)
	}
	if len(got) > 3 {
		t.Fatalf("capacity 2 (+1 in flight) cannot deliver all 4: %v", got)
	}
}

func TestBus_BlockPolicyHonorsPublishCtx(t *testing.T) {
	b := NewBus()
	defer b.Close(context.Background())

	release := make(chan struct{})
	Subscribe(b, func(_ context.Context, _ evA) { <-release }, WithQueueSize(1), WithBlock())

	Publish(context.Background(), b, evA{N: 0}) // consumer picks up, blocks in fn
	Publish(context.Background(), b, evA{N: 1}) // fills the queue

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	Publish(ctx, b, evA{N: 2}) // queue full → blocks → ctx bails out
	if time.Since(start) > 2*time.Second {
		t.Fatal("blocked publish must respect ctx cancellation")
	}
	close(release)
}

func TestBus_SyncSubscriberOrdering(t *testing.T) {
	b := NewBus()
	defer b.Close(context.Background())

	var got []int
	Subscribe(b, func(_ context.Context, e evA) { got = append(got, e.N) }, WithSync())

	for i := 0; i < 3; i++ {
		Publish(context.Background(), b, evA{N: i})
	}
	// Sync delivery happens inline: no waiting needed.
	if len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Fatalf("sync subscriber must run inline in order: %v", got)
	}
}

func TestBus_SubscriberPanicIsolated(t *testing.T) {
	b := NewBus()
	defer b.Close(context.Background())

	var after atomic.Int64
	Subscribe(b, func(_ context.Context, e evA) {
		if e.N == 0 {
			panic("bad event")
		}
		after.Add(1)
	})

	Publish(context.Background(), b, evA{N: 0}) // panics
	Publish(context.Background(), b, evA{N: 1}) // must still arrive
	waitFor(t, func() bool { return after.Load() == 1 },
		"subscription survives a panicking event")

	// Sync path panics must not propagate to the publisher either.
	Subscribe(b, func(_ context.Context, _ evB) { panic("sync boom") }, WithSync())
	Publish(context.Background(), b, evB{S: "x"}) // must not panic the test
}

func TestBus_CancelIdempotentAndDetaches(t *testing.T) {
	b := NewBus()
	defer b.Close(context.Background())

	var n atomic.Int64
	cancel := Subscribe(b, func(_ context.Context, _ evA) { n.Add(1) })
	Publish(context.Background(), b, evA{})
	waitFor(t, func() bool { return n.Load() == 1 }, "pre-cancel delivery")

	cancel()
	cancel() // idempotent
	Publish(context.Background(), b, evA{})
	time.Sleep(20 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatal("cancelled subscription must not receive events")
	}
}

func TestBus_CloseDrainsBacklogThenPublishNoop(t *testing.T) {
	b := NewBus()

	release := make(chan struct{})
	var n atomic.Int64
	Subscribe(b, func(_ context.Context, _ evA) {
		<-release
		n.Add(1)
	}, WithQueueSize(16))

	for i := 0; i < 5; i++ {
		Publish(context.Background(), b, evA{N: i})
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(release)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.Close(ctx)

	if got := n.Load(); got != 5 {
		t.Fatalf("Close must drain the backlog, delivered %d/5", got)
	}

	Publish(context.Background(), b, evA{N: 99}) // silent no-op, no panic
	if n.Load() != 5 {
		t.Fatal("post-Close publish must be a no-op")
	}
}

func TestBus_CloseBudgetAbandons(t *testing.T) {
	b := NewBus()

	Subscribe(b, func(_ context.Context, _ evA) {
		time.Sleep(10 * time.Second) // never finishes within budget
	})
	Publish(context.Background(), b, evA{})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	b.Close(ctx)
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close must abandon the drain when the budget expires")
	}
}

func TestBus_SubscribeAfterCloseInert(t *testing.T) {
	b := NewBus()
	b.Close(context.Background())
	cancel := Subscribe(b, func(_ context.Context, _ evA) { t.Error("must never fire") })
	Publish(context.Background(), b, evA{})
	cancel()
}
