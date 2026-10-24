package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/github"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
)

// mockGitHub is a GitHub Enterprise server in process: the OAuth pair
// under /login/oauth and the REST API under /api/v3.
type mockGitHub struct {
	srv *httptest.Server

	mu          sync.Mutex
	tokens      map[string]string // code → access token
	user        map[string]any
	emails      []map[string]any
	userStatus  int
	emailStatus int
	calls       []string
	auth        []string // the Authorization headers the API saw
}

func newMockGitHub(t *testing.T) *mockGitHub {
	t.Helper()
	m := &mockGitHub{tokens: map[string]string{}, userStatus: 200, emailStatus: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		m.mu.Lock()
		tok, ok := m.tokens[r.PostForm.Get("code")]
		m.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":"bad_verification_code"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "bearer", "scope": "read:user,user:email"})
	})
	serve := func(path string, body func() (int, any)) {
		mux.HandleFunc("GET /api/v3"+path, func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			m.calls = append(m.calls, path)
			m.auth = append(m.auth, r.Header.Get("Authorization"))
			status, v := body()
			m.mu.Unlock()
			if status != http.StatusOK {
				http.Error(w, "stubbed error", status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		})
	}
	serve("/user", func() (int, any) { return m.userStatus, m.user })
	serve("/user/emails", func() (int, any) { return m.emailStatus, m.emails })
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockGitHub) stage(code, token string, user map[string]any, emails ...map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[code], m.user, m.emails = token, user, emails
}

func (m *mockGitHub) called(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Contains(m.calls, path)
}

