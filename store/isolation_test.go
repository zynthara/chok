package store_test

import (
	"context"
	"testing"

	"github.com/zynthara/chok/choktest"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

type ArchitectureIndexModel struct {
	db.SoftDeleteModel
	A string `json:"a" store:"query,update" gorm:"size:100;not null"`
	B string `json:"b" store:"query,update" gorm:"size:100;not null"`
}

func TestSoftUnique_MetadataBelongsToHandle(t *testing.T) {
	d1 := choktest.DB(t, db.Table(&ArchitectureIndexModel{}, db.SoftUnique("arch_a", "a")))
	s := store.New[ArchitectureIndexModel](d1)
	ctx := context.Background()
	if err := s.Upsert(ctx, &ArchitectureIndexModel{A: "x", B: "y"}, "a"); err != nil {
		t.Fatal(err)
	}
	_ = choktest.DB(t, db.Table(&ArchitectureIndexModel{}, db.SoftUnique("arch_b", "b")))
	err := s.Upsert(ctx, &ArchitectureIndexModel{A: "x", B: "z"}, "a")
	if err != nil {
		t.Fatalf("second database changed the first store: %v", err)
	}

}

// TestUnique_MetadataBelongsToHandle: an index's kind belongs to the
// handle too. The second database declares the same index as a
// SoftUnique; the first store's conflict target stays UNIQUE(a), which
// SQLite and PostgreSQL would refuse with delete_token added.
func TestUnique_MetadataBelongsToHandle(t *testing.T) {
	d1 := choktest.DB(t, db.Table(&ArchitectureIndexModel{}, db.Unique("arch_u", "a")))
	s := store.New[ArchitectureIndexModel](d1)
	ctx := t.Context()
	if err := s.Upsert(ctx, &ArchitectureIndexModel{A: "x", B: "y"}, "a"); err != nil {
		t.Fatal(err)
	}
	_ = choktest.DB(t, db.Table(&ArchitectureIndexModel{}, db.SoftUnique("arch_u", "a")))
	if err := s.Upsert(ctx, &ArchitectureIndexModel{A: "x", B: "z"}, "a"); err != nil {
		t.Fatalf("second database changed the first store: %v", err)
	}
	if got, err := s.Get(ctx, store.Where(where.Eq("a", "x"))); err != nil || got.B != "z" || got.Version != 2 {
		t.Fatalf("the upsert updates the row: %+v %v", got, err)
	}
}
