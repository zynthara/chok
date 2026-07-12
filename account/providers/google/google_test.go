package google_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/account/providers/google"
)

// mockIdP is a stripped-down Google OIDC stand-in. It serves:
//   - GET  /.well-known/openid-configuration   (discovery)
//   - GET  /jwks                               (signing key)
//   - POST /token                              (code → access + id_token)
//
// Tests pre-stage the (code, claims) pair via mockIdP.Issue; the
// /token handler returns whatever code resolves to. Each helper is
// sized for one mock per test — concurrency / state-sharing across
// tests would need a more elaborate fixture.
type mockIdP struct {
	server   *httptest.Server
	signKey  *rsa.PrivateKey
	signJWK  jose.JSONWebKey
	keyID    string
	clientID string
	issuer   string
	codes    map[string]idTokenClaims // code → claims to issue at /token
}

type idTokenClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	HD            string `json:"hd,omitempty"`
	Nonce         string `json:"nonce,omitempty"`
	// Iss / Aud / Exp / Iat are filled at sign time so the test
	// doesn't need to repeat them per-stage.
	Iss string `json:"iss,omitempty"`
	Aud string `json:"aud,omitempty"`
	Exp int64  `json:"exp,omitempty"`
	Iat int64  `json:"iat,omitempty"`
}

func newMockIdP(t *testing.T, clientID string) *mockIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{
		Key:       priv.Public(),
		KeyID:     "kid-test",
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	mock := &mockIdP{
		signKey:  priv,
		signJWK:  jwk,
		keyID:    "kid-test",
		clientID: clientID,
		codes:    map[string]idTokenClaims{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", mock.serveDiscovery)
	mux.HandleFunc("/jwks", mock.serveJWKS)
	mux.HandleFunc("/token", mock.serveToken)
	mux.HandleFunc("/auth", mock.serveAuth)
	mock.server = httptest.NewServer(mux)
	mock.issuer = mock.server.URL
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *mockIdP) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	doc := map[string]any{
		"issuer":                                m.issuer,
		"authorization_endpoint":                m.issuer + "/auth",
		"token_endpoint":                        m.issuer + "/token",
		"jwks_uri":                              m.issuer + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (m *mockIdP) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{m.signJWK}})
}

// serveAuth is intentionally unused in tests — Module's BeginAuth flow
// just generates the redirect URL; we don't follow the user-agent
// hop. We mount it so the discovery doc's authorization_endpoint
// points somewhere valid.
func (m *mockIdP) serveAuth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (m *mockIdP) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	code := r.PostForm.Get("code")
	claims, ok := m.codes[code]
	if !ok {
		http.Error(w, "unknown code", 400)
		return
	}
	claims.Iss = m.issuer
	claims.Aud = m.clientID
	now := time.Now()
	claims.Iat = now.Unix()
	claims.Exp = now.Add(time.Hour).Unix()
	idToken, err := m.signIDToken(claims)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "mock-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

// Issue stages an id_token claim payload to be returned for the given
// authorization code on the next /token call.
func (m *mockIdP) Issue(code string, claims idTokenClaims) {
	m.codes[code] = claims
}

func (m *mockIdP) signIDToken(claims idTokenClaims) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.signKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", m.keyID),
	)
	if err != nil {
		return "", err
	}
	return jwt.Signed(signer).Claims(claims).Serialize()
}

// signWithWrongKey uses a separate, unrelated RSA key to sign — used
// by TestCompleteAuth_RejectsWrongSignature to exercise the JWKS
// mismatch path.
func (m *mockIdP) signWithWrongKey(claims idTokenClaims) (string, error) {
	wrong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: wrong},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", m.keyID),
	)
	if err != nil {
		return "", err
	}
	claims.Iss = m.issuer
	claims.Aud = m.clientID
	now := time.Now()
	claims.Iat = now.Unix()
	claims.Exp = now.Add(time.Hour).Unix()
	return jwt.Signed(signer).Claims(claims).Serialize()
}

// _ pulls big.Int into the import set so the test file compiles even
// after gofmt rearranges imports during edits — RSA signing pulls it
// transitively.
var _ = big.NewInt

// --- helpers --------------------------------------------------------

