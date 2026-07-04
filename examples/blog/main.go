// Command blog is the chok v2 quickstart: a JWT-guarded blog API with
// owner-scoped posts in about a hundred lines of application code.
//
//	cd examples/blog && go run .
//
// The wiring model:
//
//	chok.yaml            declares the modules (and is the runtime config)
//	chok_modules_gen.go  generated assembly — `chok sync` refreshes it
//	main.go              the Post model, db Override and routes
//
// Walkthrough with curl commands: README.md next to this file.
package main

import (
	"os"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store"
)

// Post is the blog's one resource. OwnedSoftDeleteModel gives it a
// prefixed public ID, optimistic locking, soft delete and — because
// the model is owner-aware — an automatic owner scope: every query a
// store runs is fenced to the authenticated user, and unauthenticated
// access fails closed.
type Post struct {
	db.OwnedSoftDeleteModel
	Title   string `json:"title"   store:"query,update" gorm:"size:200;not null"`
	Content string `json:"content" store:"update"       gorm:"type:text;not null"`
	Status  string `json:"status"  store:"query,update" gorm:"size:20;default:'draft';not null"`
}

// RIDPrefix exposes posts as pst_xxx; the numeric key stays internal.
func (Post) RIDPrefix() string { return "pst" }

// Post statuses.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
)

// blogTables declares the schema (auto-migrated in dev). The partial
// unique index keeps titles unique per author among live rows while
// soft-deleted ones free the name.
var blogTables = []db.TableSpec{
	db.Table(&Post{}, db.SoftUnique("uk_post_title_owner", "title", "owner_id")),
}

// buildApp is shared by main and the acceptance test.
func buildApp() *chok.App {
	return chok.New("blog",
		chokModules(), // generated from chok.yaml
		chok.WithErrorMapper(store.MapError),
		chok.Override(db.Module(db.WithTables(blogTables...))),
		chok.Routes(routes),
	)
}

func main() {
	// chok resolves BLOG_CONFIG → ./blog.yaml → ./configs/blog.yaml;
	// default the env slot to the branded chok.yaml so `go run .`
	// works from this directory while operators keep the override.
	if os.Getenv("BLOG_CONFIG") == "" {
		_ = os.Setenv("BLOG_CONFIG", "chok.yaml")
	}
	buildApp().Execute()
}
