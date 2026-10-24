package logtap

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// The tests share the installed tap, a process global: none of them is
// parallel.

func TestWrap_PassesThroughWithoutATap(t *testing.T) {
	var out bytes.Buffer
	l := slog.New(Wrap(slog.NewTextHandler(&out, nil)))
	l.With("k", "v").WithGroup("g").Info("hello", "a", 1)
	if got := out.String(); !strings.Contains(got, `msg=hello k=v g.a=1`) {
		t.Fatalf("the wrapped handler writes as before: %q", got)
	}
}

func TestInstall_CopiesRecordsWithAttrsAndGroups(t *testing.T) {
	var out, tapped bytes.Buffer
	level := new(slog.LevelVar)
	l := slog.New(Wrap(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: level})))
	child := l.With("request_id", "r1").WithGroup("req")

	uninstall := Install(slog.NewTextHandler(&tapped, &slog.HandlerOptions{Level: slog.LevelDebug}))
	child.Info("served", "status", 200)
	l.Debug("below the wrapped handler's level")
	level.Set(slog.LevelDebug)
	l.Debug("at it now")
	uninstall()
	child.Info("after uninstall")

	got := tapped.String()
	for _, want := range []string{`msg=served request_id=r1 req.status=200`, `msg="at it now"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("the tap misses %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "below") || strings.Contains(got, "after uninstall") {
		t.Fatalf("the tap gets what the wrapped handler is enabled for, while installed:\n%s", got)
	}
	if !strings.Contains(out.String(), "after uninstall") {
		t.Fatalf("the wrapped handler keeps writing: %s", out.String())
	}
}

func TestInstall_LaterInstallWins(t *testing.T) {
	var first, second bytes.Buffer
	l := slog.New(Wrap(slog.NewTextHandler(io.Discard, nil))).With("k", "v")
	undoFirst := Install(slog.NewTextHandler(&first, nil))
	l.Info("one")
	undoSecond := Install(slog.NewTextHandler(&second, nil))
	l.Info("two")
	undoFirst() // replaced already: removes nothing
	l.Info("three")
	undoSecond()
	l.Info("four")
	if f, s := first.String(), second.String(); !strings.Contains(f, "msg=one k=v") || strings.Contains(f, "two") ||
		!strings.Contains(s, "msg=two k=v") || !strings.Contains(s, "msg=three") || strings.Contains(s, "four") {
		t.Fatalf("first:\n%s\nsecond:\n%s", f, s)
	}
}

func TestHandle_ConcurrentWithInstall(t *testing.T) {
	l := slog.New(Wrap(slog.NewTextHandler(io.Discard, nil))).With("k", "v")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				l.InfoContext(context.Background(), "x")
			}
		})
	}
	for range 50 {
		Install(slog.NewTextHandler(io.Discard, nil))()
	}
	wg.Wait()
}