func newProvider(t *testing.T, mock *mockIdP, opts func(*google.Options)) account.AuthProvider {
	t.Helper()
	o := google.Options{
		ClientID:     mock.clientID,
		ClientSecret: "mock-secret",
		RedirectURL:  "https://app.example.test/auth/google/callback",
		IssuerURL:    mock.issuer,
	}
	if opts != nil {
		opts(&o)
	}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := google.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --- tests ----------------------------------------------------------

func TestOptions_Validate(t *testing.T) {
	cases := []struct {
		name string
		opts google.Options
		ok   bool
	}{
		{"missing client_id", google.Options{ClientSecret: "x", RedirectURL: "https://a/cb"}, false},
		{"missing client_secret", google.Options{ClientID: "x", RedirectURL: "https://a/cb"}, false},
		{"missing redirect_url", google.Options{ClientID: "x", ClientSecret: "x"}, false},
		{"relative redirect_url", google.Options{ClientID: "x", ClientSecret: "x", RedirectURL: "/cb"}, false},
		{"ok minimal", google.Options{ClientID: "x", ClientSecret: "x", RedirectURL: "https://a/cb"}, true},
		{"bad issuer_url", google.Options{ClientID: "x", ClientSecret: "x", RedirectURL: "https://a/cb", IssuerURL: "not-a-url"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestBeginAuth_BuildsAuthorizeURL(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	p := newProvider(t, mock, nil)

	resp, err := p.BeginAuth(context.Background(), &account.BeginRequest{
		State:         "state-1",
		Nonce:         "nonce-1",
		CodeChallenge: "challenge-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(resp.RedirectTo)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got := q.Get("client_id"); got != "client-abc" {
		t.Errorf("client_id = %q", got)
	}
	if got := q.Get("state"); got != "state-1" {
		t.Errorf("state = %q", got)
	}
	if got := q.Get("nonce"); got != "nonce-1" {
		t.Errorf("nonce = %q (expected nonce-1)", got)
	}
	if got := q.Get("code_challenge"); got != "challenge-1" {
		t.Errorf("code_challenge = %q", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q", got)
	}
	// Default scopes — openid is mandatory for OIDC.
	if scope := q.Get("scope"); !strings.Contains(scope, "openid") || !strings.Contains(scope, "email") || !strings.Contains(scope, "profile") {
		t.Errorf("scope missing default entries: %q", scope)
	}
}

func TestBeginAuth_HostedDomainParam(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	p := newProvider(t, mock, func(o *google.Options) {
		o.HostedDomain = "example.com"
	})
	resp, _ := p.BeginAuth(context.Background(), &account.BeginRequest{State: "s"})
	u, _ := url.Parse(resp.RedirectTo)
	if got := u.Query().Get("hd"); got != "example.com" {
		t.Fatalf("hd = %q", got)
	}
}

func TestCompleteAuth_HappyPath(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	p := newProvider(t, mock, nil)

	mock.Issue("code-1", idTokenClaims{
		Sub:           "google-sub-99",
		Email:         "alice@example.com",
		EmailVerified: true,
		Name:          "Alice",
		Picture:       "https://lh.example/alice.jpg",
		Nonce:         "nonce-1",
	})

	pi, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{
		Code:  "code-1",
		Nonce: "nonce-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pi.Provider != "google" || pi.ProviderAccountID != "google-sub-99" {
		t.Errorf("identity wrong: %+v", pi)
	}
	if pi.Email != "alice@example.com" || !pi.EmailVerified {
		t.Errorf("email/verified wrong: %+v", pi)
	}
	if pi.Name != "Alice" || pi.Picture == "" {
		t.Errorf("profile fields wrong: %+v", pi)
	}
	if pi.IsAliasedEmail {
		t.Error("Google never sets IsAliasedEmail=true")
	}
}

func TestCompleteAuth_RejectsNonceMismatch(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	p := newProvider(t, mock, nil)

	mock.Issue("code-2", idTokenClaims{
		Sub:           "x",
		Email:         "x@example.com",
		EmailVerified: true,
		Nonce:         "issued-nonce",
	})
	_, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{
		Code:  "code-2",
		Nonce: "expected-different-nonce",
	})
	if err == nil || !strings.Contains(err.Error(), "nonce mismatch") {
		t.Fatalf("expected nonce mismatch error, got %v", err)
	}
}

func TestCompleteAuth_RejectsWrongAudience(t *testing.T) {
	// Build provider expecting client-abc, but the mock signs with
	// aud=different-client; verifier rejects the audience mismatch.
	wrongAudMock := newMockIdP(t, "different-client")
	// Point provider at wrongAudMock's issuer (so it discovers the
	// JWKS there) but with the original clientID.
	p, err := google.New(context.Background(), google.Options{
		ClientID:     "client-abc",
		ClientSecret: "secret",
		RedirectURL:  "https://app.example.test/cb",
		IssuerURL:    wrongAudMock.issuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongAudMock.Issue("code-3", idTokenClaims{
		Sub: "x", Email: "x@example.com", EmailVerified: true,
	})
	_, err = p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "code-3"})
	if err == nil {
		t.Fatal("expected aud mismatch error")
	}
	// coreos/go-oidc's verifier returns an error mentioning audience.
	if !strings.Contains(strings.ToLower(err.Error()), "audience") {
		t.Logf("error not specifically about audience: %v", err)
	}
}

func TestCompleteAuth_RejectsWrongSignature(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	p := newProvider(t, mock, nil)

	// Manually construct an id_token signed with a different key but
	// otherwise correct claims. Bypass the mock's /token by hooking
	// the codes map directly to a custom signed token via httptest.
	wrongSigned, err := mock.signWithWrongKey(idTokenClaims{
		Sub:           "x",
		Email:         "x@example.com",
		EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Replace the /token handler temporarily to return our forged token.
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			mock.serveDiscovery(w, r)
		case "/jwks":
			mock.serveJWKS(w, r)
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"access_token":"x","token_type":"Bearer","expires_in":3600,"id_token":%q}`, wrongSigned)
		default:
			http.NotFound(w, r)
		}
	})
	_, err = p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "any"})
	if err == nil || !strings.Contains(err.Error(), "id_token verify") {
		t.Fatalf("expected verify failure, got %v", err)
	}
}

func TestCompleteAuth_RejectsHostedDomainMismatch(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	p := newProvider(t, mock, func(o *google.Options) {
		o.HostedDomain = "company.com"
	})
	mock.Issue("code-4", idTokenClaims{
		Sub:           "x",
		Email:         "x@othercorp.com",
		EmailVerified: true,
		HD:            "othercorp.com",
	})
	_, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "code-4"})
	if err == nil || !strings.Contains(err.Error(), "hosted domain") {
		t.Fatalf("expected hosted domain mismatch, got %v", err)
	}
}

func TestCompleteAuth_HostedDomainAccepted(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	p := newProvider(t, mock, func(o *google.Options) {
		o.HostedDomain = "company.com"
	})
	mock.Issue("code-5", idTokenClaims{
		Sub:           "x",
		Email:         "alice@company.com",
		EmailVerified: true,
		HD:            "company.com",
	})
	pi, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "code-5"})
	if err != nil {
		t.Fatal(err)
	}
	if hd, _ := pi.Raw["hosted_domain"].(string); hd != "company.com" {
		t.Errorf("hosted_domain raw = %q", hd)
	}
}

func TestCompleteAuth_TokenExchangeFailure(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	p := newProvider(t, mock, nil)

	// Don't Issue any code — /token returns 400 unknown code.
	_, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "nope"})
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("expected token exchange failure, got %v", err)
	}
}

func TestProvider_SpecShape(t *testing.T) {
	spec := google.Provider()
	if spec.Name != "google" {
		t.Fatalf("spec name = %q, want google", spec.Name)
	}
	if spec.Build == nil {
		t.Fatal("spec Build is nil")
	}
}

func TestProvider_DecodeRoundTrip(t *testing.T) {
	mock := newMockIdP(t, "client-abc")
	spec := google.Provider()
	p, err := spec.Build(context.Background(), map[string]any{
		"client_id":     "client-abc",
		"client_secret": "secret",
		"redirect_url":  "https://app.example.test/cb",
		"issuer_url":    mock.issuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "google" {
		t.Fatalf("name = %q", p.Name())
	}
}

func TestProvider_RejectsInvalidConfig(t *testing.T) {
	if _, err := google.Provider().Build(context.Background(), map[string]any{
		"client_id": "only-id",
	}); err == nil {
		t.Fatal("expected validation error for incomplete provider config")
	}
}
