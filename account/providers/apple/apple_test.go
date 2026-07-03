package apple_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/account/providers/apple"
)

// generateTestPEM produces a fresh PKCS8-wrapped P-256 ECDSA key in
// PEM format — exactly the layout Apple .p8 files use.
func generateTestPEM(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// generateRSAKeyPEM returns a PKCS8-wrapped *RSA* key — used to verify
// apple.parsePrivateKey rejects non-ECDSA inputs.
func generateRSAKeyPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// --- Options.Validate -----------------------------------------------

func TestOptions_Validate(t *testing.T) {
	goodPEM := generateTestPEM(t)
	cases := []struct {
		name    string
		opts    apple.Options
		wantErr string
	}{
		{
			"missing service_id",
			apple.Options{TeamID: "1234567890", KeyID: "ABCDEFGHIJ", PrivateKey: goodPEM, RedirectURL: "https://a/cb", ClientSecretTTL: time.Hour},
			"service_id",
		},
		{
			"team_id wrong length",
			apple.Options{ServiceID: "x", TeamID: "short", KeyID: "ABCDEFGHIJ", PrivateKey: goodPEM, RedirectURL: "https://a/cb", ClientSecretTTL: time.Hour},
			"team_id",
		},
		{
			"key_id wrong length",
			apple.Options{ServiceID: "x", TeamID: "1234567890", KeyID: "x", PrivateKey: goodPEM, RedirectURL: "https://a/cb", ClientSecretTTL: time.Hour},
			"key_id",
		},
		{
			"missing private_key",
			apple.Options{ServiceID: "x", TeamID: "1234567890", KeyID: "ABCDEFGHIJ", RedirectURL: "https://a/cb", ClientSecretTTL: time.Hour},
			"private_key",
		},
		{
			"private_key not PEM",
			apple.Options{ServiceID: "x", TeamID: "1234567890", KeyID: "ABCDEFGHIJ", PrivateKey: "not a pem", RedirectURL: "https://a/cb", ClientSecretTTL: time.Hour},
			"PEM-encoded",
		},
		{
			"missing redirect_url",
			apple.Options{ServiceID: "x", TeamID: "1234567890", KeyID: "ABCDEFGHIJ", PrivateKey: goodPEM, ClientSecretTTL: time.Hour},
			"redirect_url",
		},
		{
			"ttl too small",
			apple.Options{ServiceID: "x", TeamID: "1234567890", KeyID: "ABCDEFGHIJ", PrivateKey: goodPEM, RedirectURL: "https://a/cb", ClientSecretTTL: 0},
			"client_secret_ttl",
		},
		{
			"ttl > 180d",
			apple.Options{ServiceID: "x", TeamID: "1234567890", KeyID: "ABCDEFGHIJ", PrivateKey: goodPEM, RedirectURL: "https://a/cb", ClientSecretTTL: 200 * 24 * time.Hour},
			"180 days",
		},
		{
			"ok minimal",
			apple.Options{ServiceID: "x", TeamID: "1234567890", KeyID: "ABCDEFGHIJ", PrivateKey: goodPEM, RedirectURL: "https://a/cb", ClientSecretTTL: 30 * 24 * time.Hour},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected ok, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestNew_RejectsRSAKey covers SPEC §3 v0.3.5 PEM fail-fast: New must
// surface "private key is not ECDSA" at construction time when the
// PEM wraps an RSA key, not later at first signing.
func TestNew_RejectsRSAKey(t *testing.T) {
	rsaPEM := generateRSAKeyPEM(t)
	_, err := apple.New(context.Background(), apple.Options{
		ServiceID:       "x",
		TeamID:          "1234567890",
		KeyID:           "ABCDEFGHIJ",
		PrivateKey:      rsaPEM,
		RedirectURL:     "https://a/cb",
		ClientSecretTTL: time.Hour,
		IssuerURL:       "https://example.test", // skip discovery
	})
	if err == nil || !strings.Contains(err.Error(), "ECDSA") {
		t.Fatalf("expected ECDSA rejection, got %v", err)
	}
}

func TestNew_RejectsCorruptPEM(t *testing.T) {
	corrupt := "-----BEGIN PRIVATE KEY-----\nXXXNOT-VALID-BASE64XXX\n-----END PRIVATE KEY-----\n"
	_, err := apple.New(context.Background(), apple.Options{
		ServiceID:       "x",
		TeamID:          "1234567890",
		KeyID:           "ABCDEFGHIJ",
		PrivateKey:      corrupt,
		RedirectURL:     "https://a/cb",
		ClientSecretTTL: time.Hour,
		IssuerURL:       "https://example.test",
	})
	if err == nil {
		t.Fatal("expected error on corrupt PEM")
	}
}

// --- mock Apple IdP --------------------------------------------------

// mockApple stands in for appleid.apple.com. Serves discovery, JWKS,
// /auth/token (with audience-aware id_token signing), and a stub
// /auth/authorize.
type mockApple struct {
	server   *httptest.Server
	signKey  *rsa.PrivateKey
	signJWK  jose.JSONWebKey
	keyID    string
	clientID string
	issuer   string

	codes map[string]appleIDClaims
}

type appleIDClaims struct {
	Sub            string `json:"sub"`
	Email          string `json:"email"`
	EmailVerified  any    `json:"email_verified"`
	IsPrivateEmail any    `json:"is_private_email"`
	Nonce          string `json:"nonce,omitempty"`

	Iss string `json:"iss,omitempty"`
	Aud string `json:"aud,omitempty"`
	Iat int64  `json:"iat,omitempty"`
	Exp int64  `json:"exp,omitempty"`
}

func newMockApple(t *testing.T, clientID string) *mockApple {
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
	mock := &mockApple{
		signKey:  priv,
		signJWK:  jwk,
		keyID:    "kid-test",
		clientID: clientID,
		codes:    map[string]appleIDClaims{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", mock.serveDiscovery)
	mux.HandleFunc("/auth/keys", mock.serveJWKS)
	mux.HandleFunc("/auth/token", mock.serveToken)
	mux.HandleFunc("/auth/authorize", mock.serveAuthorize)
	mock.server = httptest.NewServer(mux)
	mock.issuer = mock.server.URL
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *mockApple) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                m.issuer,
		"authorization_endpoint":                m.issuer + "/auth/authorize",
		"token_endpoint":                        m.issuer + "/auth/token",
		"jwks_uri":                              m.issuer + "/auth/keys",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (m *mockApple) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{m.signJWK}})
}

func (m *mockApple) serveAuthorize(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(200)
}

func (m *mockApple) serveToken(w http.ResponseWriter, r *http.Request) {
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
		"access_token":  "mock-access",
		"token_type":    "Bearer",
		"expires_in":    3600,
		"id_token":      idToken,
		"refresh_token": "mock-refresh",
	})
}

func (m *mockApple) signIDToken(claims appleIDClaims) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.signKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", m.keyID),
	)
	if err != nil {
		return "", err
	}
	return jwt.Signed(signer).Claims(claims).Serialize()
}

