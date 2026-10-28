package identity

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
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
	"github.com/zynthara/chok/v3/internal/conf"
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

// mockSender records the mails identity hands it, and answers err. Its
// send method is the Sender.
type mockSender struct {
	mu     sync.Mutex
	mails  []Mail
	calls  []sendCall
	err    error
	called chan struct{}
}

// sendCall is one call of the Sender, as identity made it.
type sendCall struct {
	to, template string
	data         any
}

func newMockSender() *mockSender { return &mockSender{called: make(chan struct{}, 64)} }

func (s *mockSender) send(_ context.Context, to, template string, data any) error {
	m, _ := data.(Mail)
	s.mu.Lock()
	s.mails = append(s.mails, m)
	s.calls = append(s.calls, sendCall{to, template, data})
	err := s.err
	s.mu.Unlock()
	select {
	case s.called <- struct{}{}:
	default:
	}
	return err
}

// wait reports whether a mail came within d.
func (s *mockSender) wait(d time.Duration) bool {
	select {
	case <-s.called:
		return true
	case <-time.After(d):
		return false
	}
}

// last is the latest mail.
func (s *mockSender) last() Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mails) == 0 {
		return Mail{}
	}
	return s.mails[len(s.mails)-1]
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

// syncBuffer is a log destination tests read while identity writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// loggedApp is an App with idn, served in process through its handler,
// whose log the test reads.
func loggedApp(t testing.TB, idn *Identity) (http.Handler, *syncBuffer) {
	t.Helper()
	var base chok.Base
	if err := conf.Defaults(&base); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	app := chok.New(base, chok.Logger(slog.New(slog.NewTextHandler(logs, nil))))
	app.Use(idn)
	return app.Handler(), logs
}

// serveJSON sends a JSON request to h and returns the recorded response.
func serveJSON(t testing.TB, h http.Handler, method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jsonRequest(t, method, path, body, headers...))
	return rec
}

// eventually fails the test unless cond holds within five seconds.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
