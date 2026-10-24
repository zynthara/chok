package db_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zynthara/chok/v3/db"
)

type Post struct {
	db.OwnedSoftDeleteModel
	Title string `gorm:"size:200;not null"`
}

func (Post) RIDPrefix() string { return "pst" }

func ExampleOpen() {
	dir, _ := os.MkdirTemp("", "chok-example")
	defer func() { _ = os.RemoveAll(dir) }()
	d, err := db.Open(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: filepath.Join(dir, "app.db")}},
		db.Tables(db.Table(&Post{}, db.SoftUnique("uk_post_title_owner", "title", "owner_id"))))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = d.Close(context.Background()) }()
	fmt.Println(d.Unsafe(context.Background()).Migrator().HasTable(&Post{}))
	// Output: true
}

func ExampleDB_RunInTx() {
	d, _ := db.Open(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}}, db.Tables(db.Table(&Post{})))
	defer func() { _ = d.Close(context.Background()) }()
	err := d.RunInTx(context.Background(), func(tx context.Context) error {
		d.AfterCommit(tx, func(context.Context) { fmt.Println("committed") })
		return d.Unsafe(tx).Create(&Post{Title: "hello", OwnerID: "u1"}).Error
	})
	fmt.Println(err)
	// Output:
	// committed
	// <nil>
}

func ExampleNewRID() {
	rid := db.NewRID("pst")
	prefix, ok := db.ParseRID(rid)
	fmt.Println(prefix, ok, len(rid))
	// Output: pst true 16
}