func (m *mockApple) Issue(code string, claims appleIDClaims) {
	m.codes[code] = claims
}

// --- helpers --------------------------------------------------------

func newProvider(t *testing.T, mock *mockApple, opts func(*apple.Options)) account.AuthProvider {
	t.Helper()
	o := apple.Options{
		ServiceID:       mock.clientID,
		TeamID:          "TEAM123456",
		KeyID:           "KEY1234567",
		PrivateKey:      generateTestPEM(t),
		RedirectURL:     "https://app.example.test/auth/apple/callback",
		ClientSecretTTL: 30 * 24 * time.Hour,
		IssuerURL:       mock.issuer,
	}
	if opts != nil {
		opts(&o)
	}
	p, err := apple.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --- BeginAuth ------------------------------------------------------

func TestBeginAuth_RequestsFormPostAndNonce(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)

	resp, err := p.BeginAuth(context.Background(), &account.BeginRequest{
		State: "state-1",
		Nonce: "nonce-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(resp.RedirectTo)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got := q.Get("response_mode"); got != "form_post" {
		t.Errorf("response_mode = %q, want form_post", got)
	}
	if got := q.Get("nonce"); got != "nonce-1" {
		t.Errorf("nonce = %q", got)
	}
	if got := q.Get("scope"); !strings.Contains(got, "name") || !strings.Contains(got, "email") {
		t.Errorf("scope missing name+email: %q", got)
	}
}

func TestCapabilities_FormPostNonceOnly(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)
	caps := p.Capabilities()
	if caps.CallbackMethod != "POST" {
		t.Errorf("CallbackMethod = %q", caps.CallbackMethod)
	}
	if !caps.RequiresFormPost {
		t.Error("RequiresFormPost must be true")
	}
	if !caps.RequiresNonce {
		t.Error("RequiresNonce must be true")
	}
	if caps.SupportsPKCE {
		t.Error("Apple does not support PKCE; SupportsPKCE must be false")
	}
}

// --- CompleteAuth ---------------------------------------------------

