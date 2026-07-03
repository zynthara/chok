package facebook_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/account/providers/facebook"
)

// mockGraph stands in for Facebook's Graph API + OAuth endpoint pair.
// Both the dialog/oauth (authorize) + oauth/access_token (token
// exchange) + /vXX.X/me (Graph) paths are versioned, so the mock
// honours arbitrary v-prefixed segments.
type mockGraph struct {
	server *httptest.Server

	codeToToken map[string]string // authorization code → access token
	user        fbUser
	userStatus  int

	apiCalls []string
}

type fbUser struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	Picture struct {
		Data struct {
			URL          string `json:"url"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			IsSilhouette bool   `json:"is_silhouette"`
		} `json:"data"`
	} `json:"picture"`
}

func newMockGraph(t *testing.T) *mockGraph {
	t.Helper()
	mock := &mockGraph{
		codeToToken: map[string]string{},
		userStatus:  200,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", mock.serveAny)
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *mockGraph) serveAny(w http.ResponseWriter, r *http.Request) {
	m.apiCalls = append(m.apiCalls, r.URL.Path)
	switch {
	case strings.HasSuffix(r.URL.Path, "/dialog/oauth"):
		w.WriteHeader(200)
	case strings.HasSuffix(r.URL.Path, "/oauth/access_token"):
		m.serveToken(w, r)
	case strings.HasSuffix(r.URL.Path, "/me"):
		m.serveMe(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *mockGraph) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	code := r.PostForm.Get("code")
	tok, ok := m.codeToToken[code]
	if !ok {
		http.Error(w, "unknown code", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": tok,
		"token_type":   "bearer",
		"expires_in":   3600,
	})
}

func (m *mockGraph) serveMe(w http.ResponseWriter, r *http.Request) {
	if m.userStatus != 200 {
		http.Error(w, "stubbed error", m.userStatus)
		return
	}
	// Graph API returns only requested fields. We pass through
	// the full staged user payload — tests verifying field selection
	// will inspect mockGraph.lastMeQuery if they care.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m.user)
}

// stage configures the mock with a (code, token) pair and the /me
// payload to return.
func (m *mockGraph) stage(code, token string, u fbUser) {
	m.codeToToken[code] = token
	m.user = u
}

// --- helpers --------------------------------------------------------

func newProvider(t *testing.T, mock *mockGraph, opts func(*facebook.Options)) account.AuthProvider {
	t.Helper()
	o := facebook.Options{
		Enabled:      true,
		ClientID:     "app-abc",
		ClientSecret: "shh",
		RedirectURL:  "https://app.example.test/auth/facebook/callback",
		APIVersion:   "v18.0",
	}
	if opts != nil {
		opts(&o)
	}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := facebook.NewWithAPIBase(o, mock.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --- tests ----------------------------------------------------------

func TestOptions_Validate(t *testing.T) {
	cases := []struct {
		name string
		opts facebook.Options
		ok   bool
	}{
		{"disabled bypasses", facebook.Options{Enabled: false}, true},
		{"missing client_id", facebook.Options{Enabled: true, ClientSecret: "x", RedirectURL: "https://a/cb"}, false},
		{"missing client_secret", facebook.Options{Enabled: true, ClientID: "x", RedirectURL: "https://a/cb"}, false},
		{"missing redirect_url", facebook.Options{Enabled: true, ClientID: "x", ClientSecret: "x"}, false},
		{"relative redirect_url", facebook.Options{Enabled: true, ClientID: "x", ClientSecret: "x", RedirectURL: "/cb"}, false},
		{"ok minimal", facebook.Options{Enabled: true, ClientID: "x", ClientSecret: "x", RedirectURL: "https://a/cb"}, true},
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
	mock := newMockGraph(t)
	p := newProvider(t, mock, nil)

	resp, err := p.BeginAuth(context.Background(), &account.BeginRequest{
		State:         "state-1",
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
	if q.Get("client_id") != "app-abc" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("state") != "state-1" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("code_challenge") != "challenge-1" {
		t.Errorf("code_challenge = %q", q.Get("code_challenge"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	if scope := q.Get("scope"); !strings.Contains(scope, "email") || !strings.Contains(scope, "public_profile") {
		t.Errorf("default scope missing entries: %q", scope)
	}
}

func TestCompleteAuth_HappyPath(t *testing.T) {
	mock := newMockGraph(t)
	p := newProvider(t, mock, nil)

	staged := fbUser{ID: "fb-9999", Name: "Alice", Email: "alice@example.com"}
	staged.Picture.Data.URL = "https://lookaside.example/alice.jpg"
	staged.Picture.Data.Width = 200
	staged.Picture.Data.Height = 200
	mock.stage("code-1", "tok-1", staged)

	pi, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "code-1"})
	if err != nil {
		t.Fatal(err)
	}
	if pi.ProviderAccountID != "fb-9999" {
		t.Errorf("account_id = %q", pi.ProviderAccountID)
	}
	if pi.Email != "alice@example.com" || !pi.EmailVerified {
		t.Errorf("email/verified = %q/%v", pi.Email, pi.EmailVerified)
	}
	if pi.Picture != "https://lookaside.example/alice.jpg" {
		t.Errorf("picture URL not extracted from nested struct: %q", pi.Picture)
	}
	if v, _ := pi.Raw["api_version"].(string); v != "v18.0" {
		t.Errorf("raw.api_version = %v", pi.Raw["api_version"])
	}
}

// TestCompleteAuth_NoEmailScope covers the SPEC §7 case where the user
// declined the email scope at consent: Graph API returns email="".
// We must surface EmailVerified=false so account.ResolveOAuthIdentity
// rejects the create-User path with OAUTH_EMAIL_REQUIRED.
func TestCompleteAuth_NoEmailScope(t *testing.T) {
	mock := newMockGraph(t)
	p := newProvider(t, mock, nil)

	mock.stage("code-2", "tok-2", fbUser{
		ID:    "fb-no-email",
		Name:  "Bob",
		Email: "", // scope declined
	})
	pi, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "code-2"})
	if err != nil {
		t.Fatal(err)
	}
	if pi.EmailVerified {
		t.Error("EmailVerified must be false when Facebook returns no email")
	}
	if pi.Email != "" {
		t.Errorf("expected empty email, got %q", pi.Email)
	}
}

// TestCompleteAuth_DefaultAPIVersion covers SPEC §7 "APIVersion default
// applies when omitted". We construct without explicitly setting
// APIVersion and assert the /me URL the mock sees uses the default
// v18.0 prefix.
func TestCompleteAuth_DefaultAPIVersion(t *testing.T) {
	mock := newMockGraph(t)
	p := newProvider(t, mock, func(o *facebook.Options) {
		o.APIVersion = "" // force default
	})

	mock.stage("code-3", "tok-3", fbUser{ID: "fb-x", Email: "x@example.com"})
	if _, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "code-3"}); err != nil {
		t.Fatal(err)
	}

	sawV18Me := false
	for _, path := range mock.apiCalls {
		if strings.Contains(path, "/v18.0/me") {
			sawV18Me = true
			break
		}
	}
	if !sawV18Me {
		t.Fatalf("expected /v18.0/me hit; got %v", mock.apiCalls)
	}
}

func TestCompleteAuth_TokenExchangeFailure(t *testing.T) {
	mock := newMockGraph(t)
	p := newProvider(t, mock, nil)

	_, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "no-such-code"})
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("expected wrapped token exchange error, got %v", err)
	}
}

func TestCompleteAuth_MeAPIFailure(t *testing.T) {
	mock := newMockGraph(t)
	p := newProvider(t, mock, nil)

	mock.codeToToken["c"] = "tok"
	mock.userStatus = 401
	_, err := p.CompleteAuth(context.Background(), &account.CompleteRequest{Code: "c"})
	if err == nil || !strings.Contains(err.Error(), "fetch /me") {
		t.Fatalf("expected wrapped /me fetch error, got %v", err)
	}
}

func TestFactory_RegistersInRegistry(t *testing.T) {
	f, ok := account.LookupProviderFactory("facebook")
	if !ok || f == nil {
		t.Fatal("facebook factory not registered")
	}
}

func TestFactory_DecodeRoundTrip(t *testing.T) {
	account.ResetProviderRegistryForTest()
	t.Cleanup(func() {
		account.ResetProviderRegistryForTest()
		account.RegisterProviderFactory("facebook", facebook.Factory)
	})
	account.RegisterProviderFactory("facebook", facebook.Factory)

	raw := &fakeRawDecoder{data: map[string]any{
		"enabled":       true,
		"client_id":     "app-abc",
		"client_secret": "shh",
		"redirect_url":  "https://app.example.test/cb",
		"api_version":   "v18.0",
	}}
	f, _ := account.LookupProviderFactory("facebook")
	p, err := f(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "facebook" {
		t.Fatalf("name = %q", p.Name())
	}
}

type fakeRawDecoder struct {
	data map[string]any
}

func (r *fakeRawDecoder) Decode(out any) error {
	opts, ok := out.(*facebook.Options)
	if !ok {
		return fmt.Errorf("fakeRawDecoder.Decode: want *facebook.Options, got %T", out)
	}
	if v, ok := r.data["enabled"].(bool); ok {
		opts.Enabled = v
	}
	if v, ok := r.data["client_id"].(string); ok {
		opts.ClientID = v
	}
	if v, ok := r.data["client_secret"].(string); ok {
		opts.ClientSecret = v
	}
	if v, ok := r.data["redirect_url"].(string); ok {
		opts.RedirectURL = v
	}
	if v, ok := r.data["api_version"].(string); ok {
		opts.APIVersion = v
	}
	return nil
}
