package identity

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// tokenKey is exactly the 32 bytes HS256 needs.
const tokenKey = "test-key-that-is-exactly-32-byte"

func fixedNow() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func newTestTokens(t *testing.T, issuer string, now func() time.Time) *tokens {
	t.Helper()
	m, err := newTokens(tokenKey, issuer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m.now = now
	return m
}

// signRaw signs claims as is, for tokens the manager would never make.
func signRaw(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTokens_RefusesShortKeys(t *testing.T) {
	if _, err := newTokens("", issuerAccess, time.Hour); err == nil {
		t.Fatal("an empty key must be refused")
	}
	_, err := newTokens("short", issuerAccess, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("a short key must be refused as too short, got %v", err)
	}
}

func TestTokens_RawKeyAndDefaultTTL(t *testing.T) {
	m, err := newTokens(tokenKey, issuerAccess, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.key) != 32 || string(m.key) != tokenKey {
		t.Fatalf("the key is used as raw bytes: %d", len(m.key))
	}
	if m.ttl != 2*time.Hour {
		t.Fatalf("the default lifetime is 2h, got %v", m.ttl)
	}
}

func TestTokens_SignAndParse(t *testing.T) {
	m := newTestTokens(t, issuerAccess, fixedNow)
	token, exp, err := m.sign("usr_123", map[string]any{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(fixedNow().Add(time.Hour)) {
		t.Fatalf("exp = %v", exp)
	}
	sub, claims, err := m.parse(token)
	if err != nil || sub != "usr_123" || claims["role"] != "admin" || claims["iss"] != issuerAccess {
		t.Fatalf("sub=%q claims=%v err=%v", sub, claims, err)
	}
}

func TestTokens_RegisteredClaimsWin(t *testing.T) {
	m := newTestTokens(t, issuerAccess, fixedNow)
	token, _, err := m.sign("real_sub", map[string]any{"sub": "evil_sub", "exp": float64(9999999999), "iat": float64(0), "iss": "evil"})
	if err != nil {
		t.Fatal(err)
	}
	sub, claims, err := m.parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if exp, _ := intClaim(claims, "exp"); sub != "real_sub" || exp != fixedNow().Add(time.Hour).Unix() || claims["iss"] != issuerAccess {
		t.Fatalf("the caller's claims must not override the registered ones: sub=%q claims=%v", sub, claims)
	}
}

func TestTokens_RefusesExpired(t *testing.T) {
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	signer := newTestTokens(t, issuerAccess, func() time.Time { return past })
	token, _, err := signer.sign("usr_old", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := newTestTokens(t, issuerAccess, fixedNow).parse(token); err == nil {
		t.Fatal("an expired token must be refused")
	}
	// A token without exp at all is refused too.
	noExp := signRaw(t, jwt.SigningMethodHS256, []byte(tokenKey), jwt.MapClaims{"sub": "usr_noexp", "iat": fixedNow().Unix(), "iss": issuerAccess})
	if _, _, err := newTestTokens(t, issuerAccess, fixedNow).parse(noExp); err == nil {
		t.Fatal("a token without exp must be refused")
	}
}

func TestTokens_RefusesFutureIssuedAt(t *testing.T) {
	future := fixedNow().Add(24 * time.Hour)
	token := signRaw(t, jwt.SigningMethodHS256, []byte(tokenKey), jwt.MapClaims{
		"sub": "usr_future", "iat": future.Unix(), "exp": future.Add(time.Hour).Unix(), "iss": issuerAccess,
	})
	if _, _, err := newTestTokens(t, issuerAccess, fixedNow).parse(token); err == nil {
		t.Fatal("a token issued in the future must be refused")
	}
	// Within the leeway, a little drift passes.
	drift := fixedNow().Add(10 * time.Second)
	token = signRaw(t, jwt.SigningMethodHS256, []byte(tokenKey), jwt.MapClaims{
		"sub": "usr_drift", "iat": drift.Unix(), "exp": drift.Add(time.Hour).Unix(), "iss": issuerAccess,
	})
	if _, _, err := newTestTokens(t, issuerAccess, fixedNow).parse(token); err != nil {
		t.Fatalf("10s of clock drift is within the leeway: %v", err)
	}
}

func TestTokens_RefusesForeignTokens(t *testing.T) {
	m := newTestTokens(t, issuerAccess, fixedNow)
	token, _, _ := m.sign("usr_123", nil)

	other, err := newTokens("another-key-that-is-32-bytes-ok!", issuerAccess, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other.now = fixedNow
	reset := newTestTokens(t, issuerReset, fixedNow)
	resetToken, _, _ := reset.sign("usr_123", nil)
	noSub, _, _ := m.sign("", nil)
	claims := jwt.MapClaims{"sub": "usr_123", "iat": fixedNow().Unix(), "exp": fixedNow().Add(time.Hour).Unix(), "iss": issuerAccess}
	cases := map[string]struct {
		parser *tokens
		token  string
	}{
		"another key":        {other, token},
		"garbage":            {m, "not.a.token"},
		"a reset token":      {m, resetToken},
		"an access token":    {reset, token},
		"no subject":         {m, noSub},
		"alg none":           {m, signRaw(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, claims)},
		"another HMAC (384)": {m, signRaw(t, jwt.SigningMethodHS384, []byte(tokenKey), claims)},
	}
	for name, c := range cases {
		if _, _, err := c.parser.parse(c.token); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestTokens_Clock(t *testing.T) {
	start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	token, _, err := newTestTokens(t, issuerAccess, func() time.Time { return start }).sign("usr_clock", nil)
	if err != nil {
		t.Fatal(err)
	}
	later := newTestTokens(t, issuerAccess, func() time.Time { return start.Add(30 * time.Minute) })
	if sub, _, err := later.parse(token); err != nil || sub != "usr_clock" {
		t.Fatalf("30 minutes into a 1h token: %q %v", sub, err)
	}
	expired := newTestTokens(t, issuerAccess, func() time.Time { return start.Add(2 * time.Hour) })
	if _, _, err := expired.parse(token); err == nil {
		t.Fatal("2h into a 1h token must fail")
	}
}
