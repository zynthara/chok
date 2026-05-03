package apple

import (
	"crypto/ecdsa"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// clientSecretCache wraps the dynamically-signed Apple client_secret
// JWT and its expiry. ES256 signing isn't free, and Apple's
// /auth/token endpoint is hit on every login; refreshing the JWT once
// per TTL window is the right amortisation.
//
// All access goes through Get() under the mutex. The cache is owned
// by *provider, never escaping; concurrent /auth/apple/start +
// /auth/apple/callback flows compete for the same secret string and
// the mutex serialises them. Test
// TestClientSecret_ConcurrentSignsOnce asserts a 1000-goroutine
// thundering herd produces exactly one signature.
type clientSecretCache struct {
	mu       sync.Mutex
	value    string
	expiresAt time.Time

	// signing inputs are immutable after provider construction; we
	// keep references rather than copying so the cache shares them.
	teamID    string
	keyID     string
	serviceID string
	ttl       time.Duration
	privKey   *ecdsa.PrivateKey
	now       func() time.Time // injectable for tests
}

// newClientSecretCache constructs an empty cache wired to the
// provider's signing inputs. The first Get() does the actual sign.
func newClientSecretCache(teamID, keyID, serviceID string, ttl time.Duration, key *ecdsa.PrivateKey) *clientSecretCache {
	return &clientSecretCache{
		teamID:    teamID,
		keyID:     keyID,
		serviceID: serviceID,
		ttl:       ttl,
		privKey:   key,
		now:       time.Now,
	}
}

// Get returns a valid client_secret, signing a new one if the cache
// is empty or within the refresh window. The refresh window is
// `ttl - 1m` so concurrent callers near expiry don't all race past a
// barely-valid token.
func (c *clientSecretCache) Get() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.value != "" && now.Before(c.expiresAt) {
		return c.value, nil
	}
	signed, err := c.sign(now)
	if err != nil {
		return "", err
	}
	c.value = signed
	// One-minute safety margin: callers that grab the secret right
	// before expiry don't ship an already-stale JWT to Apple.
	c.expiresAt = now.Add(c.ttl).Add(-time.Minute)
	return signed, nil
}

// sign constructs the JWT per Apple's documentation:
//
//	header.alg = ES256, header.kid = KeyID
//	claims:
//	  iss = TeamID
//	  iat = now
//	  exp = now + ttl
//	  aud = "https://appleid.apple.com"
//	  sub = ServiceID
//
// Caller must hold c.mu.
func (c *clientSecretCache) sign(now time.Time) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": c.teamID,
		"iat": now.Unix(),
		"exp": now.Add(c.ttl).Unix(),
		"aud": productionAudience,
		"sub": c.serviceID,
	})
	tok.Header["kid"] = c.keyID
	signed, err := tok.SignedString(c.privKey)
	if err != nil {
		return "", fmt.Errorf("apple: sign client_secret: %w", err)
	}
	return signed, nil
}
