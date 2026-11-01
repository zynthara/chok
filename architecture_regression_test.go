package chok

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestReload_BelongsToItsBase(t *testing.T) {
	dir := t.TempDir()
	fa, fb := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	for _, f := range []string{fa, fb} {
		if err := os.WriteFile(f, []byte("log: {level: info}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := Load[Base]("architecturea", ConfigFile(fa), Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load[Base]("architectureb", ConfigFile(fb), Args(nil)); err != nil {
		t.Fatal(err)
	}
	app := New(a)
	defer app.closeLogs()
	if err := os.WriteFile(fb, []byte("log: {level: debug}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app.reloadLogLevel()
	if app.level.Level() != slog.LevelInfo {
		t.Fatalf("level=%v", app.level.Level())
	}
	if err := os.WriteFile(fa, []byte("log: {level: debug}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app.reloadLogLevel()
	if app.level.Level() != slog.LevelDebug {
		t.Fatal("App did not reload its own configuration")
	}

}

type architecturePanicHealth struct{}

func (architecturePanicHealth) Health(context.Context) error {
	panic("architecture probe health panic")
}
func TestHealth_PanicBecomesFailure(t *testing.T) {
	if err := callHealth(t.Context(), architecturePanicHealth{}); err == nil {
		t.Fatal("panic must fail the probe")
	}
}
func TestStart_BudgetBoundsHealthProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := testBase(t)
		b.App.StartTimeout = 10 * time.Millisecond
		b.HTTP.Health.ProbeTimeout = 80 * time.Millisecond
		app := New(b, Logger(quiet()))
		app.Use(architectureSlowHealth{})
		start := time.Now()
		err := app.Run(t.Context())
		elapsed := time.Since(start)
		if err == nil || elapsed != b.App.StartTimeout {
			t.Fatalf("elapsed=%v err=%v", elapsed, err)
		}

	})
}

type architectureSlowHealth struct{}

func (architectureSlowHealth) Health(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func TestClose_BeforeRunReleasesComponentsInReverseOrder(t *testing.T) {
	var events []int
	a := New(testBase(t), Logger(quiet()))
	a.Use(constructionCloser{1, &events}, constructionCloser{2, &events})
	if err := a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != 2 || events[1] != 1 {
		t.Fatalf("close order: %v", events)
	}
	if err := a.Run(t.Context()); err == nil {
		t.Fatal("closed App ran")
	}
}

type constructionCloser struct {
	id     int
	events *[]int
}

func (c constructionCloser) Close(context.Context) error {
	*c.events = append(*c.events, c.id)
	return nil
}
