package conf

import (
	"fmt"
	"strings"
	"testing"
)

type redactInner struct {
	Password string `mapstructure:"password" sensitive:"true"`
	Host     string `mapstructure:"host"`
}

type redactOuter struct {
	Name   string            `mapstructure:"name"`
	Secret string            `mapstructure:"secret_key" sensitive:"true"`
	Inner  redactInner       `mapstructure:"inner"`
	Ptr    *redactInner      `mapstructure:"ptr"`
	Raw    map[string]any    `mapstructure:"raw"`
	Many   []redactInner     `mapstructure:"many"`
	Tags   map[string]string `mapstructure:"tags"`
}

func TestRedact_MasksTaggedFieldsRecursively(t *testing.T) {
	in := redactOuter{
		Name:   "app",
		Secret: "s3cr3t",
		Inner:  redactInner{Password: "pw1", Host: "db.local"},
		Ptr:    &redactInner{Password: "pw2", Host: "replica"},
		Raw:    map[string]any{"client_secret": "cs", "plain": "keep", "nested": map[string]any{"api_key": "ak"}},
		Many:   []redactInner{{Password: "pw3", Host: "h3"}},
		Tags:   map[string]string{"password": "tag-pw", "env": "prod"},
	}

	out, ok := Redact(&in).(redactOuter)
	if !ok {
		t.Fatalf("Redact returned %T, want redactOuter", Redact(&in))
	}

	if out.Secret != "***" || out.Inner.Password != "***" || out.Ptr.Password != "***" || out.Many[0].Password != "***" {
		t.Fatalf("tagged fields not masked: %+v", out)
	}
	if out.Name != "app" || out.Inner.Host != "db.local" || out.Ptr.Host != "replica" || out.Many[0].Host != "h3" {
		t.Fatalf("non-sensitive fields lost: %+v", out)
	}
	if out.Raw["client_secret"] != "***" || out.Raw["plain"] != "keep" {
		t.Fatalf("map heuristic wrong: %v", out.Raw)
	}
	if nested := out.Raw["nested"].(map[string]any); nested["api_key"] != "***" {
		t.Fatalf("nested map key not masked: %v", nested)
	}
	if out.Tags["password"] != "***" || out.Tags["env"] != "prod" {
		t.Fatalf("string map heuristic wrong: %v", out.Tags)
	}

	// Original untouched (maps are reference types — the deep copy must
	// protect the caller's data).
	if in.Secret != "s3cr3t" || in.Inner.Password != "pw1" || in.Ptr.Password != "pw2" {
		t.Fatalf("Redact mutated input struct: %+v", in)
	}
	if in.Raw["client_secret"] != "cs" || in.Tags["password"] != "tag-pw" {
		t.Fatalf("Redact mutated input maps: %v %v", in.Raw, in.Tags)
	}
}

func TestRedact_EmptySensitiveStaysEmpty(t *testing.T) {
	out := Redact(redactInner{Host: "h"}).(redactInner)
	if out.Password != "" {
		t.Fatalf("empty secret should stay empty (absence is information), got %q", out.Password)
	}
}

func TestRedact_NonStructPassthrough(t *testing.T) {
	if got := Redact("plain"); got != "plain" {
		t.Fatalf("non-struct input should pass through, got %v", got)
	}
	var nilPtr *redactInner
	if got := Redact(nilPtr); got != any(nilPtr) {
		t.Fatalf("nil pointer should pass through, got %v", got)
	}
}

// goStringOptions mirrors the documented module-side GoString pattern:
// a method-less twin type formatted after Redact.
type goStringOptions struct {
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password" sensitive:"true"`
}

type goStringOptionsRaw goStringOptions

func (o goStringOptions) GoString() string {
	return fmt.Sprintf("%#v", Redact(goStringOptionsRaw(o)))
}

func TestRedact_GoStringTwinPattern(t *testing.T) {
	o := goStringOptions{Username: "root", Password: "hunter2"}
	got := fmt.Sprintf("%#v", o)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("GoString leaked the password: %s", got)
	}
	if !strings.Contains(got, "root") || !strings.Contains(got, "***") {
		t.Fatalf("GoString lost non-sensitive data or mask: %s", got)
	}
	// %+v / %v also route through GoStringer only for %#v; ensure %v of
	// the value type does not panic or recurse (fmt uses String/GoString
	// rules — plain %v prints raw fields, acceptable for non-log paths,
	// documented; the guarded verb is %#v).
	_ = fmt.Sprintf("%v", o)
}

func TestRedactedSettings_MasksByPathAndHeuristic(t *testing.T) {
	l := NewLoader("testapp", "TESTAPP")
	if err := l.Register("db", redactOuter{}); err != nil {
		t.Fatal(err)
	}
	snap := newSnapshot(map[string]any{
		"db": map[string]any{
			"name":       "app",
			"secret_key": "s3cr3t",
			"inner":      map[string]any{"password": "pw", "host": "h"},
			"raw":        map[string]any{"api_key": "ak", "plain": "keep"},
		},
		"unregistered": map[string]any{
			"password": "heuristic-pw",
			"note":     "visible",
			"empty":    "",
		},
	}, l)

	out := snap.RedactedSettings()

	dbSec := out["db"].(map[string]any)
	if dbSec["secret_key"] != "***" {
		t.Fatalf("tagged path db.secret_key not masked: %v", dbSec)
	}
	if inner := dbSec["inner"].(map[string]any); inner["password"] != "***" || inner["host"] != "h" {
		t.Fatalf("nested tagged path wrong: %v", inner)
	}
	if raw := dbSec["raw"].(map[string]any); raw["api_key"] != "***" || raw["plain"] != "keep" {
		t.Fatalf("heuristic inside registered section wrong: %v", raw)
	}
	unreg := out["unregistered"].(map[string]any)
	if unreg["password"] != "***" || unreg["note"] != "visible" {
		t.Fatalf("heuristic on unregistered section wrong: %v", unreg)
	}
	if unreg["empty"] != "" {
		t.Fatalf("empty value at heuristic key should stay empty, got %v", unreg["empty"])
	}

	// The snapshot's own tree must be untouched.
	orig := snap.settings["db"].(map[string]any)
	if orig["secret_key"] != "s3cr3t" {
		t.Fatalf("RedactedSettings mutated the frozen tree: %v", orig)
	}
}

func TestIsSensitiveKey_CoversDSN(t *testing.T) {
	for _, k := range []string{"dsn", "DSN", "pg_dsn", "password", "client_secret", "signing_key"} {
		if !isSensitiveKey(k) {
			t.Fatalf("key %q should be sensitive", k)
		}
	}
	for _, k := range []string{"host", "port", "database", "username"} {
		if isSensitiveKey(k) {
			t.Fatalf("key %q should NOT be sensitive", k)
		}
	}
}
