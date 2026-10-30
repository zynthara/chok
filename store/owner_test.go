package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

func docTitles(t *testing.T, ctx context.Context, s *store.Store[doc]) []string {
	t.Helper()
	page, err := s.List(ctx, where.Order("title"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range page.Items {
		out = append(out, d.Title)
	}
	return out
}

func TestOwner_CreateStampsThePrincipal(t *testing.T) {
	docs := store.New[doc](openDB(t), store.Policy(db.StorePolicy{AdminRoles: []string{"admin"}}))
	ctx := t.Context()
	d := &doc{Title: "mine"}
	mustCreate(t, as(ctx, "alice"), docs, d)
	if d.OwnerID != "alice" {
		t.Fatalf("owner %q", d.OwnerID)
	}
	batch := []*doc{{Title: "b1"}, {Title: "b2"}}
	if err := docs.BatchCreate(as(ctx, "bob"), batch); err != nil || batch[0].OwnerID != "bob" || batch[1].OwnerID != "bob" {
		t.Fatalf("batch owners %+v %v", batch, err)
	}
	// A non-admin cannot write as someone else; an admin can.
	forged := &doc{Title: "forged"}
	forged.OwnerID = "alice"
	mustCreate(t, as(ctx, "mallory"), docs, forged)
	if forged.OwnerID != "mallory" {
		t.Fatalf("a non-admin's preset owner is overwritten: %q", forged.OwnerID)
	}
	onBehalf := &doc{Title: "for carol"}
	onBehalf.OwnerID = "carol"
	mustCreate(t, as(ctx, "root", "admin"), docs, onBehalf)
	if onBehalf.OwnerID != "carol" {
		t.Fatalf("an admin's preset owner stays: %q", onBehalf.OwnerID)
	}
	// Without a principal: code that sets the owner may write (a job), a
	// row without one is refused.
	job := &doc{Title: "from a job"}
	job.OwnerID = "dave"
	mustCreate(t, ctx, docs, job)
	err := docs.Create(ctx, &doc{Title: "orphan"})
	if !errors.As(err, new(*store.NoPrincipalError)) || !errors.Is(err, chok.ErrUnauthorized) {
		t.Fatalf("orphan row: %v", err)
	}
}

func TestOwner_RequirePrincipal(t *testing.T) {
	cfg := dbtest.Config(t)
	cfg.Store = db.StorePolicy{RequirePrincipal: true}
	d := dbtest.OpenConfig(t, cfg, tables())
	docs := store.New[doc](d)
	job := &doc{Title: "job"}
	job.OwnerID = "dave"
	if err := docs.Create(t.Context(), job); !errors.As(err, new(*store.NoPrincipalError)) {
		t.Fatalf("require_principal refuses a preset owner without a principal: %v", err)
	}
	if err := docs.BatchCreate(t.Context(), []*doc{job}); !errors.Is(err, chok.ErrUnauthorized) {
		t.Fatalf("batch: %v", err)
	}
	// Models without an owner are not affected.
	if err := store.New[post](d).Create(t.Context(), &post{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	// Policy replaces the handle's posture, require_principal included.
	lenient := store.New[doc](d, store.Policy(db.StorePolicy{}))
	if err := lenient.Create(t.Context(), job); err != nil || job.OwnerID != "dave" {
		t.Fatalf("a store without require_principal writes the preset owner: %v", err)
	}
	if err := lenient.Create(t.Context(), &doc{Title: "orphan"}); !errors.Is(err, chok.ErrUnauthorized) {
		t.Fatalf("still no row without an owner: %v", err)
	}
}

func TestOwner_ScopeFencesReadsAndWrites(t *testing.T) {
	docs := store.New[doc](openDB(t), store.Policy(db.StorePolicy{AdminRoles: []string{"admin"}}))
	ctx := t.Context()
	alice, bob := as(ctx, "alice"), as(ctx, "bob")
	a := &doc{Title: "a1"}
	mustCreate(t, alice, docs, a, &doc{Title: "a2"})
	mustCreate(t, bob, docs, &doc{Title: "b1"})

	if got := docTitles(t, alice, docs); !slices.Equal(got, []string{"a1", "a2"}) {
		t.Fatalf("alice sees %v", got)
	}
	if got := docTitles(t, as(ctx, "root", "admin"), docs); len(got) != 3 {
		t.Fatalf("an admin sees all: %v", got)
	}
	for user, want := range map[string]int64{"alice": 2, "bob": 1, "carol": 0} {
		if n, err := docs.Count(as(ctx, user)); err != nil || n != want {
			t.Errorf("%s counts %d, %v", user, n, err)
		}
	}
	if ok, _ := docs.Exists(bob, store.RID(a.RID)); ok {
		t.Fatal("bob sees alice's row exist")
	}
	// No principal: reads are empty, writes fail closed.
	if got := docTitles(t, ctx, docs); len(got) != 0 {
		t.Fatalf("anonymous sees %v", got)
	}
	if n, err := docs.Count(ctx); n != 0 || err != nil {
		t.Fatalf("anonymous count %d %v", n, err)
	}
	if _, err := docs.Get(ctx, store.RID(a.RID)); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("anonymous get: %v", err)
	}
	for name, err := range map[string]error{
		"update":  docs.Update(ctx, store.RID(a.RID), store.Set(map[string]any{"title": "x"})),
		"delete":  docs.Delete(ctx, store.RID(a.RID)),
		"restore": docs.Restore(ctx, store.RID(a.RID)),
	} {
		if !errors.As(err, new(*store.NoPrincipalError)) {
			t.Errorf("anonymous %s: %v", name, err)
		}
	}
	// Bob cannot see, change or delete alice's row.
	if _, err := docs.Get(bob, store.RID(a.RID)); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("bob reads alice's: %v", err)
	}
	if err := docs.Update(bob, store.RID(a.RID), store.Set(map[string]any{"title": "pwned"})); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("bob updates alice's: %v", err)
	}
	if err := docs.Update(bob, store.RID(a.RID), store.Set(map[string]any{"title": "pwned"}), where.Version(1)); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("a locked update of a foreign row is not found, not a conflict: %v", err)
	}
	if err := docs.Delete(bob, store.RID(a.RID)); err != nil {
		t.Fatal(err)
	}
	if got, err := docs.Get(alice, store.RID(a.RID)); err != nil || got.Title != "a1" {
		t.Fatalf("alice's row survives bob: %+v %v", got, err)
	}
	if err := docs.Delete(alice, store.RID(a.RID)); err != nil {
		t.Fatal(err)
	}
	if err := docs.Restore(bob, store.RID(a.RID)); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("bob restores alice's: %v", err)
	}
	if err := docs.Restore(alice, store.RID(a.RID)); err != nil {
		t.Fatal(err)
	}
	// The same title is fine for another owner: the index includes owner_id.
	mustCreate(t, bob, docs, &doc{Title: "a1"})
}

