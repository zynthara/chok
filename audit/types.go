package audit

import (
	"context"
	"time"
)

// Result is the canonical outcome enum stored in Log.Result. The
// values are stable: changing them would break audit history queries
// and admin UI filters. New outcomes go through SPEC review.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
	ResultDenied  = "denied"
)

// Actor type enum. Stored in Log.ActorType. Distinguishes a real user
// from system-driven actions (cron, replication) and machine-to-
// machine API key callers (where ActorID is the key ID, not a user).
const (
	ActorTypeUser   = "user"
	ActorTypeSystem = "system"
	ActorTypeAPIKey = "api_key"
)

// Entry is the operator-facing audit input. Callers fill the
// "what" fields; framework helpers (FromContext) populate "who" and
// observability cross-references. Entry is the wire shape; Logger
// implementations are responsible for translating it into Log
// records (assigning ID, defaulting Result, marshalling
// Before/After to JSON).
//
// Required fields: Action. Resource is strongly recommended; an
// empty Resource is allowed for events that aren't bound to a
// concrete object (e.g. "auth.login" — the actor IS the resource,
// pointed at via ActorID/ActorType).
//
// Defaulting: Logger implementations apply Result=ResultSuccess
// when blank, ActorType=ActorTypeUser when ActorID is non-empty
// and ActorType is blank, and OccurredAt=time.Now() when zero.
// Operators that need different defaults set the field explicitly.
type Entry struct {
	// What — required.
	Action string

	// On what — recommended; some events have no resource binding.
	Resource   string
	ResourceID string

	// Outcome. Defaults to ResultSuccess when blank.
	Result string

	// Optional diffs (operator-supplied any; Logger marshals to JSON).
	// Strip large/sensitive fields client-side — the Logger does not
	// size-cap.
	Before any
	After  any

	// Operator note. Plain text, ≤ 512 chars (truncated by sink if
	// longer; depending on dialect VARCHAR(512) may itself reject).
	Reason string

	// Free-form structured metadata. Marshalled to JSON.
	Metadata map[string]any

	// Who — typically populated by FromContext. Callers may override
	// (e.g. system actions setting ActorType=ActorTypeSystem).
	ActorID   string
	ActorType string
	ActorIP   string

	// Cross-references — populated by FromContext from OTel + ctxval.
	TraceID   string
	RequestID string

	// OccurredAt is when the event happened from the caller's
	// perspective. Zero ⇒ "now" at sink time. Non-zero overrides
	// for events backfilled from external systems.
	OccurredAt time.Time
}

// Query is the parameter shape for Logger.Query — admin UI / CLI
// pagination + filter. Empty fields mean "any". Time range bounds
// are inclusive on both ends; zero time on either side disables
// that bound. Page is 1-indexed; Size <= 0 falls back to a Logger-
// implementation default (typical 50).
type Query struct {
	ActorID    string
	Resource   string
	ResourceID string
	Action     string
	Result     string

	From, To time.Time

	Page, Size int
}

// Logger is the contract every audit sink implements. The audit
// Component wires *DBLogger (async DB sink) in; tests can inject a
// *MemoryLogger or similar without changing call sites.
//
// Lifecycle: Logger is bound to the audit Component. After
// Component.Close, calls to Log/LogSync return errors / drop on the
// floor — caller code must not assume the sink remains live past
// shutdown. Query is read-only against the underlying table; it
// keeps working as long as DB stays alive.
type Logger interface {
	// LogSync writes synchronously and returns when the row is
	// committed (or fails). Use ONLY when audit success is a
	// pre-condition for the business operation (rare — typically
	// "compliance refused, abort the action"). For all other
	// callsites prefer Log.
	LogSync(ctx context.Context, entry Entry) error

	// Log enqueues asynchronously. Returns immediately on success
	// (entry queued) or when the buffer is full and DropOnFull=true
	// (entry counted as dropped). With DropOnFull=false the call
	// blocks until the buffer has space — that's the back-pressure
	// path for compliance-critical workloads.
	Log(ctx context.Context, entry Entry)

	// Query reads the persisted records. Returns the page slice and
	// the total matching row count for pagination UI. The total is
	// computed under the same filter as the page so admin UIs
	// don't see drift between page count and total.
	Query(ctx context.Context, q Query) ([]Log, int64, error)
}

// Stats is exposed by Logger implementations that want to surface
// async-sink health to operators (matching the WatcherStats
// pattern in authz/casbin). Not all Loggers implement this — the
// in-memory test logger doesn't; the DB-backed DBLogger does.
// Callers type-assert on this interface to decide whether to
// render the panel.
type Stats struct {
	Pending uint64 // entries in the async buffer right now
	Dropped uint64 // lifetime DropOnFull-mode rejections
	Written uint64 // lifetime successful inserts (committed rows)
	Failed  uint64 // lifetime sink failures (DB error etc.)
}

// Statser is the optional escape hatch. DBLogger satisfies it;
// admin / health endpoints type-assert to render counters.
type Statser interface {
	Stats() Stats
}
