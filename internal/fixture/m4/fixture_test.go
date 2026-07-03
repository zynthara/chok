package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/store/where"
	"github.com/zynthara/chok/v2/web"
)

// TestM4Fixture_EndToEnd is the M4 acceptance run (SPEC §10 M4 row):
// the full-battery assembly boots, register/login work over real HTTP,
// the audit admin API is fail-closed at every step, and authorization
// decides one-yes-one-no through the real middleware stack.
func TestM4Fixture_EndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "m4.db")
	t.Setenv("M4FIXTURE_CONFIG", "chok.yaml") // test CWD is the package dir
	t.Setenv("M4FIXTURE_DB_SQLITE_PATH", dbPath)
	t.Setenv("M4FIXTURE_DEBUG_ENABLED", "true")
	t.Setenv("M4FIXTURE_HTTP_ADDR", "127.0.0.1:0")

	app := buildApp()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()

	base := waitForHTTP(t, app, runErr)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	do := func(method, path, token string, body string) (int, string) {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, base+path, rdr)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// --- observability: all batteries visible, redis shows `disabled` ---
	code, body := do(http.MethodGet, "/componentz", "", "")
	if code != 200 {
		t.Fatalf("/componentz: %d %s", code, body)
	}
	for _, comp := range []string{"account", "authz", "audit", "scheduler", "cache", "redis", "db", "http"} {
		if !strings.Contains(body, `"`+comp+`"`) {
			t.Fatalf("/componentz must list %q: %s", comp, body)
		}
	}
	if !strings.Contains(body, "disabled") {
		t.Fatalf("/componentz must render the disabled redis (registration-disabled model): %s", body)
	}
	if code, body := do(http.MethodGet, "/healthz", "", ""); code != 200 || !strings.Contains(body, `"status":"up"`) {
		t.Fatalf("/healthz: %d %s", code, body)
	}

	// --- register / login over real HTTP -------------------------------
	code, body = do(http.MethodPost, "/auth/register", "",
		`{"email":"alice@m4.test","password":"password123","name":"Alice"}`)
	if code != 201 {
		t.Fatalf("register: %d %s", code, body)
	}
	code, body = do(http.MethodPost, "/auth/login", "",
		`{"email":"alice@m4.test","password":"password123"}`)
	if code != 200 {
		t.Fatalf("login: %d %s", code, body)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &login); err != nil || login.Token == "" {
		t.Fatalf("login response missing token: %s", body)
	}

	// --- the blessed guard on a business route --------------------------
	if code, _ := do(http.MethodGet, "/api/v1/whoami", "", ""); code != 401 {
		t.Fatalf("anonymous /api/v1/whoami = %d, want 401", code)
	}
	code, body = do(http.MethodGet, "/api/v1/whoami", login.Token, "")
	if code != 200 || !strings.Contains(body, "Alice") {
		t.Fatalf("authenticated /api/v1/whoami: %d %s", code, body)
	}

	// --- audit admin API: fail-closed, then authorization one-no-one-yes -
	if code, _ := do(http.MethodGet, "/audit/logs", "", ""); code != 401 {
		t.Fatalf("anonymous /audit/logs = %d, want 401 (fail-closed)", code)
	}
	// Authenticated but ungranted: the authorizer says no.
	if code, _ := do(http.MethodGet, "/audit/logs", login.Token, ""); code != 403 {
		t.Fatalf("ungranted /audit/logs = %d, want 403 (deny branch)", code)
	}

	// Grant alice audit read through the policy service, then the same
	// token passes (allow branch) — one-no-one-yes through real HTTP.
	k := app.Kernel()
	ac, _ := chok.Get[*account.Component](k, "account")
	alice, err := ac.Service().Store().Get(context.Background(),
		store.Where(where.WithFilter("email", "alice@m4.test")))
	if err != nil {
		t.Fatal(err)
	}
	azc, _ := chok.Get[*authz.Component](k, "authz")
	if err := azc.Service().GrantUser(context.Background(), alice.RID, "audit", "read"); err != nil {
		t.Fatal(err)
	}
	code, body = do(http.MethodGet, "/audit/logs", login.Token, "")
	if code != 200 {
		t.Fatalf("granted /audit/logs = %d %s, want 200", code, body)
	}
	// 7.E evidence inside the payload: the synchronous switch-on probe
	// is durable by construction; the audited GrantUser mutation rides
	// the async sink, so poll past its batch-flush interval.
	if !strings.Contains(body, "authz.audit.enabled") {
		t.Fatalf("audit payload must contain the 7.E switch-on probe entry: %s", body)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, b := do(http.MethodGet, "/audit/logs", login.Token, "")
		return strings.Contains(b, "authz.GrantUser")
	}, "audited GrantUser mutation never reached the queryable sink")

	// Bootstrap-seeded admin authorizes on anything (the yes side of
	// the policy engine itself).
	allowed, err := azc.Authorizer().Authorize(context.Background(), "usr_bootstrap_admin", "anything", "read")
	if err != nil || !allowed {
		t.Fatalf("bootstrap admin should authorize: allowed=%v err=%v", allowed, err)
	}
	denied, _ := azc.Authorizer().Authorize(context.Background(), alice.RID, "posts", "delete")
	if denied {
		t.Fatal("ungranted tuple must deny (fail-closed policy engine)")
	}

	// --- clean stop ------------------------------------------------------
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("app did not stop")
	}
}

func waitFor(t *testing.T, budget time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(msg)
}

// waitForHTTP blocks until the web component reports its bound
// address (Serve running), failing fast if Run exits first.
func waitForHTTP(t *testing.T, app *chok.App, runErr <-chan error) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-runErr:
			t.Fatalf("app exited during startup: %v", err)
		case <-deadline:
			t.Fatal("web component never became reachable")
		case <-time.After(25 * time.Millisecond):
		}
		if k := app.Kernel(); k != nil {
			if webc, ok := chok.Get[*web.Component](k, "http"); ok {
				if addr := webc.BoundAddr(); addr != "" {
					return "http://" + addr
				}
			}
		}
	}
}
