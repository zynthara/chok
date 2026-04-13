package log

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// GORMLogger adapts a chok Logger to gorm/logger.Interface.
func GORMLogger(l Logger) gormlogger.Interface {
	return &gormLog{logger: l, level: gormlogger.Warn}
}

type gormLog struct {
	logger Logger
	level  gormlogger.LogLevel
}

func (g *gormLog) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	return &gormLog{logger: g.logger, level: level}
}

func (g *gormLog) Info(ctx context.Context, msg string, data ...any) {
	if g.level >= gormlogger.Info {
		g.logger.InfoContext(ctx, fmt.Sprintf(msg, data...))
	}
}

func (g *gormLog) Warn(ctx context.Context, msg string, data ...any) {
	if g.level >= gormlogger.Warn {
		g.logger.WarnContext(ctx, fmt.Sprintf(msg, data...))
	}
}

func (g *gormLog) Error(ctx context.Context, msg string, data ...any) {
	if g.level >= gormlogger.Error {
		g.logger.ErrorContext(ctx, fmt.Sprintf(msg, data...))
	}
}

func (g *gormLog) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	if g.level <= gormlogger.Silent {
		return
	}
	elapsed := time.Since(begin)
	sql, rows := fc()

	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && g.level >= gormlogger.Error:
		g.logger.ErrorContext(ctx, "gorm",
			"error", err, "elapsed", elapsed.String(), "rows", rows, "sql", sql)
	case g.level >= gormlogger.Info:
		g.logger.InfoContext(ctx, "gorm",
			"elapsed", elapsed.String(), "rows", rows, "sql", sql)
	}
}