func TestCompleteAuth_HappyPath(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)

	mock.Issue("code-1", appleIDClaims{
		Sub:            "apple-sub-001",
		Email:          "alice@icloud.com",
		EmailVerified:  "true", // Apple's stringly-typed bool
		IsPrivateEmail: "false",
		Nonce:          "nonce-1",
	})

	pi, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{
		Code:  "code-1",
		Nonce: "nonce-1",
		FormBody: url.Values{
			"user": {`{"name":{"firstName":"Alice","lastName":"Iverson"}}`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pi.ProviderAccountID != "apple-sub-001" {
		t.Errorf("account_id = %q", pi.ProviderAccountID)
	}
	if pi.Email != "alice@icloud.com" || !pi.EmailVerified {
		t.Errorf("email/verified = %q/%v", pi.Email, pi.EmailVerified)
	}
	if pi.IsAliasedEmail {
		t.Error("non-private email should set IsAliasedEmail=false")
	}
	if pi.Name != "Alice Iverson" {
		t.Errorf("Name not parsed from user form field: %q", pi.Name)
	}
}

// TestCompleteAuth_PrivateRelayMapsToAliased covers SPEC §4.4: when
// is_private_email=true the IdP returned a privaterelay alias and we
// MUST surface IsAliasedEmail=true so SPEC §8 LinkByEmail and §8.1
// create-user gate both reject it.
func TestCompleteAuth_PrivateRelayMapsToAliased(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)

	mock.Issue("code-2", appleIDClaims{
		Sub:            "apple-sub-002",
		Email:          "abc123@privaterelay.appleid.com",
		EmailVerified:  "true",
		IsPrivateEmail: "true",
		Nonce:          "n",
	})
	pi, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{
		Code: "code-2", Nonce: "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pi.IsAliasedEmail {
		t.Error("is_private_email=true MUST set IsAliasedEmail=true")
	}
	// EmailVerified is still true (Apple verified the alias address);
	// the gate that prevents auto-link is IsAliasedEmail, not the
	// EmailVerified flag.
	if !pi.EmailVerified {
		t.Error("Apple verified the alias; EmailVerified should be true")
	}
}

// TestCompleteAuth_BoolEmailVerified covers Apple's other id_token
// shape: email_verified arrives as a real bool rather than a string.
// Our coerceBoolClaim must accept both.
func TestCompleteAuth_BoolEmailVerified(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)

	mock.Issue("code-bool", appleIDClaims{
		Sub:            "apple-sub-003",
		Email:          "x@example.com",
		EmailVerified:  true, // bool, not string
		IsPrivateEmail: false,
	})
	pi, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "code-bool"})
	if err != nil {
		t.Fatal(err)
	}
	if !pi.EmailVerified {
		t.Error("bool email_verified=true must coerce to true")
	}
}

// TestCompleteAuth_SecondLoginNoUserField covers Apple's "user only on
// first login" behaviour: subsequent callbacks have no `user` form
// field, so Name must come back empty and login still succeeds.
func TestCompleteAuth_SecondLoginNoUserField(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)

	mock.Issue("code-3", appleIDClaims{
		Sub:            "apple-sub-004",
		Email:          "x@example.com",
		EmailVerified:  "true",
		IsPrivateEmail: "false",
	})
	pi, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{
		Code: "code-3",
		// No FormBody.user — second login.
		FormBody: url.Values{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pi.Name != "" {
		t.Errorf("Name should be empty on second login; got %q", pi.Name)
	}
	if pi.ProviderAccountID == "" {
		t.Error("login should still succeed without user field")
	}
}

// TestCompleteAuth_RejectsNonceMismatch covers OIDC §15.5.2: nonce
// claim must equal the per-request nonce Module stashed in OAuthSession.
func TestCompleteAuth_RejectsNonceMismatch(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)

	mock.Issue("code-4", appleIDClaims{
		Sub:            "x",
		Email:          "x@example.com",
		EmailVerified:  "true",
		IsPrivateEmail: "false",
		Nonce:          "issued-nonce",
	})
	_, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{
		Code:  "code-4",
		Nonce: "expected-different-nonce",
	})
	if err == nil || !strings.Contains(err.Error(), "nonce mismatch") {
		t.Fatalf("expected nonce mismatch error, got %v", err)
	}
}

func TestCompleteAuth_TokenExchangeFailure(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)

	_, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "no-code"})
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("expected token exchange error, got %v", err)
	}
}

// --- client_secret cache --------------------------------------------

