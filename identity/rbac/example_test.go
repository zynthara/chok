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

// Roles carry permissions; users carry roles, globally or in a domain.
func ExamplePolicyService() {
	d := db.MustOpen(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}})
	rb, err := rbac.New(identity.MustNew(identity.Config{SigningKey: strings.Repeat("k", 32)}, d), d)
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx := context.Background()
	p := rb.Policy()
	_ = p.BootstrapAdmin(ctx, "usr_root")
	_ = p.GrantRoleInDomain(ctx, "member", "post", "read", "ws_1")
	_ = p.AddUserToRoleInDomain(ctx, "usr_alice", "member", "ws_1")

	inWS1, _ := p.HasPermissionInDomain(ctx, "usr_alice", "post", "read", "ws_1")
	inWS2, _ := p.HasPermissionInDomain(ctx, "usr_alice", "post", "read", "ws_2")
	root, _ := p.HasPermissionInDomain(ctx, "usr_root", "post", "delete", "ws_2")
	fmt.Println(inWS1, inWS2, root)
	// Output: true false true
}
