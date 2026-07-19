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

// Contact carries a store tag on a relation field: GORM parses Profile
// with an empty DBName so the runtime whitelist never sees "profile";
// the generator must skip it with a warning rather than emit a symbol
// that resolves to ErrUnknownField.
type Contact struct {
	db.Model
	ProfileID uint    `json:"profile_id" store:"query"`
	Profile   Profile `json:"profile" store:"query"`
	Note      string  `json:"note" store:"query,update" gorm:"size:64"`
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
