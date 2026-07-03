package redis_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/redis"
)

func TestModule_InitExposesWorkingClient(t *testing.T) {
	mr := miniredis.RunT(t)
	tk := choktest.NewTestKernel(t, fmt.Sprintf(`
redis:
  addr: %s
`, mr.Addr()), redis.Module())

	rc, ok := kernel.Get[*redis.Component](tk, "redis")
	if !ok {
		t.Fatal("redis component not visible via kernel.Get")
	}
	client := rc.Client()
	if client == nil {
		t.Fatal("Client() returned nil after Init")
	}
	if err := client.Set(context.Background(), "k", "v", 0).Err(); err != nil {
		t.Fatalf("SET through the shared client: %v", err)
	}
	got, err := client.Get(context.Background(), "k").Result()
	if err != nil || got != "v" {
		t.Fatalf("GET = %q, %v; want \"v\", nil", got, err)
	}
	if err := rc.Health(context.Background()); err != nil {
		t.Fatalf("Health with reachable redis: %v", err)
	}
}

func TestModule_RoleInterfaceClientAccessor(t *testing.T) {
	// Peers (cache, authz watcher) consume the client through this
	// structural role interface — pin the shape.
	mr := miniredis.RunT(t)
	tk := choktest.NewTestKernel(t, "redis:\n  addr: "+mr.Addr()+"\n", redis.Module())

	rc, ok := kernel.Get[interface{ Client() *goredis.Client }](tk, "redis")
	if !ok {
		t.Fatal("redis component must satisfy the Client() role interface")
	}
	if rc.Client() == nil {
		t.Fatal("role-interface Client() returned nil")
	}
}

func TestModule_Disabled_NotVisible(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
redis:
  enabled: false
`, redis.Module())

	if _, ok := kernel.Get[*redis.Component](tk, "redis"); ok {
		t.Fatal("disabled redis must not be reachable via kernel.Get")
	}
}

func TestModule_UnreachableAddr_StillBoots(t *testing.T) {
	// Transient boot-time unreachability must not fail startup (the
	// v1 warn-only semantic): the client retries on demand and Health
	// keeps reporting the truth.
	tk := choktest.NewTestKernel(t, `
redis:
  addr: 127.0.0.1:1
`, redis.Module())

	rc, ok := kernel.Get[*redis.Component](tk, "redis")
	if !ok {
		t.Fatal("component should be up despite unreachable redis")
	}
	if err := rc.Health(context.Background()); err == nil {
		t.Fatal("Health should report the unreachable backend")
	}
}

func TestModule_BadCACert_FailsStartup(t *testing.T) {
	_, err := choktest.StartKernel(t, `
redis:
  addr: 127.0.0.1:6379
  ca_cert: /no/such/ca.pem
`, redis.Module())
	if err == nil {
		t.Fatal("expected startup failure for missing ca_cert")
	}
	if !strings.Contains(err.Error(), "ca_cert") {
		t.Fatalf("error should name ca_cert, got: %v", err)
	}
}

func TestModule_RedactedSettingsMaskPassword(t *testing.T) {
	// §12.9 closure evidence: the registered redis section's password
	// is masked in the snapshot-wide redacted dump.
	mr := miniredis.RunT(t)
	tk := choktest.NewTestKernel(t, fmt.Sprintf(`
redis:
  addr: %s
  password: super-secret-credential
`, mr.Addr()), redis.Module())

	dump := fmt.Sprintf("%v", tk.Config().RedactedSettings())
	if strings.Contains(dump, "super-secret-credential") {
		t.Fatalf("RedactedSettings leaked the redis password: %s", dump)
	}
}
