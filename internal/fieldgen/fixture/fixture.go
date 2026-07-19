// Package fixture hosts the compiled models behind the fieldgen tests:
// the golden test (internal/fieldgen) regenerates chok_fields_gen.go from
// this source and byte-compares it, and the semantic latch
// (store/fieldgen_latch_test.go) builds real stores over these models to
// assert every generated value resolves through the runtime whitelists.
// The structs deliberately cover every public-name derivation path the
// generator replicates from store.tagDeclaredFields: JSON names with
// comma options, json:"-" and missing-tag DBName fallbacks, explicit
// gorm columns, acronym snake-casing, and a user field taking the base
// "id" key.
package fixture

import "github.com/zynthara/chok/v2/db"

// Article exercises the naming fallbacks on the full owned+soft-delete
// base, so the latch test sees the widest protected-column set.
type Article struct {
	db.OwnedSoftDeleteModel
	Title        string `json:"title,omitempty" store:"query,update" gorm:"size:200;not null"`
	Body         string `json:"body" store:"update" gorm:"type:text"`
	Secret       string `json:"-" store:"query" gorm:"size:64"`
	InternalNote string `store:"query" gorm:"size:64"`
	HTTPStatus   int    `json:"-" store:"query"`
	LegacyBody   string `store:"update" gorm:"column:body_raw;type:text"`
	Aliased      string `json:"aliased" store:"query" gorm:"size:64"`
	Plain        string `json:"plain"`
}

// ShadowID declares its own public "id" key: the base-model trio must
// skip the taken name (one symbol per public name), matching
// store.tagDeclaredFields.
type ShadowID struct {
	db.Model
	PublicID string `json:"id" store:"query" gorm:"column:public_id;size:24"`
	Name     string `json:"name" store:"query,update" gorm:"size:64"`
}
