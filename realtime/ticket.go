package realtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok"
)

// claims is what a ticket binds: the principal it was issued to, none for
// an anonymous ticket.
type claims struct {
	Principal *chok.Principal `json:"principal,omitzero"`
}

// userID is the bound principal's UserID, "" for an anonymous ticket.
func (c claims) userID() string {
	if c.Principal == nil {
		return ""
	}
	return c.Principal.UserID
}

// tickets holds what each ticket binds until it is redeemed once or
// expires: in process, or in Redis so that any replica redeems it.
type tickets interface {
	put(ctx context.Context, ticket string, c claims, ttl time.Duration) error
	// take redeems ticket: its claims, and false when it is unknown,
	// expired or already redeemed.
	take(ctx context.Context, ticket string) (claims, bool, error)
}

// ticketBytes is a ticket's entropy; it travels as unpadded base64url.
const ticketBytes = 32

var ticketLen = base64.RawURLEncoding.EncodedLen(ticketBytes)

func newTicket() string {
	b := make([]byte, ticketBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

// wellFormed reports whether s can be a ticket, so that nothing else is
// looked up.
func wellFormed(s string) bool {
	if len(s) != ticketLen {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}

// memTickets keeps the tickets of a single replica. Every ticket lives
// the same TTL, so issue order is expiry order and expired tickets are
// popped off the front.
type memTickets struct {
	mu    sync.Mutex
	byID  map[string]memTicket
	order []string
}

type memTicket struct {
	claims  claims
	expires time.Time
}

func newMemTickets() *memTickets { return &memTickets{byID: map[string]memTicket{}} }

func (m *memTickets) put(_ context.Context, ticket string, c claims, ttl time.Duration) error {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(now)
	m.byID[ticket] = memTicket{claims: c, expires: now.Add(ttl)}
	m.order = append(m.order, ticket)
	return nil
}

func (m *memTickets) take(_ context.Context, ticket string) (claims, bool, error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(now)
	t, ok := m.byID[ticket]
	if !ok || !now.Before(t.expires) {
		return claims{}, false, nil
	}
	delete(m.byID, ticket)
	return t.claims, true, nil
}

// expire forgets the tickets that expired by now.
func (m *memTickets) expire(now time.Time) {
	i := 0
	for ; i < len(m.order); i++ {
		if t, ok := m.byID[m.order[i]]; ok && now.Before(t.expires) {
			break
		}
		delete(m.byID, m.order[i])
	}
	m.order = m.order[i:]
}

// redisTickets keeps tickets in Redis under {Channel}:ticket:<ticket>:
// SET NX with the TTL to issue, GETDEL to redeem, so a ticket is redeemed
// once whichever replica gets it.
type redisTickets struct {
	client *goredis.Client
	prefix string
}

func (r *redisTickets) put(ctx context.Context, ticket string, c claims, ttl time.Duration) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	ok, err := r.client.SetNX(ctx, r.prefix+ticket, b, ttl).Result()
	if err != nil {
		return fmt.Errorf("realtime: store a ticket in Redis: %w", err)
	}
	if !ok {
		return errors.New("realtime: a new ticket is already in Redis") // 256 random bits: never
	}
	return nil
}

func (r *redisTickets) take(ctx context.Context, ticket string) (claims, bool, error) {
	b, err := r.client.GetDel(ctx, r.prefix+ticket).Bytes()
	if errors.Is(err, goredis.Nil) {
		return claims{}, false, nil
	}
	if err != nil {
		return claims{}, false, fmt.Errorf("realtime: redeem a ticket in Redis: %w", err)
	}
	var c claims
	if err := json.Unmarshal(b, &c); err != nil {
		return claims{}, false, fmt.Errorf("realtime: a ticket in Redis does not decode: %w", err)
	}
	return c, true, nil
}
