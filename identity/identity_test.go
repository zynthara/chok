package identity

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/internal/conf"
)

type operation struct {
	Summary   string                `json:"summary"`
	Tags      []string              `json:"tags"`
	Security  []map[string][]string `json:"security"`
	Responses map[string]any        `json:"responses"`
}

type spec struct {
	Paths map[string]map[string]*operation `json:"paths"`
}

func TestOpenAPI_AuthRoutes(t *testing.T) {
	idn := newIdentity(t, Config{Passwordless: PasswordlessConfig{Enabled: true}}, Sender(newMockSender()))
	c := serve(t, idn)
	var s spec
	if code := c.GetJSON("/openapi.json", &s); code != http.StatusOK {
		t.Fatalf("openapi = %d", code)
	}
	op := func(method, path string) *operation {
		t.Helper()
		o := s.Paths[path][method]
		if o == nil {
			t.Fatalf("%s %s is not documented: %v", method, path, slices.Collect(maps.Keys(s.Paths)))
		}
		return o
	}
	public := map[[2]string]string{
		{"post", "/auth/register"}:            "Register an account",
		{"post", "/auth/login"}:               "Log in",
		{"post", "/auth/forgot-password"}:     "Mail a password-reset token",
		{"post", "/auth/reset-password"}:      "Reset the password with a token",
		{"post", "/auth/passwordless/start"}:  "Mail a sign-in code",
		{"post", "/auth/passwordless/verify"}: "Sign in with a mailed code",
	}
	for route, summary := range public {
		o := op(route[0], route[1])
		// security: [] — present and empty — is a public route.
		if o.Security == nil || len(o.Security) != 0 || o.Summary != summary || !slices.Equal(o.Tags, []string{"auth"}) {
			t.Errorf("%v: %+v", route, o)
		}
	}
	if _, ok := op("post", "/auth/passwordless/start").Responses["202"]; !ok {
		t.Errorf("start answers 202: %v", op("post", "/auth/passwordless/start").Responses)
	}
	bearer := map[[2]string]string{
		{"get", "/auth/me"}:              "Get the signed-in user",
		{"post", "/auth/refresh-token"}:  "Refresh the access token",
		{"put", "/auth/change-password"}: "Change the password",
	}
	for route, summary := range bearer {
		o := op(route[0], route[1])
		if len(o.Security) != 1 || o.Security[0]["bearerAuth"] == nil || o.Summary != summary {
			t.Errorf("%v must require the bearer token: %+v", route, o)
		}
	}
}

func TestMount_RegisterLoginFlow(t *testing.T) {
	idn := newIdentity(t, Config{})
	// Mounted by app.Use, before the application's routes.
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		app.Use(idn)
		chok.GET(app.Group("/api", idn.Authn()), "/ping", func(context.Context, *chok.None) (string, error) { return "pong", nil })
	})
	tok := register(t, c, "alice@test.com", "password123")
	var pong string
	if code := c.WithBearer(tok).GetJSON("/api/ping", &pong); code != http.StatusOK || pong != "pong" {
		t.Fatalf("ping = %d %q", code, pong)
	}
	// identity reports on /healthz next to the application's components.
	var health struct {
		Components []struct {
			Component string `json:"component"`
			Status    string `json:"status"`
		} `json:"components"`
	}
	c.GetJSON("/healthz", &health)
	if len(health.Components) != 1 || health.Components[0].Component != "identity" || health.Components[0].Status != "up" {
		t.Fatalf("healthz: %+v", health)
	}
}

func TestClose_StopsTheBackgroundWork(t *testing.T) {
	idn := newIdentity(t, Config{LoginRateWindow: 1, LoginRateLimit: 1, Audit: AuditConfig{Enabled: true}})
	idn.Audit().Log(t.Context(), Event{Action: "x"})
	if err := idn.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := idn.Close(t.Context()); err != nil {
		t.Fatalf("a second Close is a no-op: %v", err)
	}
	if !idn.limiter.closed || idn.audit.pending.Load() != 0 {
		t.Fatal("the throttle stopped and the queue drained")
	}
	idn.Audit().Log(t.Context(), Event{Action: "late"}) // dropped, not a panic
}

func TestConfig_SecretsAreMasked(t *testing.T) {
	cfg := Config{
		SigningKey: "super-secret-signing-key-32bytes!!!",
		Providers:  map[string]ProviderConfig{"google": {ClientID: "visible-client-id", ClientSecret: "provider-client-secret-value"}},
	}
	out, err := conf.Dump(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dump := string(out)
	if strings.Contains(dump, "super-secret") || strings.Contains(dump, "provider-client-secret-value") {
		t.Fatalf("a secret leaked:\n%s", dump)
	}
	if !strings.Contains(dump, "visible-client-id") || strings.Count(dump, conf.Masked) != 2 {
		t.Fatalf("only the secrets are masked:\n%s", dump)
	}
}
