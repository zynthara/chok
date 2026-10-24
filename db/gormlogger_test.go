package db

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestGORMLogger_ThresholdAndClassification(t *testing.T) {
	var buf bytes.Buffer
	d := &DB{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	g := &gormLogger{d: d, slow: 50 * time.Millisecond, level: gormlogger.Warn}
	sql := func() (string, int64) { return "SELECT * FROM t WHERE email = ?", 1 }
	ctx := context.Background()

	g.Trace(ctx, time.Now().Add(-time.Second), sql, nil)
	if !strings.Contains(buf.String(), "slow query") || !strings.Contains(buf.String(), "email = ?") {
		t.Fatalf("a slow query is a warning with placeholders: %s", buf.String())
	}
	buf.Reset()
	g.Trace(ctx, time.Now(), sql, nil)
	if buf.Len() != 0 {
		t.Fatalf("a fast query is debug: %s", buf.String())
	}
	g.Trace(ctx, time.Now(), sql, errors.New("Error 1062 (23000): Duplicate entry 'a' for key 'uk'"))
	g.Trace(ctx, time.Now(), sql, gorm.ErrRecordNotFound)
	g.Trace(ctx, time.Now(), sql, context.Canceled)
	if buf.Len() != 0 {
		t.Fatalf("expected outcomes are not errors: %s", buf.String())
	}
	g.Trace(ctx, time.Now(), sql, errors.New("connection reset by peer"))
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "query failed") {
		t.Fatalf("a fault is an error: %s", buf.String())
	}
	if s, args := g.ParamsFilter(ctx, "SELECT ?", "secret"); s != "SELECT ?" || args != nil {
		t.Fatal("bound values never reach the log")
	}
	g0 := g.LogMode(gormlogger.Silent).(*gormLogger)
	buf.Reset()
	g0.Trace(ctx, time.Now().Add(-time.Second), sql, errors.New("boom"))
	if buf.Len() != 0 {
		t.Fatal("silent mode logs nothing")
	}
}

func TestGORMLogger_SlowThresholdZeroDisables(t *testing.T) {
	var buf bytes.Buffer
	d := &DB{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	g := &gormLogger{d: d, level: gormlogger.Warn}
	g.Trace(context.Background(), time.Now().Add(-time.Hour), func() (string, int64) { return "SELECT 1", 0 }, nil)
	if buf.Len() != 0 {
		t.Fatalf("slow_threshold 0 disables slow logs: %s", buf.String())
	}
}
