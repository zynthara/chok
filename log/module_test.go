package log_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
)

// levelRecorder wraps a Logger to observe SetLevel calls.
type levelRecorder struct {
	log.Logger
	levels []string
}

func (l *levelRecorder) SetLevel(level string) error {
	l.levels = append(l.levels, level)
	return nil
}

func TestLogModule_HotLevelReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(path, []byte("log:\n  level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader := conf.NewLoader("app", "APP")
	loader.SetPath(path)
	mod := log.Module()
	d := mod.Describe()
	if err := loader.Register(kernel.SectionKeyOf(d), d.Options); err != nil {
		t.Fatal(err)
	}
	store, err := conf.NewStore(loader)
	if err != nil {
		t.Fatal(err)
	}

	rec := &levelRecorder{Logger: log.Empty()}
	reg, err := kernel.New(kernel.Config{Store: store, Logger: rec, Components: []kernel.Component{mod}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Stop(context.Background()) })

	// level is reload:"hot" → the module must receive dispatch and
	// apply it to the live root logger.
	if err := os.WriteFile(path, []byte("log:\n  level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.levels) != 1 || rec.levels[0] != "debug" {
		t.Fatalf("SetLevel must be applied on hot reload: %v", rec.levels)
	}

	// format is restart-only → no second SetLevel.
	if err := os.WriteFile(path, []byte("log:\n  level: debug\n  format: text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.levels) != 1 {
		t.Fatalf("restart-only change must not dispatch: %v", rec.levels)
	}
}
