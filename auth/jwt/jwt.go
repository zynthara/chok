// Package jwt provides an instance-based JWT manager using HS256.
//
// The Manager is not a global singleton — each instance holds its own key,
// issuer, and expiration, making it safe for concurrent use and test isolation.
package jwt

import (
	"errors"
	"fmt"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// Options configures a JWT Manager.
type Options struct {
	// SigningKey is required. Must be >= 32 bytes for HS256 security.
	// Recommended: generate with `openssl rand -base64 32`, then decode
	// before passing (or pass the raw 32+ byte string directly).
	// The value is used as-is (raw bytes); no automatic base64 decoding.
	SigningKey string

	// Issuer is written to the "iss" claim if non-empty. When set, Parse also
	// validates the issuer.
	Issuer string

	// Expiration is the token lifetime. Defaults to 2 hours.
	Expiration time.Duration

	// Leeway tolerates small clock drift between signer and verifier when
	// validating exp / iat / nbf. Defaults to DefaultLeeway (30s). Set
	// to a negative value to disable (strict validation).
	Leeway time.Duration

	// Now returns the current time. Defaults to time.Now.
	// Inject a fixed clock in tests to verify expiration without sleeping.
	Now func() time.Time
}

// MaxLeeway caps clock-skew tolerance at 5 minutes. Beyond that, Leeway
// becomes a silent extension of token lifetime — a 24h Leeway on a 2h
// exp makes expired tokens accepted for a full day. NewManager rejects
// larger values.
const MaxLeeway = 5 * time.Minute

// DefaultLeeway is the default exp/iat skew tolerance. Clocks between
// signer and verifier in distributed deployments commonly drift by
// hundreds of milliseconds; without leeway, freshly signed tokens can
// be rejected as "issued in the future". 30 seconds is conservative
// and covers typical NTP-synced fleets.
const DefaultLeeway = 30 * time.Second

// Manager signs and parses JWT tokens using HS256.
type Manager struct {
	key  []byte
	opts Options
	now  func() time.Time
}

// NewManager creates a Manager.
// Returns an error if SigningKey is empty or shorter than 32 bytes.
func NewManager(opts Options) (*Manager, error) {
	if opts.SigningKey == "" {
		return nil, errors.New("jwt: signing key must not be empty")
	}
	key := []byte(opts.SigningKey)
	if len(key) < 32 {
		return nil, fmt.Errorf("jwt: signing key too short (%d bytes), minimum 32 bytes for HS256", len(key))
	}
	if opts.Expiration == 0 {
		opts.Expiration = 2 * time.Hour
	}
	if opts.Leeway == 0 {
		opts.Leeway = DefaultLeeway
	}
	// Upper-bound Leeway so callers can't (accidentally or deliberately)
	// extend token lifetime by orders of magnitude. Negative leeway is
	// allowed — it enables strict validation for sensitive paths.
	if opts.Leeway > MaxLeeway {
		return nil, fmt.Errorf("jwt: leeway %s exceeds maximum %s (use negative value for strict validation)", opts.Leeway, MaxLeeway)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{key: key, opts: opts, now: now}, nil
}

// Sign creates a signed JWT for the given subject.
// Extra claims are merged into the token (e.g. roles, tenant).
// Registered claims (sub, iat, exp, iss) are written last and cannot be
// overridden by the caller's claims map.
// Returns the token string and its expiration time.
func (m *Manager) Sign(subject string, claims map[string]any) (string, time.Time, error) {
	now := m.now()
	exp := now.Add(m.opts.Expiration)

	// Copy business claims first, then overwrite with registered claims.
	mc := make(jwtv5.MapClaims, len(claims)+4)
	for k, v := range claims {
		mc[k] = v
	}
	mc["sub"] = subject
	mc["iat"] = now.Unix()
	mc["exp"] = exp.Unix()
	if m.opts.Issuer != "" {
		mc["iss"] = m.opts.Issuer
	}

	token := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, mc)
	signed, err := token.SignedString(m.key)
	return signed, exp, err
}

// Parse validates a JWT string and returns the subject and all claims.
//
// Validation rules:
//   - Only HS256 is accepted (alg=none and other methods are rejected).
//   - exp must be present and valid (WithExpirationRequired).
//   - iat must not be in the future (WithIssuedAt).
//   - iss is validated if Options.Issuer was set.
//
// Parse does not depend on *gin.Context — Bearer extraction is handled by
// the middleware layer.
//
// Parse satisfies the middleware.TokenParser interface.
func (m *Manager) Parse(tokenString string) (string, map[string]any, error) {
	parserOpts := []jwtv5.ParserOption{
		jwtv5.WithValidMethods([]string{"HS256"}),
		jwtv5.WithTimeFunc(m.now),
		jwtv5.WithExpirationRequired(),
		jwtv5.WithIssuedAt(),
	}
	if m.opts.Leeway > 0 {
		parserOpts = append(parserOpts, jwtv5.WithLeeway(m.opts.Leeway))
	}
	if m.opts.Issuer != "" {
		parserOpts = append(parserOpts, jwtv5.WithIssuer(m.opts.Issuer))
	}

	token, err := jwtv5.Parse(tokenString, func(t *jwtv5.Token) (any, error) {
		return m.key, nil
	}, parserOpts...)
	if err != nil || !token.Valid {
		return "", nil, errors.New("jwt: invalid token")
	}

	mc, ok := token.Claims.(jwtv5.MapClaims)
	if !ok {
		return "", nil, errors.New("jwt: invalid claims")
	}
	sub, _ := mc["sub"].(string)
	if sub == "" {
		return "", nil, errors.New("jwt: missing subject")
	}
	return sub, map[string]any(mc), nil
}
