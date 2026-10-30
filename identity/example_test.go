package identity_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/identity"
)

// The battery is a value: build it on the database, register it with
// the App, and guard the application's routes with Authn.
func Example() {
	d := db.MustOpen(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}})
	auth := identity.MustNew(identity.Config{SigningKey: strings.Repeat("k", 32)}, d)

	app := chok.New(chok.MustLoad[chok.Base]("example", chok.Args(nil)), chok.Logger(slog.New(slog.DiscardHandler)))
	app.Use(d, auth)
	chok.GET(app.Group("/api", auth.Authn()), "/me", func(ctx context.Context, _ *chok.None) (string, error) {
		p, _ := chok.PrincipalFrom(ctx)
		return p.Email, nil
	})

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	fmt.Println(rec.Code)
	// Output: 401
}

// The administration of accounts, for a deployment that provisions them.
func ExampleUserService() {
	d := db.MustOpen(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}})
	auth := identity.MustNew(identity.Config{SigningKey: strings.Repeat("k", 32), DisableRegister: true}, d)
	ctx := context.Background()

	u, err := auth.Users().Create(ctx, "ops@example.com", "Initial-pass1", "Ops")
	if err != nil {
		fmt.Println(err)
		return
	}
	// Each change revokes the user's tokens: the next login carries it.
	_ = auth.Users().SetRoles(ctx, u.RID, []string{"admin"})
	fmt.Println(strings.HasPrefix(u.RID, "usr_"))
	// Output: true
}

// An audit event names what happened; Log fills in who did it from the
// request.
func ExampleAudit_Log() {
	var auth *identity.Identity // from identity.New, with auth.audit.enabled
	publish := func(ctx context.Context, postID string) {
		auth.Audit().Log(ctx, identity.Event{
			Action: "post.publish", Resource: "post", ResourceID: postID,
			Before: map[string]string{"status": "draft"}, After: map[string]string{"status": "published"},
		})
	}
	_ = publish
}
