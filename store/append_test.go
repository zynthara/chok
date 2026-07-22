package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// --- test models (append-only gate, arch-backlog #13) ---

// AuditEntry is the canonical append-only model: tag-declared query
// surface, no update tags (an append store has no update path).
type AuditEntry struct {
	db.AppendOnlyModel
	Actor  string `json:"actor"  gorm:"size:100" store:"query"`
	Action string `json:"action" gorm:"size:100" store:"query"`
	Note   string `json:"note"   gorm:"size:200"`
}

// DedupEvent carries the append-only idempotency pattern: INSERT plus
// a unique key, with WithConstraintFields blaming the public field.
type DedupEvent struct {
	db.AppendOnlyModel
	Key string `json:"key" gorm:"uniqueIndex:uk_dedup_key;size:64;not null" store:"query"`
}

// UntaggedSample has no store tags — exercises discovery + strict mode.
type UntaggedSample struct {
	db.AppendOnlyModel
	Kind string `json:"kind" gorm:"size:40"`
}

// UpdateTaggedSample declares an update-side tag — a contradiction on
// an append-only model; NewAppend must fail construction.
type UpdateTaggedSample struct {
	db.AppendOnlyModel
	Kind string `json:"kind" gorm:"size:40" store:"query,update"`
}

// DoubleBaseSample embeds both bases (the append one through an
// intermediate so the duplicate created_at json tags sit at different
// depths and don't trip go vet). Both markers promote, so this
// compiles against store.New AND store.NewAppend — round-1 review #1:
// both constructors must refuse it at construction.
type doubleBasePart struct{ db.AppendOnlyModel }

type DoubleBaseSample struct {
	db.Model
	doubleBasePart
}

// ShadowIDSample shadows the base ID with its own column — round-1
// review #2: without rejection the pagination tie-breaker would bind
// to the possibly non-unique event_key column.
type ShadowIDSample struct {
	db.AppendOnlyModel
	ID string `json:"event_key" gorm:"column:event_key;size:64"`
}

// --- helpers ---

func setupAuditStore(t *testing.T) (*AppendStore[AuditEntry], *db.DB) {
	t.Helper()
	h := setupDB(t)
	if err := h.Migrate(context.Background(), db.Table(&AuditEntry{})); err != nil {
		t.Fatal(err)
	}
	return NewAppend[AuditEntry](h, log.Empty()), h
}

func appendEntries(t *testing.T, s *AppendStore[AuditEntry], entries ...*AuditEntry) {
	t.Helper()
	for _, e := range entries {
		if err := s.Create(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

// pinCreatedAt collapses every row onto one created_at value so the
// deterministic-order guarantees are exercised under real ties (batch
// inserts land on the same millisecond routinely; here we force it).
func pinCreatedAt(t *testing.T, h *db.DB, model any, ts time.Time) {
	t.Helper()
	if err := h.Unsafe(context.Background()).Model(model).Where("1 = 1").
		Update("created_at", ts).Error; err != nil {
		t.Fatal(err)
	}
}

// --- construction ---

func TestNewAppend_MarkerIsolation(t *testing.T) {
	// store.New's constraint IS db.Modeler and store.NewAppend's IS
	// db.AppendModeler, so these runtime interface checks are exactly
	// the compile-time exclusions: an append model cannot instantiate
	// New[T] and a full model cannot instantiate NewAppend[T].
	var a any = &AuditEntry{}
	if _, ok := a.(db.Modeler); ok {
		t.Fatal("append-only model must NOT satisfy db.Modeler (would admit it to store.New)")
	}
	if _, ok := a.(db.AppendModeler); !ok {
		t.Fatal("append-only model must satisfy db.AppendModeler")
	}
	var m any = &Item{}
	if _, ok := m.(db.AppendModeler); ok {
		t.Fatal("full model must NOT satisfy db.AppendModeler (would admit it to store.NewAppend)")
	}
	if _, ok := m.(db.Modeler); !ok {
		t.Fatal("full model must satisfy db.Modeler")
	}
}

func TestNewAppend_RejectedOptionsPanic(t *testing.T) {
	h := setupDB(t)
	if err := h.Migrate(context.Background(), db.Table(&AuditEntry{})); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		opt  StoreOption
		want string
	}{
		{"update fields", WithUpdateFields("note"), "WithUpdateFields"},
		{"all update fields", WithAllUpdateFields(), "WithAllUpdateFields"},
		{"admin roles", WithAdminRoles("admin"), "WithAdminRoles"},
		{"require principal", WithRequirePrincipal(), "WithRequirePrincipal"},
		{"without require principal", WithoutRequirePrincipal(), "WithRequirePrincipal"},
		{"without owner scope", WithoutOwnerScope(), "WithoutOwnerScope"},
		// WithBeforeCreate is constrained to db.Modeler, so an append
		// model cannot even type a hook — reject any full-model one.
		{"before create hook", WithBeforeCreate(func(context.Context, *Item) error { return nil }), "hooks"},
		{"before update hook", WithBeforeUpdate(func(context.Context, Locator, ChangeSnapshot) error { return nil }), "hooks"},
		{"before delete hook", WithBeforeDelete(func(context.Context, Locator) error { return nil }), "hooks"},
		{"default page size", WithDefaultPageSize(20), "WithDefaultPageSize"},
		{"bus", WithBus(event.NewBus()), "WithBus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil || !strings.Contains(fmt.Sprint(r), tc.want) {
					t.Fatalf("want panic containing %q, got %v", tc.want, r)
				}
			}()
			_ = NewAppend[AuditEntry](h, log.Empty(), tc.opt)
		})
	}
}

