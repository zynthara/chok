package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

// writer has no owner: anyone may read one, not its remarks.
type writer struct {
	db.Model
	Name    string   `json:"name" store:"query" gorm:"size:20;not null"`
	Remarks []remark `json:"remarks,omitzero"`
}

// remark is owned by whoever wrote it, whatever article it is on.
type remark struct {
	db.OwnedModel
	ArticleID uint    `json:"-" gorm:"not null;index"`
	WriterID  uint    `json:"-" gorm:"not null"`
	Writer    *writer `json:"writer,omitzero"`
	Body      string  `json:"body" store:"query" gorm:"size:50;not null"`
}

type article struct {
	db.OwnedModel
	Title    string   `json:"title" store:"query,update" gorm:"size:50;not null"`
	WriterID uint     `json:"-" gorm:"not null"`
	Writer   *writer  `json:"writer,omitzero"`
	Remarks  []remark `json:"remarks,omitzero"`
}

func bodies(rs []remark) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Body)
	}
	slices.Sort(out)
	return out
}

// TestPreload_FencesOwnedAssociations: preloading reaches only the
// associated rows a store of their model would show the caller — an
// owned association is fenced to the principal (all of it for an admin,
// none without a principal) — while an association without an owner
// loads whole.
func TestPreload_FencesOwnedAssociations(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&writer{}), db.Table(&article{}), db.Table(&remark{})))
	policy := store.Policy(db.StorePolicy{AdminRoles: []string{"admin"}})
	articles, remarks := store.New[article](d, policy), store.New[remark](d, policy)
	writers := store.New[writer](d, policy)
	ctx := t.Context()
	alice, bob, root := as(ctx, "alice"), as(ctx, "bob"), as(ctx, "root", "admin")
	w := &writer{Name: "w"}
	mustCreate(t, ctx, writers, w)
	a := &article{Title: "alice's", WriterID: w.ID}
	mustCreate(t, alice, articles, a)
	mustCreate(t, alice, remarks, &remark{ArticleID: a.ID, WriterID: w.ID, Body: "mine"})
	mustCreate(t, bob, remarks, &remark{ArticleID: a.ID, WriterID: w.ID, Body: "bob's on alice's"})

	got, err := articles.Get(alice, store.RID(a.RID), where.Preload("Remarks"), where.Preload("Writer"))
	if err != nil {
		t.Fatal(err)
	}
	if b := bodies(got.Remarks); !slices.Equal(b, []string{"mine"}) || got.Writer == nil || got.Writer.Name != "w" {
		t.Fatalf("alice preloads %v and writer %+v", b, got.Writer)
	}
	got, _ = articles.Get(root, store.RID(a.RID), where.Preload("Remarks"))
	if b := bodies(got.Remarks); len(b) != 2 {
		t.Fatalf("an admin preloads every remark: %v", b)
	}
	// Nested and wildcard paths fence each owned step.
	page, err := articles.List(alice, where.Preload("Remarks.Writer"))
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("nested: %+v %v", page, err)
	}
	if rs := page.Items[0].Remarks; len(rs) != 1 || rs[0].Writer == nil || rs[0].Writer.Name != "w" {
		t.Fatalf("nested preload %+v", rs)
	}
	got, _ = articles.Get(alice, store.RID(a.RID), where.Preload("*"))
	if b := bodies(got.Remarks); !slices.Equal(b, []string{"mine"}) || got.Writer == nil {
		t.Fatalf("every association: %v %+v", b, got.Writer)
	}
	// Under a policy without the owner scope nothing is fenced.
	off := false
	open := store.New[article](d, store.Policy(db.StorePolicy{OwnerScope: &off}))
	got, err = open.Get(ctx, store.RID(a.RID), where.Preload("Remarks"))
	if err != nil {
		t.Fatalf("no owner scope: %v", err)
	}
	if len(got.Remarks) != 2 {
		t.Fatalf("no owner scope: %+v", got.Remarks)
	}
	// The rows of a model without an owner are open, its owned
	// associations are not: nothing without a principal.
	for who, want := range map[string]int{"anonymous": 0, "alice": 1, "bob": 1, "root": 2} {
		c := map[string]context.Context{"anonymous": ctx, "alice": alice, "bob": bob, "root": root}[who]
		got, err := writers.Get(c, store.RID(w.RID), where.Preload("Remarks"))
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		if len(got.Remarks) != want {
			t.Errorf("%s reads %d of the writer's remarks", who, len(got.Remarks))
		}
	}
	if _, err := articles.Get(alice, store.RID(a.RID), where.Preload("Nope")); err == nil {
		t.Fatal("an unknown association is an error")
	}
	if _, err := articles.Get(chok.WithPrincipal(ctx, chok.Principal{}), store.RID(a.RID)); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("an empty principal reads nothing: %v", err)
	}
}
