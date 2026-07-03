package choktest

import (
	"context"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store"
)

// testModel is a minimal model for testing the test helpers.
type testModel struct {
	db.Model
	Name string `json:"name" gorm:"column:name"`
}

func (testModel) RIDPrefix() string { return "tst" }
func (testModel) TableName() string { return "test_models" }

func TestNewTestDB_CreatesAndMigrates(t *testing.T) {
	h := NewTestDB(t, &testModel{})

	// Verify table exists by inserting a record — raw access goes
	// through the handle's escape hatch (v2: NewTestDB returns *db.DB).
	ctx := context.Background()
	if err := h.Unsafe(ctx).Create(&testModel{Name: "hello"}).Error; err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	var count int64
	h.Unsafe(ctx).Model(&testModel{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

func TestNewTestStore_CRUD(t *testing.T) {
	s := NewTestStore[testModel](t,
		store.WithQueryFields("name"),
		store.WithUpdateFields("name"),
	)

	ctx := context.Background()
	obj := &testModel{Name: "Alice"}
	if err := s.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	if obj.RID == "" {
		t.Fatal("RID should be set after create")
	}

	got, err := s.Get(ctx, store.RID(obj.RID))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Alice" {
		t.Fatalf("expected Alice, got %q", got.Name)
	}
}

func TestNopLogger_DoesNotPanic(t *testing.T) {
	l := NopLogger()
	l.Info("should not panic")
	l.Error("should not panic")
}
