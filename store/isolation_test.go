package store_test

import (
	"context"
	"testing"

	"github.com/zynthara/chok/choktest"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/store"
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
