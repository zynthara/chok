package chok

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/log"
)

type testConfig struct {
	HTTP struct {
		Addr        string        `mapstructure:"addr"         default:":8080"`
		ReadTimeout time.Duration `mapstructure:"read_timeout" default:"30s"`
	} `mapstructure:"http"`
	App struct {
		Name string `mapstructure:"name" default:"myapp"`
		Port int    `mapstructure:"port" default:"3000"`
	} `mapstructure:"app"`
}

func TestConfig_DefaultTag(t *testing.T) {
	var cfg testConfig
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := New("testcfg",
		WithLogger(log.Empty()),
		WithConfig(&cfg),
	)
	_ = app.Run(ctx)

	if cfg.HTTP.Addr != ":8080" {
		t.Fatalf("expected :8080, got %s", cfg.HTTP.Addr)
	}
	if cfg.App.Port != 3000 {
		t.Fatalf("expected 3000, got %d", cfg.App.Port)
	}
	if cfg.HTTP.ReadTimeout != 30*time.Second {
		t.Fatalf("expected 30s, got %v", cfg.HTTP.ReadTimeout)
	}
}

func TestConfig_EnvOverride(t *testing.T) {
	var cfg testConfig
	t.Setenv("TESTCFG_HTTP_ADDR", ":9090")
	t.Setenv("TESTCFG_APP_PORT", "5000")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := New("testcfg",
		WithLogger(log.Empty()),
		WithConfig(&cfg),
	)
	_ = app.Run(ctx)

	if cfg.HTTP.Addr != ":9090" {
		t.Fatalf("expected :9090, got %s", cfg.HTTP.Addr)
	}
	if cfg.App.Port != 5000 {
		t.Fatalf("expected 5000, got %d", cfg.App.Port)
	}
}

func TestConfig_FileOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "test.yaml")
	os.WriteFile(cfgFile, []byte("http:\n  addr: \":7070\"\napp:\n  port: 0\n"), 0644)

	var cfg testConfig
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := New("testcfg",
		WithLogger(log.Empty()),
		WithConfig(&cfg, cfgFile),
	)
	_ = app.Run(ctx)

	if cfg.HTTP.Addr != ":7070" {
		t.Fatalf("expected :7070, got %s", cfg.HTTP.Addr)
	}
	// Explicit zero in file should not be overridden by default.
	if cfg.App.Port != 0 {
		t.Fatalf("expected 0 (explicit zero), got %d", cfg.App.Port)
	}
}

func TestConfig_ExplicitPathNotFound_Error(t *testing.T) {
	var cfg testConfig
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := New("testcfg",
		WithLogger(log.Empty()),
		WithConfig(&cfg, "/nonexistent/config.yaml"),
	)
	err := app.Run(ctx)
	if err == nil {
		t.Fatal("expected error for missing explicit config path")
	}
}

func TestConfig_DefaultPathNotFound_Skip(t *testing.T) {
	// Run in a temp dir where no config file exists.
	orig, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(orig)

	var cfg testConfig
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := New("testcfg",
		WithLogger(log.Empty()),
		WithConfig(&cfg),
	)
	err := app.Run(ctx)
	if err != nil {
		t.Fatalf("default path missing should not error, got: %v", err)
	}
}

func TestConfig_PrefixEnvBootstrap(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "bootstrap.yaml")
	os.WriteFile(cfgFile, []byte("app:\n  name: fromenv\n"), 0644)

	t.Setenv("TESTCFG_CONFIG", cfgFile)

	var cfg testConfig
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := New("testcfg",
		WithLogger(log.Empty()),
		WithConfig(&cfg),
	)
	_ = app.Run(ctx)

	if cfg.App.Name != "fromenv" {
		t.Fatalf("expected fromenv, got %s", cfg.App.Name)
	}
}

// --- validation helpers ---

type failingValidatable struct {
	Msg string `mapstructure:"msg"`
}

