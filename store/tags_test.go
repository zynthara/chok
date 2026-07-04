package store

import (
	"context"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// TaggedPost declares its allowlists on the model: title/status are
// filterable, content is write-only, Internal carries no tag and must
// stay off both surfaces.
type TaggedPost struct {
	db.SoftDeleteModel
	Title    string `json:"title"   store:"query,update" gorm:"size:200"`
	Content  string `json:"content" store:"update"       gorm:"type:text"`
	Status   string `json:"status"  store:"query,update" gorm:"size:20;default:'draft'"`
	Internal string `json:"internal" gorm:"size:50"`
	Secret   string `json:"-"       store:"query" gorm:"size:50"`
}

func (TaggedPost) RIDPrefix() string { return "tpo" }

func newTaggedStore(t *testing.T) *Store[TaggedPost] {
	t.Helper()
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&TaggedPost{})); err != nil {
		t.Fatal(err)
	}
	return New[TaggedPost](gdb, log.Empty())
}

func TestNew_TagsDeclareBothSurfaces(t *testing.T) {
	s := newTaggedStore(t)

	wantQuery := map[string]string{
		"title":  "title",
		"status": "status",
		"secret": "secret", // json:"-" falls back to snake_case name
		// base model contribution:
		"id":         "rid",
		"created_at": "created_at",
		"updated_at": "updated_at",
	}
	for name, col := range wantQuery {
		if got := s.queryFieldMap[name]; got != col {
			t.Errorf("queryFieldMap[%q] = %q, want %q", name, got, col)
		}
	}
	for _, absent := range []string{"content", "internal", "version"} {
		if _, ok := s.queryFieldMap[absent]; ok {
			t.Errorf("queryFieldMap must not expose %q", absent)
		}
	}

	wantUpdate := map[string]string{"title": "title", "content": "content", "status": "status"}
	if len(s.updateFieldMap) != len(wantUpdate) {
		t.Errorf("updateFieldMap = %v, want exactly %v", s.updateFieldMap, wantUpdate)
	}
	for name, col := range wantUpdate {
		if got := s.updateFieldMap[name]; got != col {
			t.Errorf("updateFieldMap[%q] = %q, want %q", name, got, col)
		}
	}
}

func TestNew_TagsEnforcedEndToEnd(t *testing.T) {
	s := newTaggedStore(t)
	ctx := context.Background()

	p := &TaggedPost{Title: "hello", Content: "body", Status: "draft", Internal: "x"}
	if err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	// Tagged query field filters fine; untagged field is rejected.
	if _, err := s.List(ctx, where.WithFilter("status", "draft")); err != nil {
		t.Fatalf("tagged filter: %v", err)
	}
	if _, err := s.List(ctx, where.WithFilter("internal", "x")); err == nil {
		t.Fatal("untagged field must not be filterable")
	}

	// Tagged update field writes; untagged field is rejected.
	if err := s.Update(ctx, RID(p.RID), Set(map[string]any{"title": "hi"})); err != nil {
		t.Fatalf("tagged update: %v", err)
	}
	if err := s.Update(ctx, RID(p.RID), Set(map[string]any{"internal": "y"})); err == nil {
		t.Fatal("untagged field must not be writable")
	}
}

func TestNew_ExplicitOptionsOverrideTags(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&TaggedPost{})); err != nil {
		t.Fatal(err)
	}
	s := New[TaggedPost](gdb, log.Empty(),
		WithQueryFields("status"),
		WithUpdateFields("title"),
	)
	if _, ok := s.queryFieldMap["title"]; ok {
		t.Error("WithQueryFields must fully replace the tag declaration")
	}
	if _, ok := s.updateFieldMap["content"]; ok {
		t.Error("WithUpdateFields must fully replace the tag declaration")
	}
	if s.updateFieldMap["title"] != "title" {
		t.Error("explicit update list lost its field")
	}
}

func TestNew_TagsSatisfyStrictMode(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&TaggedPost{})); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("tag-declared model must satisfy WithStrict, got panic: %v", r)
		}
	}()
	_ = New[TaggedPost](gdb, log.Empty(), WithStrict())
}

type badTagModel struct {
	db.Model
	Name string `json:"name" store:"quer"`
}

func (badTagModel) RIDPrefix() string { return "btm" }

func TestNew_BadTagValuePanics(t *testing.T) {
	gdb := setupDB(t)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("bad store tag value must panic at construction")
		}
		msg := r.(string)
		if !strings.Contains(msg, "badTagModel.Name") || !strings.Contains(msg, "quer") {
			t.Fatalf("panic should name the field and the bad value, got: %v", msg)
		}
	}()
	_ = New[badTagModel](gdb, log.Empty())
}

// updateOnlyModel tags only the update side: the query surface falls
// back to the base-model fields, and nothing warns.
type updateOnlyModel struct {
	db.Model
	Note string `json:"note" store:"update"`
}

func (updateOnlyModel) RIDPrefix() string { return "uom" }

func TestNew_UpdateOnlyTags_QueryKeepsBaseFields(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&updateOnlyModel{})); err != nil {
		t.Fatal(err)
	}
	s := New[updateOnlyModel](gdb, log.Empty())
	if s.queryFieldMap["id"] != "rid" {
		t.Errorf("base id must stay queryable, got map %v", s.queryFieldMap)
	}
	if _, ok := s.queryFieldMap["note"]; ok {
		t.Error("note is update-only, must not be filterable")
	}
	if s.updateFieldMap["note"] != "note" {
		t.Errorf("note must be writable, got map %v", s.updateFieldMap)
	}
}
