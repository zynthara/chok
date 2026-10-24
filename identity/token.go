package identity

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token issuers: an access token never passes for a reset token, nor the
// other way round, though one key signs both.
const (
	issuerAccess = "access"
	issuerReset  = "reset"
)

// tokenLeeway tolerates clock drift between the signer and the verifier
// when checking exp and iat; maxTokenLeeway caps it, since a leeway is a
// silent extension of every token's life.
const (
	tokenLeeway    = 30 * time.Second
	maxTokenLeeway = 5 * time.Minute
)

// tokens signs and parses one kind of JWT with HS256.
type tokens struct {
	key    []byte
	issuer string
	ttl    time.Duration
	leeway time.Duration // negative: strict
	now    func() time.Time
}

func newTokens(key, issuer string, ttl time.Duration) (*tokens, error) {
	if key == "" {
		return nil, errors.New("identity: the signing key is empty")
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("identity: the signing key is too short (%d bytes); HS256 needs at least 32", len(key))
	}
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	return &tokens{key: []byte(key), issuer: issuer, ttl: ttl, leeway: tokenLeeway, now: time.Now}, nil
}

// sign returns a token for subject carrying claims, and its expiry. The
// registered claims (sub, iat, exp, iss) are written last: claims cannot
// override them.
func (m *tokens) sign(subject string, claims map[string]any) (string, time.Time, error) {
	now := m.now()
	exp := now.Add(m.ttl)
	mc := make(jwt.MapClaims, len(claims)+4)
	maps.Copy(mc, claims)
	mc["sub"] = subject
	mc["iat"] = now.Unix()
	mc["exp"] = exp.Unix()
	if m.issuer != "" {
		mc["iss"] = m.issuer
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, mc).SignedString(m.key)
	return signed, exp, err
}

var errBadToken = errors.New("identity: invalid token")

// parse checks a token and returns its subject and claims: HS256 only
// (alg none and every other method are refused), exp required, iat not in
// the future, iss this manager's. Bearer extraction is the caller's.
func (m *tokens) parse(raw string) (string, map[string]any, error) {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(m.now),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	}
	if m.leeway > 0 {
		opts = append(opts, jwt.WithLeeway(min(m.leeway, maxTokenLeeway)))
	}
	if m.issuer != "" {
		opts = append(opts, jwt.WithIssuer(m.issuer))
	}
	tok, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return m.key, nil }, opts...)
	if err != nil || !tok.Valid {
		return "", nil, errBadToken
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return "", nil, errBadToken
	}
	sub, _ := mc["sub"].(string)
	if sub == "" {
		return "", nil, errBadToken
	}
	return sub, map[string]any(mc), nil
}

// intClaim reads a numeric claim, which JSON decodes as float64.
func intClaim(claims map[string]any, name string) (int64, bool) {
	f, ok := claims[name].(float64)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}
