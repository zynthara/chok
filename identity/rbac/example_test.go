package rbac_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/rbac"
)

// Access control is one more value: built on identity and the database,
// registered with the App, applied per route.
func ExampleNew() {
	d := db.MustOpen(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}})
	auth := identity.MustNew(identity.Config{SigningKey: strings.Repeat("k", 32)}, d)
	rb, err := rbac.New(auth, d)
	if err != nil {
		fmt.Println(err)
		return
	}
	app := chok.New(chok.MustLoad[chok.Base]("example", chok.Args(nil)), chok.Logger(slog.New(slog.DiscardHandler)))
	app.Use(d, auth, rb)
	api := app.Group("/api", auth.Authn())
	chok.DELETE(api, "/posts/{id}", func(context.Context, *struct {
		ID string `path:"id"`
	}) (chok.None, error) {
		return chok.None{}, nil
	}, chok.Use(rb.Require("post", "delete")))
	fmt.Println(rb.Name())
	// Output: rbac
}

// Roles carry permissions. A user's global roles are identity's — admin
// passes everything — and casbin binds the roles within a domain.
func ExamplePolicyService() {
	d := db.MustOpen(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}})
	auth := identity.MustNew(identity.Config{SigningKey: strings.Repeat("k", 32)}, d)
	rb, err := rbac.New(auth, d)
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx := context.Background()
	alice, _ := auth.Users().Create(ctx, "alice@example.com", "password123", "")
	root, _ := auth.Users().Create(ctx, "root@example.com", "password123", "")
	_ = auth.Users().SetRoles(ctx, root.RID, []string{"admin"})

	p := rb.Policy()
	_ = p.GrantRoleInDomain(ctx, "member", "post", "read", "ws_1")
	_ = p.AddUserToRoleInDomain(ctx, alice.RID, "member", "ws_1")

	inWS1, _ := p.HasPermissionInDomain(ctx, alice.RID, "post", "read", "ws_1")
	inWS2, _ := p.HasPermissionInDomain(ctx, alice.RID, "post", "read", "ws_2")
	admin, _ := p.HasPermissionInDomain(ctx, root.RID, "post", "delete", "ws_2")
	fmt.Println(inWS1, inWS2, admin)
	// Output: true false true
}
