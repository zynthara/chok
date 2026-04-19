package parts

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/db"
)

type dbTestUser struct {
	db.Model
	Name string `gorm:"size:100"`
}

func (dbTestUser) RIDPrefix() string { return "usr" }

func sqliteBuilder(_ component.Kernel) (*gorm.DB, error) {
	return gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
}

func TestDBComponent_Init_Migrate_Close(t *testing.T) {
	c := NewDBComponent(sqliteBuilder, db.Table(&dbTestUser{}))

	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if c.DB() == nil {
		t.Fatal("DB() should not be nil after Init")
	}

	if err := c.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Verify the table actually exists: an insert should succeed.
	if err := c.DB().Create(&dbTestUser{Name: "alice"}).Error; err != nil {
		t.Fatalf("migrate did not create table: %v", err)
	}

	s := c.Health(context.Background())
	if s.Status != component.HealthOK {
		t.Fatalf("sqlite in-memory should be healthy, got %q (%s)", s.Status, s.Error)
	}

	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDBComponent_BuilderError(t *testing.T) {
	c := NewDBComponent(func(component.Kernel) (*gorm.DB, error) {
		return nil, gorm.ErrInvalidDB
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err == nil {
		t.Fatal("expected builder error to propagate")
	}
}

func TestDBComponent_NilBuilderResult(t *testing.T) {
	c := NewDBComponent(func(component.Kernel) (*gorm.DB, error) { return nil, nil })
	err := c.Init(context.Background(), newMockKernel(nil))
	if err == nil {
		t.Fatal("nil *gorm.DB should be rejected")
	}
}

func TestDBComponent_Migrate_NoTables(t *testing.T) {
	c := NewDBComponent(sqliteBuilder) // no tables
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate with no tables should be a no-op, got %v", err)
	}
	_ = c.Close(context.Background())
}