func TestOwner_AdminRolesComeFromThePolicy(t *testing.T) {
	cfg := dbtest.Config(t)
	cfg.Store = db.StorePolicy{AdminRoles: []string{"ops"}}
	d := dbtest.OpenConfig(t, cfg, tables())
	ctx := t.Context()
	inherited := store.New[doc](d)
	mustCreate(t, as(ctx, "alice"), inherited, &doc{Title: "a"})
	if got := docTitles(t, as(ctx, "x", "ops"), inherited); len(got) != 1 {
		t.Fatalf("the handle's admin role bypasses: %v", got)
	}
	if got := docTitles(t, as(ctx, "x", "admin"), inherited); len(got) != 0 {
		t.Fatalf("admin is not special: %v", got)
	}
	none := store.New[doc](d, store.Policy(db.StorePolicy{}))
	if got := docTitles(t, as(ctx, "x", "ops"), none); len(got) != 0 {
		t.Fatalf("Policy replaces the handle's: %v", got)
	}
	// Without admin roles nobody may write as someone else either.
	preset := &doc{Title: "for carol"}
	preset.OwnerID = "carol"
	mustCreate(t, as(ctx, "root", "ops", "admin"), none, preset)
	if preset.OwnerID != "root" {
		t.Fatalf("no admin role, no preset owner: %q", preset.OwnerID)
	}
	off := false
	open := store.New[doc](d, store.Policy(db.StorePolicy{OwnerScope: &off}))
	if got := docTitles(t, ctx, open); len(got) != 2 {
		t.Fatalf("owner_scope false removes the fence: %v", got)
	}
	if err := open.Create(ctx, &doc{Title: "unowned"}); err != nil {
		t.Fatalf("without the fence no principal is needed: %v", err)
	}
}

func TestScope_NarrowsEveryQuery(t *testing.T) {
	d := openDB(t)
	ctx := t.Context()
	all := store.New[post](d)
	mustCreate(t, ctx, all, &post{Title: "pub", Status: "published"}, &post{Title: "draft"})
	published := store.New[post](d, store.Scope(func(_ context.Context, tx *gorm.DB) (*gorm.DB, error) {
		return tx.Where("status = ?", "published"), nil
	}))
	page, _ := published.List(ctx)
	if got := titles(page.Items); !slices.Equal(got, []string{"pub"}) {
		t.Fatalf("scoped list %v", got)
	}
	if _, err := published.Get(ctx, store.Where(where.Eq("title", "draft"))); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("scoped get: %v", err)
	}
	if err := published.Update(ctx, store.Where(where.Eq("title", "draft")), store.Set(map[string]any{"views": 1})); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("scoped update: %v", err)
	}
	if err := published.Delete(ctx, store.Where(where.Eq("title", "draft"))); err != nil {
		t.Fatal(err)
	}
	if n, _ := all.Count(ctx); n != 2 {
		t.Fatalf("the out-of-scope row survives: %d", n)
	}
	boom := errors.New("scope down")
	failing := store.New[post](d, store.Scope(func(context.Context, *gorm.DB) (*gorm.DB, error) { return nil, boom }))
	if _, err := failing.List(ctx); !errors.Is(err, boom) {
		t.Fatalf("a scope error fails the read: %v", err)
	}
	if err := failing.Update(ctx, store.Where(where.Eq("title", "pub")), store.Set(map[string]any{"views": 9})); !errors.Is(err, boom) {
		t.Fatalf("and the write: %v", err)
	}
	if got, _ := all.Get(ctx, store.Where(where.Eq("title", "pub"))); got.Version != 1 {
		t.Fatalf("a failed scope writes nothing: %+v", got)
	}
	broken := store.New[post](d, store.Scope(func(context.Context, *gorm.DB) (*gorm.DB, error) { return nil, nil }))
	if _, err := broken.List(ctx); err == nil || !strings.Contains(err.Error(), "nil *gorm.DB") {
		t.Fatalf("a nil query: %v", err)
	}
}
