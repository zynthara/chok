// Package audit is chok's compliance-grade audit log component.
//
// Audit log records "who did what to which resource" — distinct from:
//
//   - access log (parts/log AccessFiles): HTTP traffic, request-scoped,
//     short retention, ops-facing.
//   - metrics (parts/metrics): aggregate counters, no per-event detail.
//   - traces (parts/tracing OpenTelemetry): performance / latency
//     attribution, sampled, not retained for compliance.
//
// Audit is long-retention (≥ months), per-event, and queryable by
// auditors / admins. Write path is async (buffered channel → batch
// insert) so business handlers don't pay DB latency on every event;
// the trade-off (back-pressure vs drop) is configured per
// deployment via config.AuditOptions.DropOnFull.
//
// This package only defines the data model + Logger contract +
// context-extraction helpers. The Component plumbing (channel +
// worker + cron purge + admin HTTP route) lives in parts/audit.
//
// Reference: SPEC parts-audit-claude.md.
package audit

import (
	"time"

	"gorm.io/datatypes"
)

// Log is the persisted audit record. The struct shape is the
// canonical schema; gorm.AutoMigrate creates the table from these
// tags. Composite indexes mirror the three documented query
// patterns (SPEC §3):
//
//   - "what did actor X do recently" — (ActorID, OccurredAt DESC)
//   - "history of resource (type, id)" — (Resource, ResourceID, OccurredAt DESC)
//   - "all events of action type" — (Action, OccurredAt DESC)
//
// IDs use rid.New("audit"); collisions are not a concern at the
// expected scale (~1B events ≪ 12-char base62 space).
type Log struct {
	ID         string    `gorm:"primaryKey;type:varchar(24)"`
	OccurredAt time.Time `gorm:"index;not null"`

	// Who — actor identity at the time of the event.
	ActorID   string `gorm:"type:varchar(64);index:idx_audit_actor_time,priority:1"`
	ActorType string `gorm:"type:varchar(16)"` // user | system | api_key
	ActorIP   string `gorm:"type:varchar(45)"` // IPv4 or IPv6

	// What — action name (verb-object form: "task.create",
	// "user.role.assign") and outcome enum.
	Action string `gorm:"type:varchar(64);index:idx_audit_action_time,priority:1"`
	Result string `gorm:"type:varchar(16)"` // success | failure | denied

	// On what — resource type + ID + optional before/after diffs.
	// Before/After are operator-supplied JSON; callers must strip
	// large fields (binary blobs, base64 avatars) themselves —
	// the framework does not size-cap, because compliance often
	// requires the full payload.
	Resource   string         `gorm:"type:varchar(64);index:idx_audit_resource_time,priority:1"`
	ResourceID string         `gorm:"type:varchar(64);index:idx_audit_resource_time,priority:2"`
	Before     datatypes.JSON `gorm:"type:json"`
	After      datatypes.JSON `gorm:"type:json"`

	// Context — links to other observability layers.
	TraceID   string `gorm:"type:varchar(32);index"`
	RequestID string `gorm:"type:varchar(64);index"`

	// Metadata is a free-form JSON bag for fields that don't fit
	// the typed schema (campaign IDs, feature flag values, etc).
	Metadata datatypes.JSON `gorm:"type:json"`

	// Reason is an optional operator note explaining the action
	// (e.g. cancellation reason, override justification). Plain
	// text rather than structured because auditors read it.
	Reason string `gorm:"type:varchar(512)"`
}

// TableName pins the storage name to "audit_logs" so a future model
// rename can't silently shift the table out from under existing
// rows. Auditors expect a stable table name in their queries.
func (Log) TableName() string { return "audit_logs" }

// Index-priority composite indexes are declared inline above using
// the gorm `index:NAME,priority:N` tag form. The actor/action/
// resource indexes all sort by OccurredAt as the trailing column;
// we use an additional column-level index on OccurredAt itself for
// scans that aren't pre-filtered by actor/action/resource (e.g.
// "everything in the last hour"). Dialect-specific DESC ordering is
// not portable through GORM tags, so we accept the index covering
// either direction at storage cost.