// Round-1 review #1/#2: types that slip past the generic constraints
// (double-base embeds satisfy both markers; shadowed base fields keep
// the marker) must die at construction, in BOTH constructors.
func TestNewAppend_DoubleBaseAndShadowingPanic(t *testing.T) {
	h := setupDB(t)
	mustPanic := func(want string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), want) {
				t.Fatalf("want panic containing %q, got %v", want, r)
			}
		}()
		fn()
	}
	// Compiles against both constructors — the loophole under test.
	mustPanic("pick one base", func() { _ = New[DoubleBaseSample](h, log.Empty()) })
	mustPanic("pick one base", func() { _ = NewAppend[DoubleBaseSample](h, log.Empty()) })
	mustPanic("shadows AppendOnlyModel.ID", func() { _ = NewAppend[ShadowIDSample](h, log.Empty()) })
}

func TestNewAppend_UpdateTagPanics(t *testing.T) {
	h := setupDB(t)
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), "update") {
			t.Fatalf("want panic about update tags, got %v", r)
		}
	}()
	_ = NewAppend[UpdateTaggedSample](h, log.Empty())
}

func TestNewAppend_StrictRejectsAutoDiscovery(t *testing.T) {
	h := setupDB(t)
	func() {
		defer func() {
			r := recover()
			if r == nil || !strings.Contains(fmt.Sprint(r), "strict mode") {
				t.Fatalf("want strict-mode panic, got %v", r)
			}
		}()
		_ = NewAppend[UntaggedSample](h, log.Empty(), WithStrict())
	}()
	// Explicit declaration satisfies strict.
	s := NewAppend[UntaggedSample](h, log.Empty(), WithStrict(), WithQueryFields("kind"))
	if s == nil {
		t.Fatal("explicit WithQueryFields must satisfy strict mode")
	}
}

func TestNewAppend_ColumnAliasQuerySide(t *testing.T) {
	h := setupDB(t)
	if err := h.Migrate(context.Background(), db.Table(&AuditEntry{})); err != nil {
		t.Fatal(err)
	}
	s := NewAppend[AuditEntry](h, log.Empty(),
		WithQueryFields("who"), WithColumnAlias("who", "actor"))
	appendEntries(t, s,
		&AuditEntry{Actor: "alice", Action: "login"},
		&AuditEntry{Actor: "bob", Action: "login"},
	)
	page, err := s.List(context.Background(), where.WithFilter("who", "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Actor != "alice" {
		t.Fatalf("alias filter failed: %+v", page.Items)
	}

	// Alias onto an undeclared field is a construction error.
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "not declared") {
			t.Fatalf("want undeclared-alias panic, got %v", r)
		}
	}()
	_ = NewAppend[AuditEntry](h, log.Empty(), WithColumnAlias("ghost", "actor"))
}

