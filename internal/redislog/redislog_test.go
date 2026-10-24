package redislog

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestLogger_WritesToSlogDefault: a go-redis message lands in whatever
// slog.Default is at the time, at warn, marked as go-redis's.
func TestLogger_WritesToSlogDefault(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logger{}.Printf(t.Context(), "redis: connection pool: failed to dial after %d attempts", 5)

	line := buf.String()
	for _, want := range []string{"level=WARN", `msg="redis: connection pool: failed to dial after 5 attempts"`, "logger=go-redis"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %s in %q", want, line)
		}
	}
}
