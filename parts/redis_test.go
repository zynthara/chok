package parts

import (
	"context"
	"testing"
	"time"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
)

type redisTestCfg struct {
	Redis *config.RedisOptions
}

func TestRedisComponent_Init_NilConfig_Disabled(t *testing.T) {
	c := NewRedisComponent(func(any) *config.RedisOptions { return nil })
	if err := c.Init(context.Background(), newMockKernel(&redisTestCfg{})); err != nil {
		t.Fatal(err)
	}
	if c.Client() != nil {
		t.Fatal("nil resolver should leave Client() nil")
	}
	s := c.Health(context.Background())
	if s.Status != component.HealthOK {
		t.Fatalf("disabled Redis should be OK, got %q", s.Status)
	}
}

func TestRedisComponent_Close_WhenDisabled(t *testing.T) {
	c := NewRedisComponent(func(any) *config.RedisOptions { return nil })
	if err := c.Init(context.Background(), newMockKernel(&redisTestCfg{})); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close when disabled should be nil, got %v", err)
	}
}

func TestRedisComponent_Health_PingTimeout_IsDown(t *testing.T) {
	// Point at an unreachable address; Ping will fail within the tight
	// timeout and Health should flag the component Down.
	cfg := &redisTestCfg{Redis: &config.RedisOptions{
		Addr: "127.0.0.1:1", // port 1 is reserved; connection refused instantly
		DB:   0,
	}}
	c := NewRedisComponent(func(a any) *config.RedisOptions {
		return a.(*redisTestCfg).Redis
	})
	c.SetPingTimeout(50 * time.Millisecond)

	if err := c.Init(context.Background(), newMockKernel(cfg)); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	s := c.Health(context.Background())
	if s.Status != component.HealthDown {
		t.Fatalf("unreachable Redis should be Down, got %q (err=%s)", s.Status, s.Error)
	}
	if s.Error == "" {
		t.Fatal("Down status should include an error message")
	}
}

func TestRedisComponent_Name_ConfigKey(t *testing.T) {
	c := NewRedisComponent(func(any) *config.RedisOptions { return nil })
	if c.Name() != "redis" {
		t.Fatalf("Name should be %q, got %q", "redis", c.Name())
	}
	if c.ConfigKey() != "redis" {
		t.Fatalf("ConfigKey should be %q, got %q", "redis", c.ConfigKey())
	}
}