// --- create / list ---

func TestAppendStore_CreateAndListRoundtrip(t *testing.T) {
	s, _ := setupAuditStore(t)
	ctx := context.Background()
	appendEntries(t, s,
		&AuditEntry{Actor: "alice", Action: "login"},
		&AuditEntry{Actor: "bob", Action: "login"},
		&AuditEntry{Actor: "alice", Action: "logout"},
	)

	page, err := s.List(ctx, where.WithFilter("actor", "alice"), where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Total != 2 {
		t.Fatalf("want 2 alice rows (total 2), got %d (total %d)", len(page.Items), page.Total)
	}
	for _, it := range page.Items {
		if it.Actor != "alice" {
			t.Fatalf("filter leak: %+v", it)
		}
		if it.CreatedAt.IsZero() {
			t.Fatal("autoCreateTime must fill CreatedAt")
		}
	}

	// Zero matches: non-nil empty slice.
	empty, err := s.List(ctx, where.WithFilter("actor", "nobody"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("zero matches must give non-nil empty Items, got %#v", empty.Items)
	}
}

func TestAppendStore_NumericIDNeverInJSON(t *testing.T) {
	s, _ := setupAuditStore(t)
	appendEntries(t, s, &AuditEntry{Actor: "alice", Action: "login"})
	page, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(page.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, leaked := m["id"]; leaked {
		t.Fatalf("numeric PK must not serialize: %s", raw)
	}
	if _, ok := m["created_at"]; !ok {
		t.Fatalf("created_at must serialize: %s", raw)
	}
}

func TestAppendStore_DefaultOrderStableAcrossPagesUnderTies(t *testing.T) {
	s, h := setupAuditStore(t)
	ctx := context.Background()
	for i := range 6 {
		appendEntries(t, s, &AuditEntry{Actor: fmt.Sprintf("u%d", i), Action: "tick"})
	}
	// Collapse created_at to one value: without the PK tie-breaker,
	// LIMIT/OFFSET pages over tied rows may shuffle between queries.
	pinCreatedAt(t, h, &AuditEntry{}, time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC))

	var got []string
	for pageNo := 1; pageNo <= 3; pageNo++ {
		page, err := s.List(ctx, where.WithPage(pageNo, 2))
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("page %d: want 2 items, got %d", pageNo, len(page.Items))
		}
		for _, it := range page.Items {
			got = append(got, it.Actor)
		}
	}
	// Default order is insertion order (created_at, then internal PK) —
	// with created_at fully tied, the PK alone must reproduce it.
	want := []string{"u0", "u1", "u2", "u3", "u4", "u5"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pagination under ties must be insertion-ordered and lossless: got %v want %v", got, want)
	}
}

func TestAppendStore_ExplicitOrderGetsPKTieBreaker(t *testing.T) {
	s, h := setupAuditStore(t)
	ctx := context.Background()
	for i := range 4 {
		appendEntries(t, s, &AuditEntry{Actor: fmt.Sprintf("u%d", i), Action: "tick"})
	}
	pinCreatedAt(t, h, &AuditEntry{}, time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC))

	var got []string
	for pageNo := 1; pageNo <= 2; pageNo++ {
		page, err := s.List(ctx, where.WithOrder("created_at", true), where.WithPage(pageNo, 2))
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			got = append(got, it.Actor)
		}
	}
	// created_at DESC over fully tied rows: the trailing ASC PK
	// tie-breaker yields insertion order within the tie — and above
	// all, no row lost or duplicated across pages.
	want := []string{"u0", "u1", "u2", "u3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("explicit order must stay deterministic under ties: got %v want %v", got, want)
	}
}

