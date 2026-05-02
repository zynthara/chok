package account

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// MemorySessionStore is an in-process OAuthSessionStore + AuthCodeStore
// backed by a TTL-aware LRU map. Suitable for single-instance dev /
// staging — multi-instance deployments need a shared back-end (Redis)
// because Save on instance A and Take on instance B would never resolve.
//
// The same struct also implements AuthCodeStore: keys are namespaced by
// prefix internally ("session:"+sid for OAuthSession entries, "code:"+code
// for AuthCodeData) so a single MemorySessionStore can serve both
// interfaces. Module wires it that way by default.
type MemorySessionStore struct {
	mu       sync.Mutex
	entries  map[string]*list.Element // key → list element (eviction order)
	order    *list.List               // front = MRU, back = LRU
	capacity int

	closeOnce sync.Once
	stopCh    chan struct{}
}

type memoryEntry struct {
	key       string
	value     any // *OAuthSession or *AuthCodeData
	expiresAt time.Time
}

// memoryConfig holds NewMemorySessionStore tunables.
type memoryConfig struct {
	capacity      int
	cleanupPeriod time.Duration
}

// MemoryOption tunes MemorySessionStore behaviour.
type MemoryOption func(*memoryConfig)

// WithCapacity caps the total number of live entries. Hitting the cap
// evicts the LRU entry. Default 10000.
//
// The eviction policy is intentionally LRU rather than FIFO: an attacker
// flooding /auth/start cannot push out an in-progress legitimate user's
// sid as long as legitimate users hit /callback within the IdP roundtrip
// window. Evicted sessions surface as ErrSessionNotFound at Take time,
// the same as natural expiry.
func WithCapacity(n int) MemoryOption {
	return func(c *memoryConfig) {
		if n > 0 {
			c.capacity = n
		}
	}
}

// WithCleanupPeriod sets how often the background goroutine sweeps
// expired entries. Default 1 minute. Zero or negative disables the
// background sweep — entries still drop on Take, but stale entries
// linger until evicted by capacity pressure.
func WithCleanupPeriod(d time.Duration) MemoryOption {
	return func(c *memoryConfig) {
		c.cleanupPeriod = d
	}
}

// NewMemorySessionStore constructs an in-memory store. The returned
// value satisfies both OAuthSessionStore and AuthCodeStore; callers can
// pass it to WithOAuthSessionStore and WithAuthCodeStore to share state
// across both flows or pass it to one and override the other.
//
// Spawns a background cleanup goroutine that exits when Close is called.
// Module.Close runs the io.Closer type assertion and chains the close
// so applications that route lifecycle through the registry don't need
// to track the store handle.
func NewMemorySessionStore(opts ...MemoryOption) *MemorySessionStore {
	cfg := memoryConfig{capacity: 10000, cleanupPeriod: time.Minute}
	for _, o := range opts {
		o(&cfg)
	}
	s := &MemorySessionStore{
		entries:  make(map[string]*list.Element),
		order:    list.New(),
		capacity: cfg.capacity,
		stopCh:   make(chan struct{}),
	}
	if cfg.cleanupPeriod > 0 {
		go s.cleanupLoop(cfg.cleanupPeriod)
	}
	return s
}

// Save implements OAuthSessionStore.
func (s *MemorySessionStore) Save(_ context.Context, sid string, sess *OAuthSession, ttl time.Duration) error {
	s.put("session:"+sid, sess, ttl)
	return nil
}

// Take implements OAuthSessionStore. Atomic load-and-delete.
func (s *MemorySessionStore) Take(_ context.Context, sid string) (*OAuthSession, error) {
	v, ok := s.takeKey("session:" + sid)
	if !ok {
		return nil, ErrSessionNotFound
	}
	sess, _ := v.(*OAuthSession)
	if sess == nil {
		// Wrong type stored under "session:" prefix — programmer error,
		// but treat as not-found to avoid leaking that an AuthCode
		// happens to share the sid namespace.
		return nil, ErrSessionNotFound
	}
	return sess, nil
}

// SaveAuthCode implements AuthCodeStore.Save.
func (s *MemorySessionStore) SaveAuthCode(_ context.Context, code string, data *AuthCodeData, ttl time.Duration) error {
	s.put("code:"+code, data, ttl)
	return nil
}

