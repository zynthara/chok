package facebook_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/facebook"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
)

// mockGraph is the Graph API in process, under any version prefix.
type mockGraph struct {
	srv *httptest.Server

	mu         sync.Mutex
	tokens     map[string]string
	user       map[string]any
	userStatus int
	paths      []string
}

func newMockGraph(t *testing.T) *mockGraph {
	t.Helper()
	m := &mockGraph{tokens: map[string]string{}, userStatus: http.StatusOK}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.paths = append(m.paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth/access_token"):
			_ = r.ParseForm()
			tok, ok := m.tokens[r.PostForm.Get("code")]
			if !ok {
				http.Error(w, `{"error":{"message":"invalid code"}}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "bearer", "expires_in": 3600})
		case strings.HasSuffix(r.URL.Path, "/me"):
			if m.userStatus != http.StatusOK {
				http.Error(w, "stubbed error", m.userStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(m.user)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockGraph) stage(code, token string, u map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[code], m.user = token, u
}

func provider(t *testing.T, endpoint string) identity.Provider {
	t.Helper()
	p := facebook.New(identity.ProviderConfig{ClientID: "app-abc", ClientSecret: "shh", RedirectURL: "https://app.example.test/auth/facebook/callback", Endpoint: endpoint})
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNew_RefusesBadConfig(t *testing.T) {
	ok := identity.ProviderConfig{ClientID: "x", ClientSecret: "x", RedirectURL: "https://a.test/cb", Endpoint: "https://graph.facebook.com/v21.0"}
	if err := facebook.New(ok).Check(); err != nil {
		t.Fatalf("the minimal config: %v", err)
	}
	for name, mutate := range map[string]func(*identity.ProviderConfig){
		"no client_id":                  func(pc *identity.ProviderConfig) { pc.ClientID = "" },
		"no client_secret":              func(pc *identity.ProviderConfig) { pc.ClientSecret = "" },
		"no redirect_url":               func(pc *identity.ProviderConfig) { pc.RedirectURL = "" },
		"a relative redirect":           func(pc *identity.ProviderConfig) { pc.RedirectURL = "/cb" },
		"no endpoint":                   func(pc *identity.ProviderConfig) { pc.Endpoint = "" },
		"a bad endpoint":                func(pc *identity.ProviderConfig) { pc.Endpoint = "graph" },
		"an unversioned Graph endpoint": func(pc *identity.ProviderConfig) { pc.Endpoint = "https://graph.facebook.com" },
		"a Graph path, not a version":   func(pc *identity.ProviderConfig) { pc.Endpoint = "https://graph.facebook.com/me" },
		"apple's key":                   func(pc *identity.ProviderConfig) { pc.KeyID = "1234567890" },
	} {
		pc := ok
		mutate(&pc)
		if err := facebook.New(pc).Check(); err == nil || !strings.HasPrefix(err.Error(), "facebook: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProvider_NameAndCapabilities(t *testing.T) {
	p := provider(t, "https://graph.facebook.com/v21.0")
	c := p.Capabilities()
	if p.Name() != "facebook" || c.CallbackMethod != http.MethodGet || c.RequiresNonce || !c.SupportsPKCE || c.RedirectURL == "" {
		t.Fatalf("%s %+v", p.Name(), c)
	}
}

func TestBeginAuth_BuildsAuthorizeURL(t *testing.T) {
	m := newMockGraph(t)
	raw, err := provider(t, m.srv.URL+"/v21.0").BeginAuth(context.Background(), &oauth.BeginRequest{State: "state-1", CodeChallenge: "challenge-1"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	if u.Path != "/v21.0/dialog/oauth" || q.Get("client_id") != "app-abc" || q.Get("state") != "state-1" || q.Get("code_challenge") != "challenge-1" ||
		q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "email") || !strings.Contains(q.Get("scope"), "public_profile") {
		t.Fatalf("dialog URL: %s", raw)
	}
	// The real Graph API's dialog lives on www.facebook.com, same version.
	raw, _ = provider(t, "https://graph.facebook.com/v21.0").BeginAuth(context.Background(), &oauth.BeginRequest{State: "s"})
	if !strings.HasPrefix(raw, "https://www.facebook.com/v21.0/dialog/oauth?") {
		t.Fatalf("the real Graph dialog: %s", raw)
	}
}

func TestCompleteAuth_HappyPath(t *testing.T) {
	m := newMockGraph(t)
	m.stage("code-1", "tok-1", map[string]any{"id": "fb-9999", "name": "Alice", "email": "alice@example.com",
		"picture": map[string]any{"data": map[string]any{"url": "https://lookaside.example/alice.jpg", "width": 200, "height": 200}}})
	ext, err := provider(t, m.srv.URL+"/v18.0").CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-1", CodeVerifier: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if ext.ProviderAccountID != "fb-9999" || ext.Email != "alice@example.com" || !ext.EmailVerified || ext.Picture != "https://lookaside.example/alice.jpg" ||
		ext.Raw["graph"] != m.srv.URL+"/v18.0" {
		t.Fatalf("%+v", ext)
	}
}

func TestCompleteAuth_NoEmailScope(t *testing.T) {
	m := newMockGraph(t)
	m.stage("code-2", "tok-2", map[string]any{"id": "fb-no-email", "name": "Bob"})
	ext, err := provider(t, m.srv.URL+"/v18.0").CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-2"})
	if err != nil || ext.EmailVerified || ext.Email != "" {
		t.Fatalf("a declined email scope: %+v %v", ext, err)
	}
}

func TestCompleteAuth_EndpointCarriesTheVersion(t *testing.T) {
	m := newMockGraph(t)
	m.stage("code-3", "tok-3", map[string]any{"id": "fb-x", "email": "x@example.com"})
	if _, err := provider(t, m.srv.URL+"/v21.0").CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-3"}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.paths) != 2 || m.paths[0] != "/v21.0/oauth/access_token" || m.paths[1] != "/v21.0/me" {
		t.Fatalf("paths: %v", m.paths)
	}
}

func TestCompleteAuth_TokenExchangeFailure(t *testing.T) {
	m := newMockGraph(t)
	if _, err := provider(t, m.srv.URL+"/v18.0").CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "no-such-code"}); err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("an unknown code: %v", err)
	}
}

func TestCompleteAuth_MeAPIFailure(t *testing.T) {
	m := newMockGraph(t)
	m.stage("c", "tok", nil)
	m.userStatus = http.StatusUnauthorized
	if _, err := provider(t, m.srv.URL+"/v18.0").CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "c"}); err == nil || !strings.Contains(err.Error(), "fetch /me") {
		t.Fatalf("a failing /me: %v", err)
	}
}
