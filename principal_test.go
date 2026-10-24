package chok

import (
	"context"
	"testing"
)

func TestPrincipal_MissingFromAPlainContext(t *testing.T) {
	if p, ok := PrincipalFrom(context.Background()); ok || p.UserID != "" {
		t.Fatalf("a context without a principal: %+v %v", p, ok)
	}
}

func TestPrincipal_RoundTripAndRoles(t *testing.T) {
	in := Principal{UserID: "usr_1", Email: "a@test.com", Roles: []string{"admin", "editor"}, Extra: map[string]string{"name": "A"}}
	p, ok := PrincipalFrom(WithPrincipal(context.Background(), in))
	if !ok || p.UserID != "usr_1" || p.Email != "a@test.com" || p.Extra["name"] != "A" {
		t.Fatalf("round trip: %+v %v", p, ok)
	}
	if !p.HasRole("admin") || !p.HasRole("editor") || p.HasRole("viewer") || (Principal{}).HasRole("") {
		t.Fatal("HasRole matches the roles held and nothing else")
	}
}