// TakeAuthCode implements AuthCodeStore.Take. Atomic load-and-delete.
func (s *MemorySessionStore) TakeAuthCode(_ context.Context, code string) (*AuthCodeData, error) {
	v, ok := s.takeKey("code:" + code)
	if !ok {
		return nil, ErrAuthCodeNotFound
	}
	data, _ := v.(*AuthCodeData)
	if data == nil {
		return nil, ErrAuthCodeNotFound
	}
	return data, nil
}

// Close stops the background cleanup goroutine. Idempotent.
func (s *MemorySessionStore) Close() error {
	s.closeOnce.Do(func() {
		close(s.stopCh)
	})
	return nil
}

func (s *MemorySessionStore) put(key string, value any, ttl time.Duration) {
	expiresAt := time.Now().Add(ttl)
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.entries[key]; ok {
		// Refresh in place: same key replaces the value and bumps to MRU.
		existing.Value.(*memoryEntry).value = value
		existing.Value.(*memoryEntry).expiresAt = expiresAt
		s.order.MoveToFront(existing)
		return
	}

	if s.capacity > 0 && len(s.entries) >= s.capacity {
		// Evict LRU. Capacity guard: capacity == 0 would never evict but
		// NewMemorySessionStore enforces capacity > 0.
		oldest := s.order.Back()
		if oldest != nil {
			s.removeElement(oldest)
		}
	}

	entry := &memoryEntry{key: key, value: value, expiresAt: expiresAt}
	s.entries[key] = s.order.PushFront(entry)
}

func (s *MemorySessionStore) takeKey(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	elem, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	entry := elem.Value.(*memoryEntry)
	if time.Now().After(entry.expiresAt) {
		s.removeElement(elem)
		return nil, false
	}
	value := entry.value
	s.removeElement(elem)
	return value, true
}

// removeElement assumes s.mu is held.
func (s *MemorySessionStore) removeElement(elem *list.Element) {
	entry := elem.Value.(*memoryEntry)
	delete(s.entries, entry.key)
	s.order.Remove(elem)
}

func (s *MemorySessionStore) cleanupLoop(period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.sweepExpired()
		case <-s.stopCh:
			return
		}
	}
}

func (s *MemorySessionStore) sweepExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for e := s.order.Back(); e != nil; {
		entry := e.Value.(*memoryEntry)
		// Walk from LRU (back) toward MRU; stop when we hit a non-expired
		// entry. LRU order doesn't equal expiration order (TTL is uniform
		// per call, and each Save bumps expiration), so this is a heuristic
		// — most expired entries cluster near the back. The sweep is
		// best-effort; precise cleanup happens on Take.
		prev := e.Prev()
		if entry.expiresAt.Before(now) {
			s.removeElement(e)
		}
		e = prev
	}
}

// Compile-time interface assertions.
var (
	_ OAuthSessionStore = (*MemorySessionStore)(nil)
)

// memoryAuthCodeAdapter satisfies AuthCodeStore by routing to the
// MemorySessionStore's "code:"-prefixed bucket. Module exposes it as the
// default AuthCodeStore via NewMemoryAuthCodeStore so callers don't need
// to know the prefix scheme.
type memoryAuthCodeAdapter struct {
	store *MemorySessionStore
}

// NewMemoryAuthCodeStore returns an AuthCodeStore backed by the same
// MemorySessionStore. Sharing the backing map keeps the dev-default
// memory footprint low; the prefix namespace prevents accidental
// cross-pollination.
func NewMemoryAuthCodeStore(store *MemorySessionStore) AuthCodeStore {
	return &memoryAuthCodeAdapter{store: store}
}

func (a *memoryAuthCodeAdapter) Save(ctx context.Context, code string, data *AuthCodeData, ttl time.Duration) error {
	return a.store.SaveAuthCode(ctx, code, data, ttl)
}

func (a *memoryAuthCodeAdapter) Take(ctx context.Context, code string) (*AuthCodeData, error) {
	return a.store.TakeAuthCode(ctx, code)
}

var _ AuthCodeStore = (*memoryAuthCodeAdapter)(nil)
