// Package idptest is an OpenID Connect provider in process, for the
// provider packages' tests: discovery, the signing keys, an authorize
// stub and a token endpoint that answers the ID token a test staged for
// a code, signed with RS256.
package idptest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// IdP is the provider. Its issuer is its URL.
type IdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	mu    sync.Mutex
	codes map[string]map[string]any
	forms []url.Values
	token http.HandlerFunc // replaces the token endpoint when set
}

// The paths it serves under its URL.
const (
	AuthorizePath = "/authorize"
	TokenPath     = "/token"
	KeysPath      = "/keys"
)

// New starts an IdP that stops with the test.
func New(t testing.TB) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &IdP{key: key, kid: "kid-test", codes: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer": p.URL(), "authorization_endpoint": p.URL() + AuthorizePath, "token_endpoint": p.URL() + TokenPath,
			"jwks_uri": p.URL() + KeysPath, "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET "+KeysPath, func(w http.ResponseWriter, _ *http.Request) {
		pub := key.PublicKey
		writeJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": p.kid, "use": "sig", "alg": "RS256",
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("GET "+AuthorizePath, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST "+TokenPath, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		override := p.token
		p.mu.Unlock()
		if override != nil {
			override(w, r)
			return
		}
		p.serveToken(w, r)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// URL is the issuer.
func (p *IdP) URL() string { return p.srv.URL }

// Issue stages the ID token claims the token endpoint answers for code;
// iss, aud (the requesting client), iat and exp are added unless set.
func (p *IdP) Issue(code string, claims map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes[code] = maps.Clone(claims)
}

// Forms are the token requests received, oldest first.
func (p *IdP) Forms() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]url.Values(nil), p.forms...)
}

// ServeToken replaces the token endpoint.
func (p *IdP) ServeToken(h http.HandlerFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = h
}

// Sign signs claims as an ID token with the IdP's key, or with another
// key when foreign — a forged token.
func (p *IdP) Sign(t testing.TB, claims map[string]any, foreign bool) string {
	t.Helper()
	key := p.key
	if foreign {
		var err error
		if key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			t.Fatal(err)
		}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(claims))
	tok.Header["kid"] = p.kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Complete fills the claims the token endpoint adds.
func (p *IdP) Complete(claims map[string]any, clientID string) map[string]any {
	out := maps.Clone(claims)
	now := time.Now()
	for k, v := range map[string]any{"iss": p.URL(), "aud": clientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()} {
		if _, set := out[k]; !set {
			out[k] = v
		}
	}
	return out
}

func (p *IdP) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clientID := r.PostForm.Get("client_id")
	if id, _, ok := r.BasicAuth(); ok {
		clientID = id
	}
	p.mu.Lock()
	form := maps.Clone(r.PostForm)
	if form.Get("client_id") == "" {
		form.Set("client_id", clientID)
	}
	p.forms = append(p.forms, form)
	claims, ok := p.codes[r.PostForm.Get("code")]
	p.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(p.Complete(claims, clientID)))
	tok.Header["kid"] = p.kid
	idToken, err := tok.SignedString(p.key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"access_token": "mock-access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
