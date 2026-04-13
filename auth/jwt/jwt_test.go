package jwt

import (
	"strings"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// 32-byte raw key for tests.
const testKeyRaw = "test-key-that-is-exactly-32-byte"

func fixedNow() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(Options{
		SigningKey:  testKeyRaw,
		Expiration: time.Hour,
		Now:        fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- NewManager ---

func TestNewManager_EmptyKey(t *testing.T) {
	_, err := NewManager(Options{SigningKey: ""})
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestNewManager_ShortKey(t *testing.T) {
	_, err := NewManager(Options{SigningKey: "short"})
	if err == nil {
		t.Fatal("expected error for short key")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewManager_RawKey(t *testing.T) {
	m, err := NewManager(Options{SigningKey: testKeyRaw})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.key) != 32 {
		t.Fatalf("key length = %d, want 32", len(m.key))
	}
}

func TestNewManager_DefaultExpiration(t *testing.T) {
	m, err := NewManager(Options{SigningKey: testKeyRaw})
	if err != nil {
		t.Fatal(err)
	}
	if m.opts.Expiration != 2*time.Hour {
		t.Fatalf("default expiration = %v, want 2h", m.opts.Expiration)
	}
}

// --- Sign + Parse round-trip ---

func TestSignAndParse(t *testing.T) {
	m := newTestManager(t)
	token, exp, err := m.Sign("usr_123", map[string]any{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}

	expectedExp := fixedNow().Add(time.Hour)
	if !exp.Equal(expectedExp) {
		t.Fatalf("exp = %v, want %v", exp, expectedExp)
	}

	sub, claims, err := m.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if sub != "usr_123" {
		t.Fatalf("subject = %q, want usr_123", sub)
	}
	if claims["role"] != "admin" {
		t.Fatalf("claims[role] = %v, want admin", claims["role"])
	}
}

// --- Sign: registered claims cannot be overridden ---

func TestSign_CannotOverrideRegisteredClaims(t *testing.T) {
	m := newTestManager(t)

	// Caller tries to override sub, exp, iat.
	token, _, err := m.Sign("real_sub", map[string]any{
		"sub": "evil_sub",
		"exp": float64(9999999999),
		"iat": float64(0),
	})
	if err != nil {
		t.Fatal(err)
	}

	sub, claims, err := m.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	// sub must be from the Sign argument, not from claims map.
	if sub != "real_sub" {
		t.Fatalf("subject = %q, want real_sub (caller override should be ignored)", sub)
	}
	// exp should be now+1h, not the evil value.
	expVal, _ := claims["exp"].(float64)
	expected := float64(fixedNow().Add(time.Hour).Unix())
	if expVal != expected {
		t.Fatalf("exp = %v, want %v (caller override should be ignored)", expVal, expected)
	}
}

// --- Parse validation ---

func TestParse_ExpiredToken(t *testing.T) {
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	m, _ := NewManager(Options{
		SigningKey:  testKeyRaw,
		Expiration: time.Hour,
		Now:        func() time.Time { return past },
	})
	token, _, _ := m.Sign("usr_123", nil)

	// Parse with current time — token is expired.
	m2, _ := NewManager(Options{
		SigningKey:  testKeyRaw,
		Expiration: time.Hour,
		Now:        fixedNow,
	})
	_, _, err := m2.Parse(token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestParse_MissingExp_Rejected(t *testing.T) {
	m := newTestManager(t)

	// Craft a token without exp claim using the library directly.
	mc := jwtv5.MapClaims{
		"sub": "usr_noexp",
		"iat": fixedNow().Unix(),
		// no "exp"
	}
	token := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, mc)
	signed, _ := token.SignedString(m.key)

	_, _, err := m.Parse(signed)
	if err == nil {
		t.Fatal("expected error for token without exp (WithExpirationRequired)")
	}
}

func TestParse_FutureIat_Rejected(t *testing.T) {
	m := newTestManager(t)

	// Craft a token with iat in the future.
	future := fixedNow().Add(24 * time.Hour)
	mc := jwtv5.MapClaims{
		"sub": "usr_future",
		"iat": future.Unix(),
		"exp": future.Add(time.Hour).Unix(),
	}
	token := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, mc)
	signed, _ := token.SignedString(m.key)

	_, _, err := m.Parse(signed)
	if err == nil {
		t.Fatal("expected error for future iat (WithIssuedAt)")
	}
}

func TestParse_WrongKey(t *testing.T) {
	m := newTestManager(t)
	token, _, _ := m.Sign("usr_123", nil)

	other, _ := NewManager(Options{
		SigningKey: "another-key-that-is-32-bytes-ok!",
		Now:       fixedNow,
	})
	_, _, err := other.Parse(token)
	if err == nil {
		t.Fatal("expected error for wrong key")
	}
}

func TestParse_InvalidToken(t *testing.T) {
	m := newTestManager(t)
	_, _, err := m.Parse("not.a.token")
	if err == nil {
		t.Fatal("expected error for garbage token")
	}
}

func TestParse_Issuer(t *testing.T) {
	m, _ := NewManager(Options{
		SigningKey: testKeyRaw,
		Issuer:    "myapp",
		Now:       fixedNow,
	})
	token, _, _ := m.Sign("usr_123", nil)

	// Same issuer — should work.
	sub, _, err := m.Parse(token)
	if err != nil {
		t.Fatalf("valid issuer should pass: %v", err)
	}
	if sub != "usr_123" {
		t.Fatalf("subject = %q, want usr_123", sub)
	}

	// Different issuer — should fail.
	m2, _ := NewManager(Options{
		SigningKey: testKeyRaw,
		Issuer:    "other",
		Now:       fixedNow,
	})
	_, _, err = m2.Parse(token)
	if err == nil {
		t.Fatal("expected error for mismatched issuer")
	}
}

func TestParse_MissingSubject(t *testing.T) {
	m := newTestManager(t)
	token, _, _ := m.Sign("", nil)
	_, _, err := m.Parse(token)
	if err == nil {
		t.Fatal("expected error for missing subject")
	}
}

// --- TokenParser interface ---

func TestManager_SatisfiesTokenParser(t *testing.T) {
	type tokenParser interface {
		Parse(token string) (string, map[string]any, error)
	}
	m := newTestManager(t)
	var _ tokenParser = m

	token, _, _ := m.Sign("usr_x", nil)
	sub, _, err := m.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if sub != "usr_x" {
		t.Fatalf("subject = %q, want usr_x", sub)
	}
}

// --- Clock injection ---

func TestClockInjection(t *testing.T) {
	now1 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now2 := now1.Add(30 * time.Minute)

	m1, _ := NewManager(Options{
		SigningKey:  testKeyRaw,
		Expiration: time.Hour,
		Now:        func() time.Time { return now1 },
	})
	m2, _ := NewManager(Options{
		SigningKey:  testKeyRaw,
		Expiration: time.Hour,
		Now:        func() time.Time { return now2 },
	})

	token, _, _ := m1.Sign("usr_clock", nil)

	// m2's clock is 30min ahead — within 1h expiration, should still parse.
	sub, _, err := m2.Parse(token)
	if err != nil {
		t.Fatalf("token should still be valid: %v", err)
	}
	if sub != "usr_clock" {
		t.Fatalf("subject = %q, want usr_clock", sub)
	}

	// m3's clock is 2h ahead — token expired.
	m3, _ := NewManager(Options{
		SigningKey:  testKeyRaw,
		Expiration: time.Hour,
		Now:        func() time.Time { return now1.Add(2 * time.Hour) },
	})
	_, _, err = m3.Parse(token)
	if err == nil {
		t.Fatal("expected error for expired token with advanced clock")
	}
}
