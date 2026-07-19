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
	"database/sql"
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
// struct a DataType (hence a DBName) from the method, so it is a
// column as a NAMED field (anonymous is the opposite — see the edge
// package's Player).
type Flags struct {
	V uint8
}

// GormDataType implements the GORM data-type hook.
func (Flags) GormDataType() string { return "smallint" }

// Box embeds Money: Go promotes Value into Box's method set, so Box is
// itself a driver.Valuer column (review round-3).
type Box struct {
	Money
}

// DV aliases the interface's result type: a signature written through
// it is still an exact driver.Valuer implementation (review round-4).
type DV = driver.Value

// Sealed implements driver.Valuer via the alias.
type Sealed struct {
	S string
}

// Value implements driver.Valuer through the DV alias.
func (s Sealed) Value() (DV, error) { return s.S, nil }

// Clock returns the literal "time" from GormDataType — one of the two
// values GORM's anonymous-embed rule exempts, so even as an EMBED it
// stays a column (review round-4).
type Clock struct {
	Unix int64
}

// GormDataType implements the GORM data-type hook with the exempt
// literal.
func (Clock) GormDataType() string { return "time" }

// Bytes is a generic defined type: instantiated with byte it IS []byte,
// a bytes column (review round-4).
type Bytes[T any] []T

// Stamp is a defined type over time.Time: methods are lost but
// reflect convertibility survives, and GORM maps it to a time column
// (review round-3).
type Stamp time.Time

// Token is a fixed byte array — GORM maps arrays of byte to a bytes
// column exactly like slices (review round-3).
type Token [16]byte

// Null aliases a cross-package driver.Valuer: aliases keep full type
// identity, so embedding Null embeds sql.NullString itself and Go
// promotes its Value method (review round-5).
type Null = sql.NullString

// Vault embeds the alias: the promoted NullString.Value makes Vault a
// driver.Valuer column exactly like Box.
type Vault struct {
	Null
}

// Byte aliases the predeclared byte: reflect sees uint8 straight
// through it, so element positions built from the alias keep the
// bytes-column identity (review round-5).
type Byte = byte

// Strip is a defined slice whose element rides the alias — underlying
// []uint8, a bytes column.
type Strip []Byte

// Wallet pins the method-proven and shape-proven column forms
// (review rounds 2–5): an anonymous driver.Valuer embed, a
// GormDataType struct field, an embed-promoted Valuer, a defined
// time.Time, a byte array, the `gorm:"json"` serializer shorthand,
// and the round-5 identity chains — an alias-embedded cross-package
// Valuer, byte aliases in element positions, and an anonymously
// embedded generic instantiation.
type Wallet struct {
	db.Model
	Money       `json:"money" store:"query"`
	Clock       `json:"clock" store:"query"`
	Bytes[byte] `json:"chunk" store:"query"`
	Flags       Flags          `json:"flags" store:"query,update"`
	Box         Box            `json:"box" store:"query"`
	Seal        Stamp          `json:"seal" store:"query"`
	Token       Token          `json:"token" store:"query"`
	Meta        map[string]any `json:"meta" store:"query" gorm:"json"`
	Locker      Sealed         `json:"locker" store:"query"`
	Payload     Bytes[byte]    `json:"payload" store:"query"`
	Vault       Vault          `json:"vault" store:"query"`
	Strip       Strip          `json:"strip" store:"query"`
	Slab        []Byte         `json:"slab" store:"query"`
	Packed      Bytes[Byte]    `json:"packed" store:"query"`
}