// TestClientSecret_ConcurrentSignsOnce drives the SPEC §7 invariant:
// 1000 goroutines hammering the cache produce exactly one signature.
//
// We probe the cache indirectly through the public CompleteAuth path,
// pre-staging the same code for every goroutine so the only contention
// point is the secret cache. After all goroutines finish, the mock
// must have issued the SAME client_secret on every /token request
// (one signature, reused by all).
func TestClientSecret_ConcurrentSignsOnce(t *testing.T) {
	mock := newMockApple(t, "com.example.web")
	p := newProvider(t, mock, nil)

	// Track every distinct client_secret value the mock saw on /token
	// requests. We swap in a wrapping handler around the existing
	// /auth/token handler so the test owns the inspection.
	var observedMu sync.Mutex
	observed := map[string]int{}
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			_ = r.ParseForm()
			observedMu.Lock()
			observed[r.PostForm.Get("client_secret")]++
			observedMu.Unlock()
		}
		// Defer to the previously-installed mux.
		mock.server.Config.Handler.ServeHTTP(w, r)
	})
	// Reinstate a non-recursive handler — the wrapper above creates
	// infinite recursion. Run the wrapper one layer; underlying mux
	// has the actual route handlers.
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/token" {
				_ = r.ParseForm()
				observedMu.Lock()
				observed[r.PostForm.Get("client_secret")]++
				observedMu.Unlock()
				// Reset PostForm reading by replacing the body —
				// easiest way is to re-parse downstream. Since the
				// mock ParseForm calls again, this is fine on a
				// fresh url.Values copy.
			}
			next.ServeHTTP(w, r)
		})
	}
	// Build a fresh mux and run wrap around it so we don't recurse.
	freshMux := http.NewServeMux()
	freshMux.HandleFunc("/.well-known/openid-configuration", mock.serveDiscovery)
	freshMux.HandleFunc("/auth/keys", mock.serveJWKS)
	freshMux.HandleFunc("/auth/token", mock.serveToken)
	freshMux.HandleFunc("/auth/authorize", mock.serveAuthorize)
	mock.server.Config.Handler = wrap(freshMux)

	mock.Issue("code-c", appleIDClaims{
		Sub:            "x",
		Email:          "x@example.com",
		EmailVerified:  "true",
		IsPrivateEmail: "false",
	})

	const N = 100 // 1000 is overkill for the assertion; 100 already proves the mutex
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			_, _ = p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "code-c"})
		}()
	}
	wg.Wait()

	observedMu.Lock()
	defer observedMu.Unlock()
	if len(observed) != 1 {
		t.Fatalf("expected exactly one distinct client_secret across %d concurrent calls; got %d distinct values: %v",
			N, len(observed), keys(observed))
	}
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- Factory --------------------------------------------------------

func TestFactory_RegistersInRegistry(t *testing.T) {
	f, ok := account.LookupProviderFactory("apple")
	if !ok || f == nil {
		t.Fatal("apple factory not registered")
	}
}

func TestFactory_DecodeRoundTrip(t *testing.T) {
	mock := newMockApple(t, "com.example.web")

	account.ResetProviderRegistryForTest()
	t.Cleanup(func() {
		account.ResetProviderRegistryForTest()
		account.RegisterProviderFactory("apple", apple.Factory)
	})
	account.RegisterProviderFactory("apple", apple.Factory)

	pem := generateTestPEM(t)
	raw := &fakeRawDecoder{data: map[string]any{
		"enabled":           true,
		"service_id":        "com.example.web",
		"team_id":           "TEAM123456",
		"key_id":            "KEY1234567",
		"private_key":       pem,
		"redirect_url":      "https://app.example.test/cb",
		"client_secret_ttl": 30 * 24 * time.Hour,
		"issuer_url":        mock.issuer,
	}}
	f, _ := account.LookupProviderFactory("apple")
	p, err := f(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "apple" {
		t.Fatalf("name = %q", p.Name())
	}
}

type fakeRawDecoder struct {
	data map[string]any
}

func (r *fakeRawDecoder) Decode(out any) error {
	opts, ok := out.(*apple.Options)
	if !ok {
		return fmt.Errorf("fakeRawDecoder.Decode: want *apple.Options, got %T", out)
	}
	if v, ok := r.data["enabled"].(bool); ok {
		opts.Enabled = v
	}
	if v, ok := r.data["service_id"].(string); ok {
		opts.ServiceID = v
	}
	if v, ok := r.data["team_id"].(string); ok {
		opts.TeamID = v
	}
	if v, ok := r.data["key_id"].(string); ok {
		opts.KeyID = v
	}
	if v, ok := r.data["private_key"].(string); ok {
		opts.PrivateKey = v
	}
	if v, ok := r.data["redirect_url"].(string); ok {
		opts.RedirectURL = v
	}
	if v, ok := r.data["client_secret_ttl"].(time.Duration); ok {
		opts.ClientSecretTTL = v
	}
	if v, ok := r.data["issuer_url"].(string); ok {
		opts.IssuerURL = v
	}
	return nil
}
