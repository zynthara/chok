package auth

import (
	"context"
	"testing"
)

func TestWithPrincipal_RoundTrip(t *testing.T) {
	p := Principal{
		Subject: "usr_abc",
		Name:    "Alice",
		Roles:   []string{"admin"},
		Claims:  map[string]any{"tenant": "acme"},
	}
	ctx := WithPrincipal(context.Background(), p)
	got, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("expected Principal in context")
	}
	if got.Subject != "usr_abc" {
		t.Fatalf("Subject = %q, want usr_abc", got.Subject)
	}
	if got.Name != "Alice" {
		t.Fatalf("Name = %q, want Alice", got.Name)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "admin" {
		t.Fatalf("Roles = %v, want [admin]", got.Roles)
	}
	if got.Claims["tenant"] != "acme" {
		t.Fatalf("Claims[tenant] = %v, want acme", got.Claims["tenant"])
	}
}

func TestPrincipalFrom_Missing(t *testing.T) {
	_, ok := PrincipalFrom(context.Background())
	if ok {
		t.Fatal("expected no Principal in empty context")
	}
}

func TestHashPassword_And_Compare(t *testing.T) {
	hash, err := HashPassword("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "s3cret!" {
		t.Fatal("hash should differ from plaintext")
	}
	if err := ComparePassword(hash, "s3cret!"); err != nil {
		t.Fatalf("correct password should match: %v", err)
	}
	if err := ComparePassword(hash, "wrong"); err == nil {
		t.Fatal("wrong password should not match")
	}
}