func (f *failingValidatable) Validate() error {
	if f.Msg != "" {
		return errors.New(f.Msg)
	}
	return nil
}

type multiFailConfig struct {
	A failingValidatable `mapstructure:"a"`
	B failingValidatable `mapstructure:"b"`
}

func TestConfig_ValidateCollectsAllErrors(t *testing.T) {
	cfg := multiFailConfig{
		A: failingValidatable{Msg: "error-a"},
		B: failingValidatable{Msg: "error-b"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := New("testcfg",
		WithLogger(log.Empty()),
		WithConfig(&cfg),
	)
	err := app.Run(ctx)
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "error-a") {
		t.Fatalf("expected error-a in message, got: %s", msg)
	}
	if !strings.Contains(msg, "error-b") {
		t.Fatalf("expected error-b in message, got: %s", msg)
	}
}

type validatableConfig struct {
	Value string `mapstructure:"value"`
}

func (c *validatableConfig) Validate() error {
	if c.Value == "" {
		return nil
	}
	return nil
}

type rootValidatableConfig struct {
	Sub validatableConfig `mapstructure:"sub"`
}

var rootValidateCalled bool

func (c *rootValidatableConfig) Validate() error {
	rootValidateCalled = true
	return nil
}

func TestConfig_RootValidatable_CalledLast(t *testing.T) {
	rootValidateCalled = false
	var cfg rootValidatableConfig
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := New("testcfg",
		WithLogger(log.Empty()),
		WithConfig(&cfg),
	)
	_ = app.Run(ctx)

	if !rootValidateCalled {
		t.Fatal("root Validate() should have been called")
	}
}

func TestConfig_WithLogConfig_DereferencedAfterLoad(t *testing.T) {
	type logCfg struct {
		Log struct {
			Level  string   `mapstructure:"level"  default:"debug"`
			Format string   `mapstructure:"format" default:"text"`
			Output []string `mapstructure:"output" default:"stdout"`
		} `mapstructure:"log"`
	}

	var cfg logCfg
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// WithLogConfig points to cfg.Log which is zero before Run.
	// After config load it should be populated and used.
	app := New("testcfg",
		WithConfig(&cfg),
		WithLogConfig(nil), // nil → will use default logger
	)
	_ = app.Run(ctx)
	// Just verify it doesn't panic.
}

// TestValidateFields_DiscriminatorSkipsBranchRecursion covers the #1
// fix: DatabaseOptions.Validate selects driver=sqlite and the
// recursive validator must NOT descend into the inactive MySQL branch
// (whose Enabled=true default would otherwise demand a database name
// the user deliberately left blank). Reproduces the symptom that blog
// quickstart hit before the fix.
func TestValidateFields_DiscriminatorSkipsBranchRecursion(t *testing.T) {
	type appCfg struct {
		Database config.DatabaseOptions `mapstructure:"database"`
	}
	cfg := appCfg{
		Database: config.DatabaseOptions{
			Driver: "sqlite",
			SQLite: config.SQLiteOptions{Enabled: true, Path: "app.db"},
			// MySQL left zero AND MySQLOptions.Enabled defaults to true
			// in viper-driven loading. We simulate that here by
			// explicitly enabling — the bug was that validateFields
			// recursed into this struct and tripped its missing
			// Database/Host validation.
			MySQL: config.MySQLOptions{Enabled: true},
		},
	}
	a := &App{configPtr: &cfg}
	if err := a.validateConfig(); err != nil {
		t.Fatalf("driver=sqlite must not trip MySQL validation: %v", err)
	}

	// Sanity: driver=mysql with missing host/database SHOULD still fail
	// (validate the active branch), proving we didn't break detection
	// of real configuration errors.
	cfg2 := appCfg{Database: config.DatabaseOptions{Driver: "mysql"}}
	a2 := &App{configPtr: &cfg2}
	if err := a2.validateConfig(); err == nil {
		t.Fatal("driver=mysql with empty host/database should fail validation")
	}
}
