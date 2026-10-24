package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

// Article is an owned model: store tags declare what the API may filter
// on and update.
type Article struct {
	db.OwnedSoftDeleteModel
	Title  string `json:"title" store:"query,update" gorm:"size:200;not null"`
	Status string `json:"status" store:"query,update" gorm:"size:20;not null"`
}

func (Article) RIDPrefix() string { return "art" }

// UpdateArticle is a PATCH body: a nil field was not sent.
type UpdateArticle struct {
	ID      string  `path:"id"`
	Version *int    `json:"version"`
	Title   *string `json:"title"`
	Status  *string `json:"status"`
}

func openExample() (*db.DB, func()) {
	dir, _ := os.MkdirTemp("", "chok-store-example")
	d := db.MustOpen(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: filepath.Join(dir, "app.db")}},
		db.Tables(db.Table(&Article{}, db.SoftUnique("uk_article_title_owner", "title", "owner_id"))))
	return d, func() { _ = d.Close(context.Background()); _ = os.RemoveAll(dir) }
}

func Example() {
	d, done := openExample()
	defer done()
	articles := store.New[Article](d)
	alice := chok.WithPrincipal(context.Background(), chok.Principal{UserID: "alice"})

	a := &Article{Title: "Hello", Status: "draft"}
	if err := articles.Create(alice, a); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(a.OwnerID, a.Version)

	// A partial update locked on the version the client saw.
	title := "Hello, store"
	req := &UpdateArticle{ID: a.RID, Version: new(1), Title: &title}
	if err := articles.Update(alice, store.RID(req.ID), store.Patch(req).Onto(a)); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(a.Title, a.Version)

	// Replaying the stale version is a 409.
	err := articles.Update(alice, store.RID(req.ID), store.Patch(req).Onto(a))
	fmt.Println(errors.Is(err, chok.ErrConflict))

	// Bob's queries never see alice's rows.
	bob := chok.WithPrincipal(context.Background(), chok.Principal{UserID: "bob"})
	_, err = articles.Get(bob, store.RID(a.RID))
	fmt.Println(errors.Is(err, chok.ErrNotFound))

	page, _ := articles.List(alice, where.Eq("status", "draft"), where.Order("created_at", true), where.Count())
	fmt.Println(page.Total, page.Items[0].Title)
	// Output:
	// alice 1
	// Hello, store 2
	// true
	// true
	// 1 Hello, store
}

// Aggregates are generic methods: the type argument says how to read the
// result, whatever the dialect returns.
func ExampleStore_GroupBy() {
	d, done := openExample()
	defer done()
	articles := store.New[Article](d)
	alice := chok.WithPrincipal(context.Background(), chok.Principal{UserID: "alice"})
	for _, title := range []string{"a", "b", "c"} {
		_ = articles.Create(alice, &Article{Title: title, Status: "draft"})
	}
	n, _ := articles.CountDistinct(alice, "title")
	groups, _ := articles.GroupBy[string](alice, "status", []store.Aggregate{{Func: "count"}})
	count, _ := groups[0].Values[0].Int64()
	fmt.Println(n, groups[0].Key, count)
	// Output: 3 draft 3
}
