package identity_test

import (
	"os/exec"
	"strings"
	"testing"
)

// goListDeps lists every package pkgs link, standard library included.
func goListDeps(t *testing.T, pkgs ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list", "-deps"}, pkgs...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %v: %v\n%s", pkgs, err, out)
	}
	return strings.Fields(string(out))
}

func linksCasbin(deps []string) bool {
	for _, d := range deps {
		if strings.HasPrefix(d, "github.com/casbin/") {
			return true
		}
	}
	return false
}

// TestDeps_CasbinOnlyWithRBAC: casbin is identity/rbac's alone. A binary
// built from identity and its providers — examples/blog among them —
// links none of it; one that imports identity/rbac does.
func TestDeps_CasbinOnlyWithRBAC(t *testing.T) {
	const mod = "github.com/zynthara/chok"
	without := goListDeps(t, mod+"/identity", mod+"/identity/google", mod+"/identity/github", mod+"/identity/apple", mod+"/identity/facebook", mod+"/examples/blog")
	if linksCasbin(without) {
		t.Fatal("identity without rbac links casbin")
	}
	for _, d := range without {
		if strings.HasPrefix(d, "github.com/redis/") {
			t.Fatalf("identity without rbac links %s", d)
		}
	}
	if !linksCasbin(goListDeps(t, mod+"/identity/rbac")) {
		t.Fatal("the check is blind: identity/rbac must link casbin")
	}
}

// TestDeps_GoRedisLogsBridgedWhereverLinked: every chok package that links
// go-redis also links internal/redislog, so go-redis's own log lines reach
// slog in any binary built from them.
func TestDeps_GoRedisLogsBridgedWhereverLinked(t *testing.T) {
	const mod = "github.com/zynthara/chok"
	for _, pkg := range []string{mod + "/identity/rbac", mod + "/cache"} {
		deps := goListDeps(t, pkg)
		var redis, bridge bool
		for _, d := range deps {
			redis = redis || strings.HasPrefix(d, "github.com/redis/go-redis/")
			bridge = bridge || d == mod+"/internal/redislog"
		}
		if !redis {
			t.Fatalf("the check is blind: %s must link go-redis", pkg)
		}
		if !bridge {
			t.Fatalf("%s links go-redis without internal/redislog", pkg)
		}
	}
}
