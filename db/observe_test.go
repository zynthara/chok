package db_test

import (
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/internal/dbx"
)

// statements records what an observer sees: the chain of each statement
// and the first word of its SQL, in order.
type statements struct {
	mu     sync.Mutex
	before []string
	after  []string
	sql    []string
}

func (s *statements) Before(_ *gorm.DB, op string) {
	s.mu.Lock()
	s.before = append(s.before, op)
	s.mu.Unlock()
}

func (s *statements) After(tx *gorm.DB, op string) {
	word, _, _ := strings.Cut(strings.TrimSpace(tx.Statement.SQL.String()), " ")
	s.mu.Lock()
	s.after = append(s.after, op)
	s.sql = append(s.sql, strings.ToUpper(word))
	s.mu.Unlock()
}

func (s *statements) seen() (before, after, sql []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.before...), append([]string(nil), s.after...), append([]string(nil), s.sql...)
}

func TestObserve_SeesEveryStatement(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{})))
	ctx := t.Context()
	rec := &statements{}
	got := dbx.Observe(d, rec)
	if got.Driver != dbtest.Driver() || got.Primary == nil {
		t.Fatalf("Observed = %+v on %s", got, dbtest.Driver())
	}
	if (got.Readers != nil) != (dbtest.Driver() == "sqlite") {
		t.Fatalf("a SQLite file has a read pool, the servers do not: %+v", got)
	}

	gdb := d.Unsafe(ctx)
	n := note{Title: "first"}
	if err := gdb.Create(&n).Error; err != nil {
		t.Fatal(err)
	}
	var back note
	if err := gdb.First(&back, n.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Model(&back).Update("title", "second").Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := gdb.Raw("SELECT COUNT(*) FROM notes").Scan(&count).Error; err != nil || count != 1 {
		t.Fatalf("count %d, %v", count, err)
	}
	if err := gdb.Exec("UPDATE notes SET title = ? WHERE id = ?", "third", n.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Delete(&note{}, n.ID).Error; err != nil {
		t.Fatal(err)
	}

	before, after, sql := rec.seen()
	wantOps := []string{"create", "query", "update", "row", "raw", "delete"}
	wantSQL := []string{"INSERT", "SELECT", "UPDATE", "SELECT", "UPDATE", "DELETE"}
	if strings.Join(before, ",") != strings.Join(wantOps, ",") || strings.Join(after, ",") != strings.Join(wantOps, ",") {
		t.Fatalf("before %v, after %v; want %v for both", before, after, wantOps)
	}
	if strings.Join(sql, ",") != strings.Join(wantSQL, ",") {
		t.Fatalf("After sees the statement's SQL: %v, want %v", sql, wantSQL)
	}

	// Another observer replaces the first; nil detaches.
	next := &statements{}
	dbx.Observe(d, next)
	if err := gdb.First(&note{}, n.ID).Error; err == nil {
		t.Fatal("the note was deleted")
	}
	dbx.Observe(d, nil)
	if err := gdb.Find(&[]note{}).Error; err != nil {
		t.Fatal(err)
	}
	if b, _, _ := rec.seen(); len(b) != len(wantOps) {
		t.Fatalf("the replaced observer saw %d statements more", len(b)-len(wantOps))
	}
	if b, a, _ := next.seen(); len(b) != 1 || len(a) != 1 {
		t.Fatalf("the second observer sees one statement until it is detached: %v %v", b, a)
	}
}

// TestObserve_AttachWhileStatementsRun attaches and detaches observers
// while other goroutines query: the hooks are in place from Open, so the
// race detector has nothing to report (registering gorm callbacks at that
// point would race the statements).
func TestObserve_AttachWhileStatementsRun(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{})))
	ctx := t.Context()
	if err := d.Unsafe(ctx).Create(&note{Title: "n"}).Error; err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				var notes []note
				if err := d.Unsafe(ctx).Find(&notes).Error; err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	var observers []*statements
	for range 20 {
		o := &statements{}
		observers = append(observers, o)
		dbx.Observe(d, o)
		if err := d.Unsafe(ctx).Find(&[]note{}).Error; err != nil {
			t.Fatal(err)
		}
		dbx.Observe(d, nil)
	}
	close(stop)
	wg.Wait()
	// Each observer saw at least the statement run while it was attached;
	// the goroutines' statements straddle the swaps.
	for i, o := range observers {
		if b, a, _ := o.seen(); len(b) == 0 || len(a) == 0 {
			t.Fatalf("observer %d saw %d befores and %d afters", i, len(b), len(a))
		}
	}
}
