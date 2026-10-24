package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// issuer is an OpenID Connect issuer in process: discovery and a key set
// that tests swap.
type issuer struct {
	srv      *httptest.Server
	keys     atomic.Pointer[[]map[string]string]
	fetches  atomic.Int32
	issuerAs string // the issuer the document claims; the server's URL by default
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := is.issuerAs
		if iss == "" {
			iss = is.srv.URL
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": iss, "authorization_endpoint": is.srv.URL + "/auth", "token_endpoint": is.srv.URL + "/token", "jwks_uri": is.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		is.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": *is.keys.Load()})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	is.keys.Store(&[]map[string]string{})
	return is
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rsaJWK(kid string, pub *rsa.PublicKey) map[string]string {
	return map[string]string{"kty": "RSA", "kid": kid, "use": "sig", "n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes())}
}

func ecJWK(kid string, pub *ecdsa.PublicKey) map[string]string {
	raw, _ := pub.Bytes() // 0x04 || x || y
	size := (len(raw) - 1) / 2
	return map[string]string{"kty": "EC", "kid": kid, "crv": "P-256", "x": b64(raw[1 : 1+size]), "y": b64(raw[1+size:])}
}

func (is *issuer) publish(jwks ...map[string]string) { is.keys.Store(&jwks) }

func (is *issuer) claims(aud string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{"iss": is.srv.URL, "aud": aud, "sub": "sub-1", "email": "a@idp.test", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": "n-1"}
}

func sign(t *testing.T, method jwt.SigningMethod, kid string, key crypto.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type idClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Nonce string `json:"nonce"`
}

func TestVerify_RS256AndES256(t *testing.T) {
	is := newIssuer(t)
	rk := rsaKey(t)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	is.publish(rsaJWK("r1", &rk.PublicKey), ecJWK("e1", &ek.PublicKey))
	i := New(is.srv.URL)
	for name, raw := range map[string]string{
		"RS256": sign(t, jwt.SigningMethodRS256, "r1", rk, is.claims("client-1")),
		"ES256": sign(t, jwt.SigningMethodES256, "e1", ek, is.claims("client-1")),
	} {
		var c idClaims
		if err := i.Verify(t.Context(), raw, "client-1", &c); err != nil || c.Sub != "sub-1" || c.Email != "a@idp.test" || c.Nonce != "n-1" {
			t.Errorf("%s: %+v %v", name, c, err)
		}
	}
	if is.fetches.Load() != 1 {
		t.Fatalf("the key set is fetched once: %d", is.fetches.Load())
	}
}

func TestVerify_Refuses(t *testing.T) {
	is := newIssuer(t)
	rk, other := rsaKey(t), rsaKey(t)
	is.publish(rsaJWK("r1", &rk.PublicKey))
	i := New(is.srv.URL)
	expired := is.claims("client-1")
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	noExp := is.claims("client-1")
	delete(noExp, "exp")
	foreign := is.claims("client-1")
	foreign["iss"] = "https://evil.test"
	future := is.claims("client-1")
	future["iat"] = time.Now().Add(time.Hour).Unix()
	pem := rk.N.Bytes() // the public key's bytes as an HMAC secret
	for name, raw := range map[string]string{
		"another audience": sign(t, jwt.SigningMethodRS256, "r1", rk, is.claims("client-2")),
		"another issuer":   sign(t, jwt.SigningMethodRS256, "r1", rk, foreign),
		"a forged key":     sign(t, jwt.SigningMethodRS256, "r1", other, is.claims("client-1")),
		"expired":          sign(t, jwt.SigningMethodRS256, "r1", rk, expired),
		"no exp":           sign(t, jwt.SigningMethodRS256, "r1", rk, noExp),
		"issued later":     sign(t, jwt.SigningMethodRS256, "r1", rk, future),
		"alg none":         sign(t, jwt.SigningMethodNone, "r1", jwt.UnsafeAllowNoneSignatureType, is.claims("client-1")),
		"HMAC confusion":   sign(t, jwt.SigningMethodHS256, "r1", pem, is.claims("client-1")),
		"garbage":          "not.a.token",
	} {
		var c idClaims
		if err := i.Verify(t.Context(), raw, "client-1", &c); err == nil {
			t.Errorf("%s passed", name)
		}
	}
}

func TestVerify_FollowsKeyRotation(t *testing.T) {
	is := newIssuer(t)
	old, rotated := rsaKey(t), rsaKey(t)
	is.publish(rsaJWK("old", &old.PublicKey))
	i := New(is.srv.URL)
	now := time.Now()
	i.now = func() time.Time { return now }
	var c idClaims
	if err := i.Verify(t.Context(), sign(t, jwt.SigningMethodRS256, "old", old, is.claims("c")), "c", &c); err != nil {
		t.Fatal(err)
	}
	is.publish(rsaJWK("new", &rotated.PublicKey))
	token := sign(t, jwt.SigningMethodRS256, "new", rotated, is.claims("c"))
	// Within a minute of the last fetch, an unknown kid does not refetch.
	if err := i.Verify(t.Context(), token, "c", &c); err == nil || is.fetches.Load() != 1 {
		t.Fatalf("an unknown kid right after a fetch: %v (fetches %d)", err, is.fetches.Load())
	}
	now = now.Add(2 * time.Minute)
	if err := i.Verify(t.Context(), token, "c", &c); err != nil || is.fetches.Load() != 2 {
		t.Fatalf("after the rotation: %v (fetches %d)", err, is.fetches.Load())
	}
}

func TestMetadata_CheckedAndCached(t *testing.T) {
	is := newIssuer(t)
	i := New(is.srv.URL + "/")
	m, err := i.Metadata(t.Context())
	if err != nil || m.AuthorizationEndpoint != is.srv.URL+"/auth" || m.TokenEndpoint != is.srv.URL+"/token" {
		t.Fatalf("%+v %v", m, err)
	}
	// A document for another issuer is refused, and not cached.
	bad := newIssuer(t)
	bad.issuerAs = "https://accounts.example.test"
	if _, err := New(bad.srv.URL).Metadata(t.Context()); err == nil || !strings.Contains(err.Error(), "describes the issuer") {
		t.Fatalf("a mismatched issuer: %v", err)
	}
	down := New("http://127.0.0.1:1")
	if _, err := down.Metadata(t.Context()); err == nil || down.meta != nil {
		t.Fatalf("an unreachable issuer: %v", err)
	}
}
