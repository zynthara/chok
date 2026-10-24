package identity

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/apptest"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

const testKey = "this-is-a-test-signing-key-32bytes!"

// Tests hash passwords in loops: the minimum cost keeps them fast.
func init() { apptest.PasswordCost.Store(int32(bcrypt.MinCost)) }

// newIdentity builds an Identity on a fresh database of the lane, with
// the test key unless cfg has one; it closes with the test.
func newIdentity(t testing.TB, cfg Config, opts ...Option) *Identity {
	t.Helper()
	return newIdentityOn(t, dbtest.Open(t), cfg, opts...)
}

func newIdentityOn(t testing.TB, d *db.DB, cfg Config, opts ...Option) *Identity {
	t.Helper()
	if cfg.SigningKey == "" {
		cfg.SigningKey = testKey
	}
	idn, err := New(cfg, d, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idn.Close(context.Background()) })
	return idn
}

// serve runs an App with idn and the routes setup adds, in process.
func serve(t testing.TB, idn *Identity, setup ...func(app *chok.App)) *choktest.Client {
	t.Helper()
	return choktest.App(t, chok.Base{}, func(app *chok.App) {
		app.Use(idn)
		for _, f := range setup {
			f(app)
		}
	})
}

// envelope is the error body.
type envelope struct {
	Code      int            `json:"code"`
	Reason    string         `json:"reason"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Metadata  map[string]any `json:"metadata"`
}

type tokenBody struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// register signs up email and returns its token.
func register(t testing.TB, c *choktest.Client, email, password string) string {
	t.Helper()
	var tok tokenBody
	if code := c.PostJSON("/auth/register", map[string]string{"email": email, "password": password}, &tok); code != http.StatusCreated || tok.Token == "" {
		t.Fatalf("register %s = %d %+v", email, code, tok)
	}
	return tok.Token
}

// login returns the status and, on success, the token.
func login(t testing.TB, c *choktest.Client, email, password string) (int, string) {
	t.Helper()
	var tok tokenBody
	code := c.PostJSON("/auth/login", map[string]string{"email": email, "password": password}, &tok)
	return code, tok.Token
}

// userByEmail reads a user straight from the store.
func userByEmail(t testing.TB, idn *Identity, email string) *User {
	t.Helper()
	var u User
	if err := idn.d.Unsafe(t.Context()).Where("email = ?", email).Take(&u).Error; err != nil {
		t.Fatalf("user %s: %v", email, err)
	}
	return &u
}

// mockSender records the reset tokens identity mails.
type mockSender struct {
	mu     sync.Mutex
	to     string
	token  string
	called chan struct{}
}

func newMockSender() *mockSender { return &mockSender{called: make(chan struct{}, 1)} }

func (s *mockSender) Send(_ context.Context, to, token string) error {
	s.mu.Lock()
	s.to, s.token = to, token
	s.mu.Unlock()
	select {
	case s.called <- struct{}{}:
	default:
	}
	return nil
}

func (s *mockSender) wait(d time.Duration) bool {
	select {
	case <-s.called:
		return true
	case <-time.After(d):
		return false
	}
}

func (s *mockSender) last() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.to, s.token
}

// hasKey reports whether the metadata value v is an object with key.
func hasKey(v any, key string) bool {
	m, ok := v.(map[string]any)
	_, has := m[key]
	return ok && has
}

func bearerHeader(token string) []string { return []string{"Authorization", "Bearer " + token} }

func httptestRequest(method, path string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, path, body)
}

func decodeBody(t testing.TB, resp *http.Response, out any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		t.Fatalf("decode the body: %v", err)
	}
}

// jsonRequest builds a request with a JSON body and header pairs.
func jsonRequest(t testing.TB, method, path string, body any, headers ...string) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return req
}
