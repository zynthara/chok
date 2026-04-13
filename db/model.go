package db

import (
	"fmt"
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
//  1. Sets Version = 1 so the in-memory object matches the DB row.
//  2. Auto-generates a RID if the model implements RIDPrefixer.
//     Validates the prefix independently so the hook is self-sufficient
//     even when bypassing store.New / db.Table.
func (m *Model) BeforeCreate(tx *gorm.DB) error {
	m.Version = 1
	if m.RID == "" {
		dest := tx.Statement.Dest
		if p, ok := dest.(RIDPrefixer); ok {
			prefix := p.RIDPrefix()
			if err := rid.ValidatePrefix(prefix, 12); err != nil {
				return fmt.Errorf("db: BeforeCreate: %w", err)
			}
			m.RID = prefix + "_" + rid.NewRaw()
		} else {
			m.RID = rid.NewRaw()
		}
	}
	return nil
}
