package db

import (
	"fmt"
	"reflect"
	"time"

	"gorm.io/gorm"
)

// Modeler is satisfied only by the four model bases, through an
// unexported method: embed one of them to make a struct a model.
type Modeler interface{ chokModel() }

// RIDPrefixer gives a model's RIDs a prefix: posts as pst_xxx.
type RIDPrefixer interface{ RIDPrefix() string }

// Model is the base of a model: a numeric key that stays internal, a
// public RID (json "id"), an optimistic-lock version and timestamps.
type Model struct {
	ID        uint      `json:"-" gorm:"primaryKey"`
	RID       string    `json:"id" gorm:"column:rid;uniqueIndex;size:24;not null"`
	Version   int       `json:"version" gorm:"default:1;not null"`
	CreatedAt time.Time `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt time.Time `json:"updated_at" gorm:"autoUpdateTime"`
}

func (Model) chokModel() {}

// SoftDeleteModel is Model with soft delete: a deleted row keeps its data,
// gets DeletedAt, and a random DeleteToken that frees it from the
// SoftUnique indexes so a live row may reuse the values.
type SoftDeleteModel struct {
	Model
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
	DeleteToken string         `json:"-" gorm:"column:delete_token;size:24;not null;default:''"`
}

// OwnedModel is Model whose rows belong to a principal: a store fences
// every query to the caller's rows and stamps OwnerID on create.
type OwnedModel struct {
	Model
	OwnerID string `json:"-" gorm:"column:owner_id;index;size:128;not null"`
}

// OwnedSoftDeleteModel is SoftDeleteModel with an owner.
type OwnedSoftDeleteModel struct {
	SoftDeleteModel
	OwnerID string `json:"-" gorm:"column:owner_id;index;size:128;not null"`
}

// GetOwnerID returns the owner.
func (m *OwnedModel) GetOwnerID() string { return m.OwnerID }

// SetOwnerID sets the owner.
func (m *OwnedModel) SetOwnerID(id string) { m.OwnerID = id }

// GetOwnerID returns the owner.
func (m *OwnedSoftDeleteModel) GetOwnerID() string { return m.OwnerID }

// SetOwnerID sets the owner.
func (m *OwnedSoftDeleteModel) SetOwnerID(id string) { m.OwnerID = id }

// BeforeCreate starts the version at 1 and assigns a RID with the model's
// prefix unless one is set; a preset RID must be well formed.
func (m *Model) BeforeCreate(tx *gorm.DB) error {
	if m.Version == 0 {
		m.Version = 1
	}
	if m.RID != "" {
		if err := validateRID(m.RID); err != nil {
			return fmt.Errorf("db: create with RID %q: %w", m.RID, err)
		}
		return nil
	}
	prefix, err := prefixOf(tx.Statement.Dest)
	if err != nil {
		return err
	}
	if prefix == "" {
		m.RID = randBase62(ridIDLen)
	} else {
		m.RID = prefix + "_" + randBase62(ridIDLen)
	}
	return nil
}

// prefixOf finds the RIDPrefixer behind a create's destination: a model,
// a pointer to one, or a slice of either.
func prefixOf(dest any) (string, error) {
	if dest == nil {
		return "", nil
	}
	if p, ok := dest.(RIDPrefixer); ok {
		return p.RIDPrefix(), validatePrefix(p.RIDPrefix())
	}
	t := reflect.TypeOf(dest)
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if p, ok := reflect.New(t).Interface().(RIDPrefixer); ok {
		return p.RIDPrefix(), validatePrefix(p.RIDPrefix())
	}
	return "", nil
}

var softDeleteType = reflect.TypeFor[SoftDeleteModel]()

// embeds reports whether struct t embeds base at any depth.
func embeds(t reflect.Type, base reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == base {
		return true
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	for f := range t.Fields() {
		// gorm maps only exported embedded structs to columns.
		if f.Anonymous && f.IsExported() && embeds(f.Type, base) {
			return true
		}
	}
	return false
}

func isSoftDelete(t reflect.Type) bool { return embeds(t, softDeleteType) }
