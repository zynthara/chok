package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// gorm's Scan runs its query under gorm's recorder logger, then hands the
// SQL the recorder kept to the handle's logger: the handle's ParamsFilter
// is never asked about that query. The recorder asks only the package
// variable gormlogger.RecorderParamsFilter, whose default keeps the
// values, so it is set here. It is process-wide: in a process that
// imports chok/db, a query read with Scan through any gorm handle, chok's
// or another, is logged with its placeholders.
func init() {
	gormlogger.RecorderParamsFilter = keepPlaceholders
}

// keepPlaceholders drops a statement's bound values, so gorm logs its SQL
// with the placeholders where the values would go.
func keepPlaceholders(_ context.Context, sql string, _ ...any) (string, []any) {
	return sql, nil
}

// gormLogger bridges gorm to the handle's slog logger: failed statements
// at error (a missing record is not a failure), slow ones at warn, the
// rest at debug. SQL is logged with placeholders, never with values, a
// query read with Scan included (see init).
type gormLogger struct {
	d     *DB
	slow  time.Duration
	level gormlogger.LogLevel
}

func (g *gormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	cp := *g
	cp.level = level
	return &cp
}

// ParamsFilter keeps bound values out of the logged SQL.
func (g *gormLogger) ParamsFilter(ctx context.Context, sql string, params ...any) (string, []any) {
	return keepPlaceholders(ctx, sql, params...)
}

func (g *gormLogger) Info(ctx context.Context, msg string, data ...any) {
	g.d.Logger().InfoContext(ctx, "db: "+fmt.Sprintf(msg, data...))
}

func (g *gormLogger) Warn(ctx context.Context, msg string, data ...any) {
	g.d.Logger().WarnContext(ctx, "db: "+fmt.Sprintf(msg, data...))
}

func (g *gormLogger) Error(ctx context.Context, msg string, data ...any) {
	g.d.Logger().ErrorContext(ctx, "db: "+fmt.Sprintf(msg, data...))
}

func (g *gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if g.level == gormlogger.Silent {
		return
	}
	l := g.d.Logger()
	elapsed := time.Since(begin)
	switch {
	case err != nil && !expected(err):
		sql, rows := fc()
		l.ErrorContext(ctx, "db: query failed", "error", err, "elapsed", elapsed, "rows", rows, "sql", sql)
	case err != nil:
		if l.Enabled(ctx, slog.LevelDebug) {
			sql, rows := fc()
			l.DebugContext(ctx, "db: query refused", "error", err, "elapsed", elapsed, "rows", rows, "sql", sql)
		}
	case g.slow > 0 && elapsed >= g.slow:
		sql, rows := fc()
		l.WarnContext(ctx, "db: slow query", "elapsed", elapsed, "threshold", g.slow, "rows", rows, "sql", sql)
	case l.Enabled(ctx, slog.LevelDebug):
		sql, rows := fc()
		l.DebugContext(ctx, "db: query", "elapsed", elapsed, "rows", rows, "sql", sql)
	}
}

// expected reports errors that are outcomes rather than faults: a missing
// record, a canceled request, a constraint the data violated (store turns
// those into 404, 409 and 400 answers).
func expected(err error) bool {
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"duplicate", "unique constraint", "foreign key", "constraint failed", "violates check constraint", "check constraint"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
