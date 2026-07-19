// Package edge hosts the compiled fixture models whose scan is EXPECTED
// to warn — the shapes where the generator and the GORM runtime cannot
// fully agree and the contract is an honest diagnostic instead of a
// generated symbol. The semantic latch (store/fieldgen_latch_test.go)
// builds real stores over these models and pins the exact divergence:
// a store-tagged relation is skipped on both sides, and promoted embeds
// exist at runtime but not in the generated surface. Keep the main
// fixture package warning-free; new expected-warning shapes go here.
package edge

import "github.com/zynthara/chok/v2/db"

// Profile is the belongs-to target for Contact — a plain struct, not a
// column, so a store tag on a field of this type is dead at runtime.
type Profile struct {
	ID   uint
	Nick string
}

// Badge has a Value method with the WRONG signature — not driver.Valuer
// (which the runtime type-asserts exactly), so a Badge field stays a
// relation (review round-2).
type Badge struct {
	ID uint
}

// Value is deliberately NOT driver.Valuer.
func (Badge) Value() (int, error) { return 0, nil }

// Contact carries store tags on relation fields: GORM parses Profile
// (a plain struct) and Badge (a struct whose Value method has the
// wrong signature) with empty DBNames, so the runtime whitelist never
// sees them; the generator must skip both with a warning rather than
// emit symbols that resolve to ErrUnknownField.
type Contact struct {
	db.Model
	ProfileID uint    `json:"profile_id" store:"query"`
	Profile   Profile `json:"profile" store:"query"`
	BadgeID   uint    `json:"badge_id" store:"query"`
	Badge     Badge   `json:"badge" store:"query"`
	Note      string  `json:"note" store:"query,update" gorm:"size:64"`
}

// Child is the has-many target for Parent.
type Child struct {
	ID       uint
	ParentID uint
}

// Children is a defined slice: GORM resolves it to a has-many relation
// — the defined-type chain must be followed to the underlying shape,
// not blessed as a scalar (review round-2).
type Children []Child

// Parent carries a store tag on the defined-slice relation.
type Parent struct {
	db.Model
	Children Children `json:"children" store:"query"`
	Note     string   `json:"note" store:"query" gorm:"size:64"`
}

// AuditBase is an exported local struct whose tags GORM promotes into
// any struct that embeds it.
type AuditBase struct {
	Actor string `json:"actor" store:"query" gorm:"size:64"`
}

// Event's whole tagged surface rides the promoted embed: at runtime it
// is a tag-declared model (query key "actor"), but the syntax-level
// scan cannot expand the embed. The contract is a promotion warning —
// staying silent here was the review round-1 failure mode.
type Event struct {
	db.Model
	AuditBase
}

// Audit is the gorm-embedded payload for Entry. It is itself a tagged
// top-level struct, so it also scans as a (base-less) model — the
// documented "harmless dead trio" tradeoff of skipping embed detection.
type Audit struct {
	By string `json:"by" store:"query" gorm:"size:64"`
}

// Entry mixes a direct tag with a gorm-embedded wrapper: the runtime
// promotes Audit's "by" into Entry's whitelist, the generated surface
// carries only Title — pinned as generated ⊂ runtime with the missing
// set exactly {"by"}, plus the promotion warning.
type Entry struct {
	db.Model
	Extra Audit  `gorm:"embedded"`
	Title string `json:"title" store:"query" gorm:"size:64"`
}

// hiddenAudit is the UNEXPORTED gorm-embedded target for Ticket: GORM
// promotes by field name (Extra is exported), so the type's
// exportedness is irrelevant at runtime (review round-2).
type hiddenAudit struct {
	Ref string `json:"ref" store:"query" gorm:"size:24"`
}

// Ticket must warn about Extra exactly like Entry does about its
// exported target — the runtime whitelist contains "ref" either way.
type Ticket struct {
	db.Model
	Extra   hiddenAudit `gorm:"embedded"`
	Subject string      `json:"subject" store:"query" gorm:"size:64"`
}
