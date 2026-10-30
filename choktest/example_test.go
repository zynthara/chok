package choktest_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/choktest"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/store"
)

type Post struct {
	db.OwnedModel
	Title string `json:"title" store:"query,update" gorm:"size:200;not null"`
}

// An application's test: a database of the lane's dialect, the routes
// served in process, a JSON client.
func ExampleApp() {
	_ = func(t *testing.T) {
		posts := store.New[Post](choktest.DB(t, db.Table(&Post{})))
		c := choktest.App(t, chok.Base{}, func(app *chok.App) {
			chok.List(app.Router(), "/posts", posts)
		})
		var page chok.Page[Post]
		if code := c.GetJSON("/posts", &page); code != http.StatusOK {
			t.Fatalf("GET /posts = %d", code)
		}
	}
}

// A store test acts as a user through Principal.
func ExamplePrincipal() {
	_ = func(t *testing.T) {
		posts := choktest.Store[Post](t)
		alice := choktest.Principal(context.Background(), chok.Principal{UserID: "alice"})
		if err := posts.Create(alice, &Post{Title: "hello"}); err != nil {
			t.Fatal(err)
		}
	}
}