func provider(t *testing.T, m *mockGitHub) identity.Provider {
	t.Helper()
	p := github.New(identity.ProviderConfig{ClientID: "client-abc", ClientSecret: "shh", RedirectURL: "https://app.example.test/auth/github/callback", Endpoint: m.srv.URL})
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNew_RefusesBadConfig(t *testing.T) {
	ok := identity.ProviderConfig{ClientID: "x", ClientSecret: "x", RedirectURL: "https://a.test/cb"}
	if err := github.New(ok).Check(); err != nil {
		t.Fatalf("the minimal config: %v", err)
	}
	withEnterprise := ok
	withEnterprise.Endpoint = "https://gh.corp"
	if err := github.New(withEnterprise).Check(); err != nil {
		t.Fatalf("an enterprise endpoint: %v", err)
	}
	for name, mutate := range map[string]func(*identity.ProviderConfig){
		"no client_id":        func(pc *identity.ProviderConfig) { pc.ClientID = "" },
		"no client_secret":    func(pc *identity.ProviderConfig) { pc.ClientSecret = "" },
		"no redirect_url":     func(pc *identity.ProviderConfig) { pc.RedirectURL = "" },
		"a relative redirect": func(pc *identity.ProviderConfig) { pc.RedirectURL = "/cb" },
		"a bad endpoint":      func(pc *identity.ProviderConfig) { pc.Endpoint = "not-a-url" },
		"google's domain":     func(pc *identity.ProviderConfig) { pc.HostedDomain = "example.com" },
		"apple's team":        func(pc *identity.ProviderConfig) { pc.TeamID = "1234567890" },
	} {
		pc := ok
		mutate(&pc)
		if err := github.New(pc).Check(); err == nil || !strings.HasPrefix(err.Error(), "github: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProvider_NameAndCapabilities(t *testing.T) {
	p := provider(t, newMockGitHub(t))
	c := p.Capabilities()
	if p.Name() != "github" || c.CallbackMethod != http.MethodGet || c.RequiresNonce || !c.SupportsPKCE || c.RequiresFormPost || c.RedirectURL == "" {
		t.Fatalf("%s %+v", p.Name(), c)
	}
}

func TestBeginAuth_BuildsAuthorizeURL(t *testing.T) {
	m := newMockGitHub(t)
	raw, err := provider(t, m).BeginAuth(context.Background(), &oauth.BeginRequest{State: "state-1", CodeChallenge: "challenge-1"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	if !strings.HasPrefix(raw, m.srv.URL+"/login/oauth/authorize?") || q.Get("client_id") != "client-abc" || q.Get("state") != "state-1" ||
		q.Get("code_challenge") != "challenge-1" || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "user:email") {
		t.Fatalf("authorize URL: %s", raw)
	}
	// github.com itself without an endpoint.
	pub := github.New(identity.ProviderConfig{ClientID: "x", ClientSecret: "x", RedirectURL: "https://a.test/cb"})
	if raw, _ := pub.BeginAuth(context.Background(), &oauth.BeginRequest{State: "s"}); !strings.HasPrefix(raw, "https://github.com/login/oauth/authorize?") {
		t.Fatalf("github.com: %s", raw)
	}
}

func TestCompleteAuth_EmailVisibleOnUser(t *testing.T) {
	m := newMockGitHub(t)
	m.stage("code-1", "tok-1", map[string]any{"id": 12345, "login": "alice", "name": "Alice", "email": "alice@example.com", "avatar_url": "https://gh.example/a.png", "company": "ACME"})
	ext, err := provider(t, m).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-1", CodeVerifier: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if ext.ProviderAccountID != "12345" || ext.Email != "alice@example.com" || !ext.EmailVerified || ext.Name != "Alice" || ext.Picture == "" ||
		ext.Raw["login"] != "alice" || ext.Raw["company"] != "ACME" {
		t.Fatalf("%+v", ext)
	}
	if m.called("/user/emails") {
		t.Fatal("no /user/emails call when /user shows the email")
	}
	if m.auth[0] != "Bearer tok-1" {
		t.Fatalf("the API call carries the token: %q", m.auth[0])
	}
}

func TestCompleteAuth_EmailHiddenFallsBackToEmails(t *testing.T) {
	m := newMockGitHub(t)
	m.stage("code-2", "tok-2", map[string]any{"id": 9001, "login": "bob", "name": "Bob", "email": ""},
		map[string]any{"email": "bob-old@example.com", "primary": false, "verified": true},
		map[string]any{"email": "bob@example.com", "primary": true, "verified": true},
		map[string]any{"email": "bob-noisy@example.com", "primary": false, "verified": false},
	)
	ext, err := provider(t, m).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-2"})
	if err != nil || ext.Email != "bob@example.com" || !ext.EmailVerified || !m.called("/user/emails") {
		t.Fatalf("%+v %v", ext, err)
	}
}

func TestCompleteAuth_AllEmailsUnverified(t *testing.T) {
	m := newMockGitHub(t)
	m.stage("code-3", "tok-3", map[string]any{"id": 4242, "login": "carol"},
		map[string]any{"email": "carol-fresh@example.com", "primary": true, "verified": false},
		map[string]any{"email": "carol-old@example.com", "primary": false, "verified": true},
	)
	ext, err := provider(t, m).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-3"})
	if err != nil || ext.EmailVerified || ext.Email != "" {
		t.Fatalf("no primary verified address: %+v %v", ext, err)
	}
}

func TestCompleteAuth_EnterpriseEndpoint(t *testing.T) {
	m := newMockGitHub(t)
	m.stage("code-ent", "tok-ent", map[string]any{"id": 7, "login": "ent-user", "email": "u@corp.test"})
	if _, err := provider(t, m).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-ent"}); err != nil {
		t.Fatal(err)
	}
	// The mock answers the API only under /api/v3: reaching it proves the
	// enterprise paths.
	if !m.called("/user") {
		t.Fatal("the enterprise API was not called")
	}
}

func TestCompleteAuth_TokenExchangeFailure(t *testing.T) {
	_, err := provider(t, newMockGitHub(t)).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "no-such-code"})
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("an unknown code: %v", err)
	}
}

func TestCompleteAuth_UserAPIFailure(t *testing.T) {
	m := newMockGitHub(t)
	m.stage("code-x", "tok-x", nil)
	m.userStatus = http.StatusUnauthorized
	if _, err := provider(t, m).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-x"}); err == nil || !strings.Contains(err.Error(), "fetch /user") {
		t.Fatalf("a failing /user: %v", err)
	}
	m.userStatus, m.emailStatus = http.StatusOK, http.StatusInternalServerError
	m.stage("code-y", "tok-y", map[string]any{"id": 1})
	if _, err := provider(t, m).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-y"}); err == nil || !strings.Contains(err.Error(), "fetch /user/emails") {
		t.Fatalf("a failing /user/emails: %v", err)
	}
}
