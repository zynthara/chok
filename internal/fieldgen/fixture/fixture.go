// Package fixture hosts the compiled models behind the fieldgen tests:
// the golden test (internal/fieldgen) regenerates chok_fields_gen.go from
// this source and byte-compares it, and the semantic latch
// (store/fieldgen_latch_test.go) builds real stores over these models to
// assert every generated value resolves through the runtime whitelists.
// The structs deliberately cover every public-name derivation path the
// generator replicates from store.tagDeclaredFields — JSON names with
// comma options, json:"-" and missing-tag DBName fallbacks, explicit
// gorm columns, acronym snake-casing, a user field taking the base "id"
// key — plus the column-shape paths review round-1 added: an anonymous
// scalar embed, a known cross-package column type (time.Time) and a
// local defined scalar. This package must scan warning-free; the
// expected-warning shapes live in the sibling edge package.
package fixture

import (
	"database/sql/driver"
	"time"

	"github.com/zynthara/chok/v2/db"
)

// Code is a local defined scalar: GORM maps it by kind, both as an
// anonymous embed (a regular column named after the type) and as a
// named field's type.
type Code string

// Article exercises the naming fallbacks on the full owned+soft-delete
// base, so the latch test sees the widest protected-column set.
type Article struct {
	db.OwnedSoftDeleteModel
	Code         `json:"code" store:"query"`
	Title        string    `json:"title,omitempty" store:"query,update" gorm:"size:200;not null"`
	Body         string    `json:"body" store:"update" gorm:"type:text"`
	Secret       string    `json:"-" store:"query" gorm:"size:64"`
	InternalNote string    `store:"query" gorm:"size:64"`
	HTTPStatus   int       `json:"-" store:"query"`
	LegacyBody   string    `store:"update" gorm:"column:body_raw;type:text"`
	Aliased      string    `json:"aliased" store:"query" gorm:"size:64"`
	PublishedAt  time.Time `json:"published_at" store:"query"`
	Plain        string    `json:"plain"`
}

// ShadowID declares its own public "id" key: the base-model trio must
// skip the taken name (one symbol per public name), matching
// store.tagDeclaredFields.
type ShadowID struct {
	db.Model
	PublicID string `json:"id" store:"query" gorm:"column:public_id;size:24"`
	Name     string `json:"name" store:"query,update" gorm:"size:64"`
	Kind     Code   `json:"kind" store:"query" gorm:"size:16"`
}

// Money implements driver.Valuer with the exact interface signature —
// a struct-shaped column, both embedded and named.
type Money struct {
	Cents int64
}

// Value implements driver.Valuer.
func (m Money) Value() (driver.Value, error) { return m.Cents, nil }

// Flags carries its column type via GormDataType — GORM assigns the
// struct a DataType (hence a DBName) from the method.
type Flags struct {
	V uint8
}

// GormDataType implements the GORM data-type hook.
func (Flags) GormDataType() string { return "smallint" }

// Wallet pins the method-proven column shapes (review round-2): an
// anonymous driver.Valuer embed and a GormDataType struct field are
// real runtime columns the classifier must include.
type Wallet struct {
	db.Model
	Money `json:"money" store:"query"`
	Flags Flags `json:"flags" store:"query,update"`
}
