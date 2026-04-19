package db

import (
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

const tracerName = "github.com/zynthara/chok/db"

// EnableTracing registers GORM callbacks that create OpenTelemetry spans
// for every query. Each span records the SQL operation (CREATE, QUERY,
// UPDATE, DELETE) and the number of affected rows.
//
// Uses the global TracerProvider — when tracing is disabled (noop
// provider), the callbacks add negligible overhead (no allocations,
// no recording).
//
// Call after *gorm.DB is opened:
//
//	gdb, _ := db.NewSQLite(opts)
//	db.EnableTracing(gdb)
func EnableTracing(gdb *gorm.DB) {
	tracer := otel.Tracer(tracerName)

	// Before callbacks: start a span.
	before := func(opName string) func(*gorm.DB) {
		return func(tx *gorm.DB) {
			if tx.Statement == nil || tx.Statement.Context == nil {
				return
			}
			ctx, span := tracer.Start(tx.Statement.Context, "gorm."+opName,
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(
					attribute.String("db.system", "gorm"),
					attribute.String("db.operation", opName),
				),
			)
			tx.Statement.Context = ctx
			tx.InstanceSet("otel:span", span)
		}
	}

	// After callbacks: record results and end the span.
	after := func(tx *gorm.DB) {
		v, ok := tx.InstanceGet("otel:span")
		if !ok {
			return
		}
		span, ok := v.(trace.Span)
		if !ok {
			return
		}
		defer span.End()

		span.SetAttributes(attribute.Int64("db.rows_affected", tx.RowsAffected))
		if tx.Statement != nil && tx.Statement.Table != "" {
			span.SetAttributes(attribute.String("db.table", tx.Statement.Table))
		}
		if tx.Error != nil && !errors.Is(tx.Error, gorm.ErrRecordNotFound) {
			span.SetAttributes(attribute.String("error.message", tx.Error.Error()))
		}
	}

	cb := gdb.Callback()
	_ = cb.Create().Before("gorm:create").Register("otel:before_create", before("create"))
	_ = cb.Create().After("gorm:create").Register("otel:after_create", after)
	_ = cb.Query().Before("gorm:query").Register("otel:before_query", before("query"))
	_ = cb.Query().After("gorm:query").Register("otel:after_query", after)
	_ = cb.Update().Before("gorm:update").Register("otel:before_update", before("update"))
	_ = cb.Update().After("gorm:update").Register("otel:after_update", after)
	_ = cb.Delete().Before("gorm:delete").Register("otel:before_delete", before("delete"))
	_ = cb.Delete().After("gorm:delete").Register("otel:after_delete", after)
	_ = cb.Row().Before("gorm:row").Register("otel:before_row", before("row"))
	_ = cb.Row().After("gorm:row").Register("otel:after_row", after)
}
