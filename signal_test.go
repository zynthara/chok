package chok

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startExecute runs app.Execute in the background, with exit stubbed, and
// waits until the App is ready — its signal handlers are installed by then.
func startExecute(t *testing.T, app *App) <-chan int {
	t.Helper()
	stubProcess(t)
	done := make(chan int, 1)
	go func() { done <- exited(app.Execute) }()
	waitFor(t, app.ready.Load, "ready")
	return done
}

func awaitExit(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return")
		return 0
	}
}

func TestSignal_SIGTERM_GracefulShutdown(t *testing.T) {
	done := startExecute(t, New(testBase(t), Logger(quiet())))
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	if code := awaitExit(t, done); code != -1 {
		t.Fatalf("a graceful stop returns without exiting, got exit %d", code)
	}
}

func TestSignal_SIGINT_GracefulShutdown(t *testing.T) {
	done := startExecute(t, New(testBase(t), Logger(quiet())))
	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	if code := awaitExit(t, done); code != -1 {
		t.Fatalf("got exit %d", code)
	}
}

func TestSignal_SIGQUIT_FastShutdown(t *testing.T) {
	base := testBase(t)
	base.HTTP.Enabled = true
	base.HTTP.Addr = "127.0.0.1:0"
	base.HTTP.DrainDelay = time.Hour // a fast stop skips it
	done := startExecute(t, New(base, Logger(quiet())))
	_ = syscall.Kill(os.Getpid(), syscall.SIGQUIT)
	if code := awaitExit(t, done); code != -1 {
		t.Fatalf("got exit %d", code)
	}
}

func TestSignal_FailureExits1(t *testing.T) {
	base := testBase(t)
	base.HTTP.Enabled = true
	base.HTTP.Addr = "256.0.0.1:0" // cannot listen
	stubProcess(t)
	app := New(base, Logger(quiet()))
	if code := exited(app.Execute); code != 1 {
		t.Fatalf("a failed run exits 1, got %d", code)
	}
}

func TestSignal_SIGHUP_NotFromLoad_KeepsRunning(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	app.reload = nil
	done := startExecute(t, app)
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(50 * time.Millisecond)
	select {
	case code := <-done:
		t.Fatalf("SIGHUP must not stop the App (exit %d)", code)
	default:
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitExit(t, done)
}

// TestSignal_SIGHUP_AppliesOnlyLogLevel rewrites the file between
// signals: the level follows it, other changes only log a warning.
func TestSignal_SIGHUP_AppliesOnlyLogLevel(t *testing.T) {
	before := slog.Default()
	t.Cleanup(func() { slog.SetDefault(before) })
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	cfgPath := filepath.Join(dir, "chok.yaml")
	write := func(level, addr string) {
		body := "http:\n  enabled: false\n  addr: \"" + addr + "\"\nlog:\n  level: " + level + "\n  output: [\"" + logPath + "\"]\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("info", ":1")
	base := MustLoad[Base]("app", ConfigFile(cfgPath), Args(nil))
	app := New(base)
	done := startExecute(t, app)

	write("debug", ":1")
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitFor(t, func() bool { return app.level.Level() == slog.LevelDebug }, "debug level")

	write("error", ":2")
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitFor(t, func() bool { return app.level.Level() == slog.LevelError }, "error level")
	if app.base.HTTP.Addr != ":1" {
		t.Fatal("SIGHUP must not change anything but the level")
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitExit(t, done)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	if strings.Count(log, "log level changed") != 2 || strings.Count(log, "require a restart") != 1 {
		t.Fatalf("log:\n%s", log)
	}
}

func TestSignal_SIGHUP_BadFileKeepsRunningConfig(t *testing.T) {
	before := slog.Default()
	t.Cleanup(func() { slog.SetDefault(before) })
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "chok.yaml")
	logPath := filepath.Join(dir, "app.log")
	body := "http:\n  enabled: false\nlog:\n  output: [\"" + logPath + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app := New(MustLoad[Base]("app", ConfigFile(cfgPath), Args(nil)))
	done := startExecute(t, app)
	if err := os.WriteFile(cfgPath, []byte(body+"  levl: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitFor(t, func() bool {
		raw, _ := os.ReadFile(logPath)
		return strings.Contains(string(raw), "configuration reload failed")
	}, "reload failure logged")
	if app.level.Level() != slog.LevelInfo {
		t.Fatal("a bad file keeps the running level")
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitExit(t, done)
}
