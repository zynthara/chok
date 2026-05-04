package audit_test

import (
	"context"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/zynthara/chok/audit"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/internal/ctxval"
)

// TestFromContext_NilCtx pins the defensive guard: FromContext must
// not panic on a nil context. Real callsites always have a ctx, but
// a nil dereference here would turn a misuse into an entire
// goroutine crash and lose the audit trail wholesale.
func TestFromContext_NilCtx(t *testing.T) {
	// Defensive: real callsites always have a ctx, but a nil
	// dereference inside FromContext would crash the calling
	// goroutine and lose the audit trail wholesale. Test the
	// guard with a typed-nil ctx so the linter recognises it as
	// intentional (passing literal nil triggers SA1012).
	var ctx context.Context
	got := audit.FromContext(ctx)
	if !reflect.DeepEqual(got, audit.Entry{}) {
		t.Errorf("FromContext(nil) should be zero Entry, got %+v", got)
	}
}

// TestFromContext_EmptyCtx asserts an empty (background) context
// produces a zero-valued Entry — ActorID blank, no panic, all
// missing-source fields documented as "" in the package doc.
func TestFromContext_EmptyCtx(t *testing.T) {
	got := audit.FromContext(context.Background())
	if got.ActorID != "" || got.ActorType != "" || got.ActorIP != "" || got.RequestID != "" || got.TraceID != "" {
		t.Errorf("empty ctx should produce blank Entry, got %+v", got)
	}
}

// TestFromContext_PrincipalFillsActor — auth.Principal in ctx maps
// to ActorID + ActorType=ActorTypeUser. ActorType auto-defaults so
// the common case (HTTP user) needs no extra wiring.
func TestFromContext_PrincipalFillsActor(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Subject: "usr_alice"})
	got := audit.FromContext(ctx)
	if got.ActorID != "usr_alice" {
		t.Errorf("ActorID = %q, want %q", got.ActorID, "usr_alice")
	}
	if got.ActorType != audit.ActorTypeUser {
		t.Errorf("ActorType = %q, want %q", got.ActorType, audit.ActorTypeUser)
	}
}

// TestFromContext_RequestIDAndIP — both pure ctxval-derived fields
// land in Entry without any extra plumbing.
func TestFromContext_RequestIDAndIP(t *testing.T) {
	ctx := ctxval.WithRequestID(context.Background(), "req_abc")
	ctx = ctxval.WithClientIP(ctx, "203.0.113.7")
	got := audit.FromContext(ctx)
	if got.RequestID != "req_abc" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	if got.ActorIP != "203.0.113.7" {
		t.Errorf("ActorIP = %q", got.ActorIP)
	}
}

// TestFromContext_OTelSpanFillsTraceID — when an OTel span is
// active in ctx, FromContext copies the trace-id into Entry.TraceID
// for cross-correlation with traces in the dashboards.
func TestFromContext_OTelSpanFillsTraceID(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatal(err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	got := audit.FromContext(ctx)
	if got.TraceID != traceID.String() {
		t.Errorf("TraceID = %q, want %q", got.TraceID, traceID.String())
	}
}

// TestFromContext_NoOTelSpan — without an OTel span (sampler off /
// no instrumentation), TraceID stays blank. The audit row writes
// fine; downstream queries that expect TraceID just lose the join.
func TestFromContext_NoOTelSpan(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Subject: "usr_alice"})
	got := audit.FromContext(ctx)
	if got.TraceID != "" {
		t.Errorf("TraceID should be blank when no span active, got %q", got.TraceID)
	}
	if got.ActorID == "" {
		t.Error("ActorID should still be filled even without a span")
	}
}

// TestMergeContext_PreservesExplicitFields pins the override
// semantics: caller-set fields win over context-derived fields.
// Use case is system-actor events where ActorType=ActorTypeSystem
// must not be clobbered just because ctx happens to carry a user
// Principal (e.g. cron task triggered by a user upload).
func TestMergeContext_PreservesExplicitFields(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Subject: "usr_alice"})
	ctx = ctxval.WithClientIP(ctx, "203.0.113.7")
	ctx = ctxval.WithRequestID(ctx, "req_abc")

	pre := audit.Entry{
		ActorID:   "system_cron",
		ActorType: audit.ActorTypeSystem,
		ActorIP:   "10.0.0.1",
		RequestID: "scheduled-job-42",
		TraceID:   "preset-trace",
	}
	got := audit.MergeContext(ctx, pre)
	if !reflect.DeepEqual(got, pre) {
		t.Errorf("MergeContext clobbered explicit fields: got %+v, want %+v", got, pre)
	}
}

// TestMergeContext_FillsBlanks — only missing fields are filled.
func TestMergeContext_FillsBlanks(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Subject: "usr_alice"})
	ctx = ctxval.WithClientIP(ctx, "203.0.113.7")

	pre := audit.Entry{Action: "task.cancel", Reason: "user request"}
	got := audit.MergeContext(ctx, pre)

	if got.ActorID != "usr_alice" {
		t.Errorf("ActorID not filled: %q", got.ActorID)
	}
	if got.ActorType != audit.ActorTypeUser {
		t.Errorf("ActorType not filled: %q", got.ActorType)
	}
	if got.ActorIP != "203.0.113.7" {
		t.Errorf("ActorIP not filled: %q", got.ActorIP)
	}
	if got.Action != "task.cancel" || got.Reason != "user request" {
		t.Errorf("explicit fields drifted: %+v", got)
	}
}