func TestAppendStore_MaxPageSizeCaps(t *testing.T) {
	h := setupDB(t)
	if err := h.Migrate(context.Background(), db.Table(&AuditEntry{})); err != nil {
		t.Fatal(err)
	}
	s := NewAppend[AuditEntry](h, log.Empty(), WithMaxPageSize(2))
	for i := range 5 {
		appendEntries(t, s, &AuditEntry{Actor: fmt.Sprintf("u%d", i), Action: "tick"})
	}
	page, err := s.List(context.Background(), where.WithPage(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Meta.Size != 2 {
		t.Fatalf("store cap must clamp the page: items=%d meta=%+v", len(page.Items), page.Meta)
	}
}

func TestAppendStore_UnknownFilterFieldIsServerBug(t *testing.T) {
	s, _ := setupAuditStore(t)
	_, err := s.List(context.Background(), where.WithFilter("typo", "x"))
	if !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("programmatic unknown field must pass through raw (500-shaped), got %v", err)
	}
}

// --- duplicates / batch / tx ---

func TestAppendStore_DuplicateMapsToConstraintField(t *testing.T) {
	h := setupDB(t)
	if err := h.Migrate(context.Background(), db.Table(&DedupEvent{})); err != nil {
		t.Fatal(err)
	}
	s := NewAppend[DedupEvent](h, log.Empty(), WithConstraintFields(map[string]string{
		"uk_dedup_key": "key", // PG / MySQL report the index name
		"key":          "key", // SQLite reports the column list
	}))
	ctx := context.Background()
	if err := s.Create(ctx, &DedupEvent{Key: "evt-1"}); err != nil {
		t.Fatal(err)
	}
	err := s.Create(ctx, &DedupEvent{Key: "evt-1"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	var dup *DuplicateEntryError
	if !errors.As(err, &dup) || dup.Field != "key" {
		t.Fatalf("declared constraint must resolve to the public field, got %+v", dup)
	}
}

func TestAppendStore_BatchCreateAtomicAndEmptyNoop(t *testing.T) {
	h := setupDB(t)
	if err := h.Migrate(context.Background(), db.Table(&DedupEvent{})); err != nil {
		t.Fatal(err)
	}
	s := NewAppend[DedupEvent](h, log.Empty())
	ctx := context.Background()

	if err := s.BatchCreate(ctx, nil); err != nil {
		t.Fatalf("empty batch must be a no-op, got %v", err)
	}

	err := s.BatchCreate(ctx, []*DedupEvent{
		{Key: "a"}, {Key: "b"}, {Key: "a"}, // duplicate inside the batch
	})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	page, err := s.List(ctx, where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatalf("failed batch must roll back entirely, found %d rows", page.Total)
	}

	if err := s.BatchCreate(ctx, []*DedupEvent{{Key: "c"}, {Key: "d"}}); err != nil {
		t.Fatal(err)
	}
	page, err = s.List(ctx, where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("want 2 rows after clean batch, got %d", page.Total)
	}
}

func TestAppendStore_JoinsContextTransaction(t *testing.T) {
	s, h := setupAuditStore(t)
	sentinel := errors.New("boom")
	err := h.RunInTx(context.Background(), func(txCtx context.Context) error {
		if err := s.Create(txCtx, &AuditEntry{Actor: "tx", Action: "step"}); err != nil {
			return err
		}
		if err := s.BatchCreate(txCtx, []*AuditEntry{{Actor: "tx", Action: "batch"}}); err != nil {
			return err
		}
		// Visible inside the transaction.
		page, err := s.List(txCtx, where.WithFilter("actor", "tx"), where.WithCount())
		if err != nil {
			return err
		}
		if page.Total != 2 {
			return fmt.Errorf("want 2 rows inside tx, got %d", page.Total)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
	page, err := s.List(context.Background(), where.WithFilter("actor", "tx"), where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatalf("rolled-back writes must not be visible, found %d", page.Total)
	}
}

// --- scopes / read-only ---

func TestAppendStore_ScopeAppliesAndFailsClosed(t *testing.T) {
	h := setupDB(t)
	if err := h.Migrate(context.Background(), db.Table(&AuditEntry{})); err != nil {
		t.Fatal(err)
	}
	scoped := NewAppend[AuditEntry](h, log.Empty(),
		WithScope(func(ctx context.Context, q *gorm.DB) (*gorm.DB, error) {
			return q.Where("actor = ?", "alice"), nil
		}))
	appendEntries(t, scoped, &AuditEntry{Actor: "alice", Action: "a"})
	if err := NewAppend[AuditEntry](h, log.Empty()).
		Create(context.Background(), &AuditEntry{Actor: "bob", Action: "b"}); err != nil {
		t.Fatal(err)
	}

	page, err := scoped.List(context.Background(), where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Actor != "alice" {
		t.Fatalf("scope must constrain List and its count: %+v", page)
	}

	failing := NewAppend[AuditEntry](h, log.Empty(),
		WithScope(func(ctx context.Context, q *gorm.DB) (*gorm.DB, error) {
			return nil, errors.New("denied")
		}))
	if _, err := failing.List(context.Background()); err == nil {
		t.Fatal("scope error must fail the read (fail-closed)")
	}
}

func TestAppendStore_ReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "append-readonly.db")
	writable, err := db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writable.Close() })
	if err := writable.Migrate(ctx, db.Table(&AuditEntry{})); err != nil {
		t.Fatal(err)
	}
	if err := NewAppend[AuditEntry](writable, log.Empty()).
		Create(ctx, &AuditEntry{Actor: "seed", Action: "a"}); err != nil {
		t.Fatal(err)
	}

	ro, err := db.Open(db.Options{Driver: "sqlite", ReadOnly: true, SQLite: db.SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "WithReadOnly") {
				t.Fatalf("read-only handle without declaration must panic with guidance, got %v", r)
			}
		}()
		_ = NewAppend[AuditEntry](ro, log.Empty())
	}()

	s := NewAppend[AuditEntry](ro, log.Empty(), WithReadOnly())
	if page, err := s.List(ctx, where.WithCount()); err != nil || page.Total != 1 {
		t.Fatalf("read-only append store must still read: page=%+v err=%v", page, err)
	}
	obj := &AuditEntry{Actor: "blocked", Action: "x"}
	if err := s.Create(ctx, obj); !errors.Is(err, db.ErrReadOnly) {
		t.Fatalf("create: want db.ErrReadOnly, got %v", err)
	}
	if err := s.BatchCreate(ctx, []*AuditEntry{obj}); !errors.Is(err, db.ErrReadOnly) {
		t.Fatalf("batch-create: want db.ErrReadOnly, got %v", err)
	}
}

// --- MySQL lane (regex-selected in Makefile test-mysql) ---

// TestForeignTableGate_MySQL covers both halves of arch-backlog #13
// against real MySQL: ForeignTable composite-PK migration + join-row
// DML through h.Unsafe, and an append-only table through db.Table +
// NewAppend with the managed columns verifiably absent.
func TestForeignTableGate_MySQL(t *testing.T) {
	h := dbtest.OpenMySQL(t)
	ctx := context.Background()

	type UserRoleJoinMy struct {
		UserID uint `gorm:"primaryKey"`
		RoleID uint `gorm:"primaryKey"`
	}
	if err := h.Migrate(ctx, db.ForeignTable(&UserRoleJoinMy{}), db.Table(&AuditEntry{})); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	gdb := h.Unsafe(ctx)
	if err := gdb.Create(&UserRoleJoinMy{UserID: 1, RoleID: 2}).Error; err != nil {
		t.Fatalf("join insert: %v", err)
	}
	if err := gdb.Create(&UserRoleJoinMy{UserID: 1, RoleID: 2}).Error; err == nil {
		t.Fatal("composite PK must reject the duplicate pair")
	}
	if err := gdb.Delete(&UserRoleJoinMy{}, "user_id = ? AND role_id = ?", 1, 2).Error; err != nil {
		t.Fatalf("join delete: %v", err)
	}

	for _, col := range []string{"rid", "version", "updated_at", "deleted_at"} {
		if gdb.Migrator().HasColumn(&AuditEntry{}, col) {
			t.Fatalf("append-only table must not have column %q", col)
		}
	}
	s := NewAppend[AuditEntry](h, log.Empty())
	appendEntries(t, s,
		&AuditEntry{Actor: "alice", Action: "login"},
		&AuditEntry{Actor: "alice", Action: "logout"},
	)
	page, err := s.List(ctx, where.WithFilter("actor", "alice"), where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("want 2 rows, got %d (total %d)", len(page.Items), page.Total)
	}
}
