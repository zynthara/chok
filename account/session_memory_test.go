package account

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestMemorySessionStore_SaveTake(t *testing.T) {
	s := NewMemorySessionStore()
	defer s.Close()
	sess := &OAuthSession{State: "st", Provider: "fake"}
	if err := s.Save(context.Background(), "sid", sess, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := s.Take(context.Background(), "sid")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "st" {
		t.Fatalf("got %v", got)
	}

	// Second Take must miss — Take is one-shot.
	if _, err := s.Take(context.Background(), "sid"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound on second Take, got %v", err)
	}
}

func TestMemorySessionStore_Expired(t *testing.T) {
	s := NewMemorySessionStore()
	defer s.Close()
	if err := s.Save(context.Background(), "sid", &OAuthSession{}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := s.Take(context.Background(), "sid"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound for expired entry, got %v", err)
	}
}

func TestMemorySessionStore_LRUEvict(t *testing.T) {
	s := NewMemorySessionStore(WithCapacity(3))
	defer s.Close()

	for i := 0; i < 3; i++ {
		_ = s.Save(context.Background(), "sid"+strconv.Itoa(i), &OAuthSession{State: strconv.Itoa(i)}, time.Minute)
	}
	// Insert 4th — evicts sid0.
	_ = s.Save(context.Background(), "sid3", &OAuthSession{State: "3"}, time.Minute)

	if _, err := s.Take(context.Background(), "sid0"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("sid0 should have been evicted, got %v", err)
	}
	for _, id := range []string{"sid1", "sid2", "sid3"} {
		if _, err := s.Take(context.Background(), id); err != nil {
			t.Fatalf("expected %s to survive eviction: %v", id, err)
		}
	}
}

func TestMemorySessionStore_TakeAtomicConcurrent(t *testing.T) {
	s := NewMemorySessionStore()
	defer s.Close()
	_ = s.Save(context.Background(), "sid", &OAuthSession{State: "race"}, time.Minute)

	const N = 50
	var wg sync.WaitGroup
	winners := 0
	var mu sync.Mutex
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := s.Take(context.Background(), "sid"); err == nil && got != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("Take must be atomic: got %d winners, want 1", winners)
	}
}

func TestMemorySessionStore_AuthCodeBucketIsolated(t *testing.T) {
	s := NewMemorySessionStore()
	defer s.Close()
	// Same id used for both buckets — they must not collide because of
	// internal "session:" / "code:" prefixing.
	_ = s.Save(context.Background(), "x", &OAuthSession{State: "session"}, time.Minute)
	_ = s.SaveAuthCode(context.Background(), "x", &AuthCodeData{UserID: "u"}, time.Minute)

	sess, err := s.Take(context.Background(), "x")
	if err != nil || sess.State != "session" {
		t.Fatalf("session bucket: %v %v", sess, err)
	}
	code, err := s.TakeAuthCode(context.Background(), "x")
	if err != nil || code.UserID != "u" {
		t.Fatalf("code bucket: %v %v", code, err)
	}
}

func TestMemoryAuthCodeAdapter(t *testing.T) {
	mem := NewMemorySessionStore()
	defer mem.Close()
	ac := NewMemoryAuthCodeStore(mem)

	if err := ac.Save(context.Background(), "code1", &AuthCodeData{UserID: "u1", RedirectBack: "/"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := ac.Take(context.Background(), "code1")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != "u1" {
		t.Fatalf("got %v", got)
	}
	if _, err := ac.Take(context.Background(), "code1"); !errors.Is(err, ErrAuthCodeNotFound) {
		t.Fatalf("expected ErrAuthCodeNotFound on second Take, got %v", err)
	}
}

func TestMemorySessionStore_CloseStopsCleanup(t *testing.T) {
	s := NewMemorySessionStore(WithCleanupPeriod(10 * time.Millisecond))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMemorySessionStore_RefreshUpdatesValueAndOrder(t *testing.T) {
	s := NewMemorySessionStore(WithCapacity(2))
	defer s.Close()
	_ = s.Save(context.Background(), "a", &OAuthSession{State: "a1"}, time.Minute)
	_ = s.Save(context.Background(), "b", &OAuthSession{State: "b1"}, time.Minute)
	// Refresh "a" — bumps it to MRU and updates the payload.
	_ = s.Save(context.Background(), "a", &OAuthSession{State: "a2"}, time.Minute)
	// Insert "c" — should evict "b" (now LRU), not "a".
	_ = s.Save(context.Background(), "c", &OAuthSession{State: "c1"}, time.Minute)

	if _, err := s.Take(context.Background(), "b"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("expected b to be evicted")
	}
	got, err := s.Take(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "a2" {
		t.Fatalf("expected refreshed state a2, got %q", got.State)
	}
}
