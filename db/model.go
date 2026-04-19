package db

import (
	"fmt"
	"reflect"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/rid"
)

// RIDPrefixer — models implementing this get auto-generated prefixed RIDs.
type RIDPrefixer interface {
	RIDPrefix() string
}

// Modeler is the generic constraint for store.Store[T].
// The unexported marker method ensures only types embedding db.Model satisfy it.
type Modeler interface {
	chokModel()
}

// Model is the base model with auto-increment PK, RID, optimistic lock, and timestamps.
type Model struct {
	ID        uint      `json:"-"          gorm:"primaryKey"`
	RID       string    `json:"id"         gorm:"column:rid;uniqueIndex;size:24;not null"`
	Version   int       `json:"version"    gorm:"default:1;not null"`
	CreatedAt time.Time `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt time.Time `json:"updated_at" gorm:"autoUpdateTime"`
}

func (Model) chokModel() {}

// SoftDeleteModel embeds Model and adds soft-delete support (opt-in).
type SoftDeleteModel struct {
	Model
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
	DeleteToken string         `json:"-" gorm:"column:delete_token;default:'';not null;size:24"`
}

// OwnedModel embeds Model and Owned — use when both ownership and
// base model fields are needed (the common case).
type OwnedModel struct {
	Model
	Owned
}

// OwnedSoftDeleteModel embeds SoftDeleteModel and Owned — use when
// ownership + soft-delete are both needed.
type OwnedSoftDeleteModel struct {
	SoftDeleteModel
	Owned
}

// OwnerAccessor is implemented by models that track resource ownership.
// Store uses this to auto-fill OwnerID on create and auto-scope queries.
type OwnerAccessor interface {
	GetOwnerID() string
	SetOwnerID(id string)
}

// Owned is a low-level mixin that adds ownership tracking.
// Prefer OwnedModel or OwnedSoftDeleteModel for convenience.
// Use Owned directly only when composing with custom base structs.
type Owned struct {
	OwnerID string `json:"-" gorm:"column:owner_id;index;not null;size:128"`
}

// GetOwnerID returns the owner's identifier.
func (o *Owned) GetOwnerID() string { return o.OwnerID }

// SetOwnerID sets the owner's identifier.
func (o *Owned) SetOwnerID(id string) { o.OwnerID = id }

// BeforeCreate is a GORM hook that:
//  1. Initialises Version to 1 when unset, so the in-memory object
//     matches the DB row. A caller-provided Version (e.g. data import or
//     restore) is preserved — we only fill the zero value.
//  2. Auto-generates a RID if the model implements RIDPrefixer. The
//     prefix probe works for both single-object Create and batch inserts
//     (CreateInBatches / slice Create) — for slices, the prefix is
//     resolved from the element type rather than the slice itself.
//     Validates the prefix independently so the hook is self-sufficient
//     even when bypassing store.New / db.Table.
func (m *Model) BeforeCreate(tx *gorm.DB) error {
	if m.Version == 0 {
		m.Version = 1
	}
	if m.RID == "" {
		prefix, err := ridPrefixFromDest(tx.Statement.Dest)
		if err != nil {
			return err
		}
		if prefix != "" {
			m.RID = prefix + "_" + rid.NewRaw()
		} else {
			m.RID = rid.NewRaw()
		}
		return nil
	}
	// Caller-supplied RID (data import, restore, testing): validate
	// shape so a malformed or over-length value can't slip past and
	// produce a DB-level error further down the call stack.
	if err := rid.ValidateRID(m.RID); err != nil {
		return fmt.Errorf("db: BeforeCreate: invalid RID %q: %w", m.RID, err)
	}
	return nil
}

// ridPrefixFromDest extracts the RIDPrefix from a GORM statement Dest.
// Handles three shapes:
//   - Dest is *T implementing RIDPrefixer (single-object Create).
//   - Dest is *[]T or []T where T implements RIDPrefixer (batch Create).
//   - Dest is anything else — returns empty prefix (fallback to unprefixed RID).
//
// Returns an error only when the resolved prefix is syntactically invalid.
func ridPrefixFromDest(dest any) (string, error) {
	if dest == nil {
		return "", nil
	}
	if p, ok := dest.(RIDPrefixer); ok {
		return validatedPrefix(p)
	}
	rv := reflect.ValueOf(dest)
	for rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Slice {
		return "", nil
	}
	et := rv.Type().Elem()
	for et.Kind() == reflect.Ptr {
		et = et.Elem()
	}
	probe := reflect.New(et).Interface()
	if p, ok := probe.(RIDPrefixer); ok {
		return validatedPrefix(p)
	}
	return "", nil
}

func validatedPrefix(p RIDPrefixer) (string, error) {
	prefix := p.RIDPrefix()
	if err := rid.ValidatePrefix(prefix, 12); err != nil {
		return "", fmt.Errorf("db: BeforeCreate: %w", err)
	}
	return prefix, nil
}
