package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
)

func TestRestore_RoundTrip(t *testing.T) {
	s, gdb := setupUserStore(t)
	ctx := context.Background()

	u := &User{Name: "alice", Email: "alice@example.com"}
	if err := s.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, RID(u.RID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("soft-deleted row must be invisible, got %v", err)
	}

	if err := s.Restore(ctx, RID(u.RID)); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := s.Get(ctx, RID(u.RID))
	if err != nil {
		t.Fatalf("restored row must be visible: %v", err)
	}
	if got.Email != "alice@example.com" {
		t.Fatalf("restored row lost data: %+v", got)
	}
	if got.Version != 3 {
		t.Fatalf("soft delete and restore must each advance the row revision, got %d", got.Version)
	}
	// The SoftUnique slot only frees up when delete_token returns to
	// the live sentinel — assert the column, not just visibility.
	var token string
	if err := gdb.Unsafe(ctx).Model(&User{}).Where("rid = ?", u.RID).
		Pluck("delete_token", &token).Error; err != nil {
		t.Fatal(err)
	}
	if token != "" {
		t.Fatalf("delete_token must return to the live sentinel, got %q", token)
	}
}

func TestRestore_SoftUniqueSlotTaken_Duplicate(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()

	first := &User{Name: "alice", Email: "alice@example.com"}
	if err := s.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, RID(first.RID)); err != nil {
		t.Fatal(err)
	}
	// A new live row claims the email slot the soft delete released.
	second := &User{Name: "alice2", Email: "alice@example.com"}
	if err := s.Create(ctx, second); err != nil {
		t.Fatal(err)
	}

	err := s.Restore(ctx, RID(first.RID))
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("restoring into a taken SoftUnique slot must map to ErrDuplicate, got %v", err)
	}
	// The loser stays deleted; the live row is untouched.
	if _, err := s.Get(ctx, RID(first.RID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed restore must leave the row deleted, got %v", err)
	}
	if _, err := s.Get(ctx, RID(second.RID)); err != nil {
		t.Fatalf("live row must survive the failed restore: %v", err)
	}
}

func TestRestore_AliveRow_IdempotentNil(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()

	u := &User{Name: "bob", Email: "bob@example.com"}
	if err := s.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(ctx, RID(u.RID)); err != nil {
		t.Fatalf("restoring a live row mirrors Delete's idempotence, got %v", err)
	}
}

func TestRestore_MissingRow_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	if err := s.Restore(context.Background(), RID("usr_nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restoring an absent row must be ErrNotFound, got %v", err)
	}
}

func TestRestore_NonSoftModel_Error(t *testing.T) {
	s := setupItemStore(t)
	err := s.Restore(context.Background(), RID("itm_x"))
	if err == nil || !strings.Contains(err.Error(), "not a soft-delete model") {
		t.Fatalf("hard-delete model must reject Restore with guidance, got %v", err)
	}
}

// restoreDoc gives the owner-scope branch a model that is both owned
// and soft-deletable (Product is owned but hard-delete).
type restoreDoc struct {
	db.OwnedSoftDeleteModel
	Title string `json:"title" store:"query,update" gorm:"size:100"`
}

func (restoreDoc) RIDPrefix() string { return "rdc" }

func TestRestore_OwnerScope_ForeignRowNotFound(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&restoreDoc{})); err != nil {
		t.Fatal(err)
	}
	s := New[restoreDoc](gdb, log.Empty())

	alice, bob := userCtx("usr_alice"), userCtx("usr_bob")
	doc := &restoreDoc{Title: "mine"}
	if err := s.Create(alice, doc); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(alice, RID(doc.RID)); err != nil {
		t.Fatal(err)
	}

	// A foreign principal can neither restore the row nor learn that
	// it exists.
	if err := s.Restore(bob, RID(doc.RID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign deleted row must read as absent, got %v", err)
	}
	// The owner restores it fine.
	if err := s.Restore(alice, RID(doc.RID)); err != nil {
		t.Fatalf("owner restore: %v", err)
	}
	if _, err := s.Get(alice, RID(doc.RID)); err != nil {
		t.Fatalf("owner must see the restored row: %v", err)
	}
}

func TestRestore_PublishesOpRestore_NotOnNoop(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&User{}, db.SoftUnique("uk_email_ev", "email"))); err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus()
	t.Cleanup(func() { bus.Close(context.Background()) })
	var seen []EntityChanged[User]
	event.Subscribe(bus, func(_ context.Context, ev EntityChanged[User]) {
		seen = append(seen, ev)
	}, event.WithSync())
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "email"),
		WithUpdateFields("name"),
		WithBus(bus),
	)
	ctx := context.Background()

	u := &User{Name: "eve", Email: "eve@example.com"}
	if err := s.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	seen = nil

	if err := s.Restore(ctx, RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Op != OpRestore {
		t.Fatalf("restore must publish exactly one OpRestore, got %+v", seen)
	}

	// Idempotent no-op restore must not publish — mirroring Delete's
	// rows-affected gate.
	seen = nil
	if err := s.Restore(ctx, RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Fatalf("no-op restore must not publish events, got %+v", seen)
	}
}
