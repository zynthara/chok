package audit

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/internal/ctxval"
)

// FromContext returns an Entry pre-populated with framework-derived
// fields: ActorID/ActorType from auth.Principal, ActorIP from
// ctxval.ClientIPFrom, TraceID from the active OTel span,
// RequestID from ctxval.RequestIDFrom. Caller fills the
// business-side fields (Action, Resource, ResourceID, Before,
// After, Reason, Metadata) and passes the result to Logger.Log.
//
// Behaviour when a source is missing:
//
//   - no auth.Principal in ctx ⇒ ActorID/ActorType blank, the entry
//     is still loggable (the action was anonymous — auditors see
//     that explicitly).
//   - no OTel span / sampler turned off ⇒ TraceID blank, no error.
//   - nil ctx ⇒ all fields blank; FromContext does not panic. Real
//     callsites always have a ctx; the nil guard is purely
//     defensive.
//
// FromContext does not set OccurredAt — that's left to the Logger
// implementation so the timestamp reflects the moment of sink (when
// the row is in flight to the DB), not the moment the helper was
// called. Caller code that needs caller-side wallclock sets
// OccurredAt explicitly.
func FromContext(ctx context.Context) Entry {
	if ctx == nil {
		return Entry{}
	}
	e := Entry{
		ActorIP:   ctxval.ClientIPFrom(ctx),
		RequestID: ctxval.RequestIDFrom(ctx),
	}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		e.ActorID = p.Subject
		// ActorType is "user" by default whenever a Principal is
		// present; system / api_key callsites override explicitly.
		// We don't try to sniff the Principal.Claims for type — that
		// would be a fragile cross-package coupling, and the
		// override path is one line of caller code.
		e.ActorType = ActorTypeUser
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		e.TraceID = sc.TraceID().String()
	}
	return e
}

// MergeContext fills any framework-derived field on entry that the
// caller left blank. Use when a caller has partially constructed
// an Entry (e.g. set ActorType=ActorTypeSystem before invocation)
// but still wants automatic ActorIP / TraceID / RequestID. Fields
// the caller already populated are preserved verbatim — auditors
// must trust that an explicit field beats a context-derived one.
func MergeContext(ctx context.Context, e Entry) Entry {
	if ctx == nil {
		return e
	}
	if e.ActorID == "" {
		if p, ok := auth.PrincipalFrom(ctx); ok {
			e.ActorID = p.Subject
			if e.ActorType == "" {
				e.ActorType = ActorTypeUser
			}
		}
	}
	if e.ActorIP == "" {
		e.ActorIP = ctxval.ClientIPFrom(ctx)
	}
	if e.RequestID == "" {
		e.RequestID = ctxval.RequestIDFrom(ctx)
	}
	if e.TraceID == "" {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			e.TraceID = sc.TraceID().String()
		}
	}
	return e
}
