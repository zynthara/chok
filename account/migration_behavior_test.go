package account_test

import (
	"errors"
	"maps"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/internal/testseq"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store"
)

func TestMigrationBehavior_AccountUpgradeMatrix(t *testing.T) {
	runAccountUpgradeMatrix(t, dbtest.Open)
}

func TestMigrationBehavior_MySQLAccountUpgradeMatrix(t *testing.T) {
	runAccountUpgradeMatrix(t, dbtest.OpenMySQL)
}

func runAccountUpgradeMatrix(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	prefix := account.MigrationPrefixForTest(t, 1)
	testseq.RunUpgradeMatrix(t, open, account.MigrationSequence(), prefix, testseq.UpgradeSpec{
		Seed:          seedAccountLegacyMatrix,
		VerifyUpgrade: verifyAccountLegacyMatrix,
		Trace:         accountBehaviorTrace,
		PrepareAdoptable: func(t testing.TB, h *db.DB) {
			t.Helper()
			if err := account.MigrateSchema(t.Context(), h); err != nil {
				t.Fatalf("account auto schema: %v", err)
			}
			seedAccountLegacyMatrix(t, h)
			if err := account.BackfillHasPassword(t.Context(), h); err != nil {
				t.Fatalf("account historical Go backfill: %v", err)
			}
		},
	})
}

func TestMigrationBehavior_AccountBackfillEquivalent(t *testing.T) {
	assertAccountBackfillEquivalent(t, dbtest.Open)
}

func TestMigrationBehavior_MySQLAccountBackfillEquivalent(t *testing.T) {
	assertAccountBackfillEquivalent(t, dbtest.OpenMySQL)
}

func assertAccountBackfillEquivalent(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	prefix := account.MigrationPrefixForTest(t, 1)

	sqlDB := open(t)
	if _, err := db.ApplySequence(t.Context(), sqlDB, prefix); err != nil {
		t.Fatalf("apply account prefix: %v", err)
	}
	seedAccountLegacyMatrix(t, sqlDB)
	if _, err := db.ApplySequence(t.Context(), sqlDB, account.MigrationSequence()); err != nil {
		t.Fatalf("apply account full sequence: %v", err)
	}
	sqlResult := accountBackfillSnapshot(t, sqlDB)
	dialect := sqlDB.Unsafe(t.Context()).Dialector.Name()
	if err := sqlDB.Unsafe(t.Context()).Exec(account.MigrationSQLForTest(t, dialect, 2)).Error; err != nil {
		t.Fatalf("rerun account SQL backfill: %v", err)
	}
	if rerun := accountBackfillSnapshot(t, sqlDB); !maps.Equal(sqlResult, rerun) {
		t.Fatalf("account SQL backfill is not idempotent: first=%v second=%v", sqlResult, rerun)
	}

	goDB := open(t)
	if err := account.MigrateSchema(t.Context(), goDB); err != nil {
		t.Fatalf("create account auto schema: %v", err)
	}
	seedAccountLegacyMatrix(t, goDB)
	if err := account.BackfillHasPassword(t.Context(), goDB); err != nil {
		t.Fatalf("run account Go backfill: %v", err)
	}
	goResult := accountBackfillSnapshot(t, goDB)
	if err := account.BackfillHasPassword(t.Context(), goDB); err != nil {
		t.Fatalf("rerun account Go backfill: %v", err)
	}
	if rerun := accountBackfillSnapshot(t, goDB); !maps.Equal(goResult, rerun) {
		t.Fatalf("account Go backfill is not idempotent: first=%v second=%v", goResult, rerun)
	}
	if !maps.Equal(sqlResult, goResult) {
		t.Fatalf("account SQL and Go backfills diverge: sql=%v go=%v", sqlResult, goResult)
	}
}

type legacyUserCase struct {
	rid         string
	email       string
	password    string
	hasPassword bool
	deleted     bool
	identity    string
	identityDel bool
	want        bool
}

var legacyUserCases = []legacyUserCase{
	{rid: "usr_legacyA", email: "legacy-positive@matrix.test", password: "hash", want: true},
	{rid: "usr_legacyB", email: "legacy-empty@matrix.test", want: false},
	{rid: "usr_legacyC", email: "legacy-true@matrix.test", password: "hash", hasPassword: true, want: true},
	{rid: "usr_legacyD", email: "legacy-deleted@matrix.test", password: "hash", deleted: true, want: false},
	{rid: "usr_legacyE", email: "legacy-active-id@matrix.test", password: "hash", identity: "idn_legacyE", want: false},
	{rid: "usr_legacyF", email: "legacy-deleted-id@matrix.test", password: "hash", identity: "idn_legacyF", identityDel: true, want: true},
}

func seedAccountLegacyMatrix(t testing.TB, h *db.DB) {
	t.Helper()
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	for i, tc := range legacyUserCases {
		user := &account.User{
			Email: tc.email, PasswordHash: tc.password, HasPassword: tc.hasPassword,
			Name: "Legacy", Active: true,
		}
		user.RID = tc.rid
		if tc.deleted {
			user.DeletedAt = gorm.DeletedAt{Time: now, Valid: true}
			user.DeleteToken = "deletedUser"
		}
		if err := h.Unsafe(t.Context()).Create(user).Error; err != nil {
			t.Fatalf("seed legacy user %s: %v", tc.email, err)
		}
		if tc.identity == "" {
			continue
		}
		identity := &account.Identity{
			UserID: tc.rid, Provider: "legacy", ProviderAccountID: tc.rid,
			Email: tc.email,
		}
		identity.RID = tc.identity
		if tc.identityDel {
			identity.DeletedAt = gorm.DeletedAt{Time: now.Add(time.Duration(i) * time.Second), Valid: true}
			identity.DeleteToken = "deletedIdent"
		}
		if err := h.Unsafe(t.Context()).Create(identity).Error; err != nil {
			t.Fatalf("seed legacy identity %s: %v", tc.email, err)
		}
	}
}

func verifyAccountLegacyMatrix(t testing.TB, h *db.DB) {
	t.Helper()
	got := accountBackfillSnapshot(t, h)
	for _, tc := range legacyUserCases {
		if value, ok := got[tc.email]; !ok || value != tc.want {
			t.Fatalf("account backfill %s = %v present=%v, want %v", tc.email, value, ok, tc.want)
		}
	}
}

func accountBackfillSnapshot(t testing.TB, h *db.DB) map[string]bool {
	t.Helper()
	var rows []struct {
		Email       string
		HasPassword bool
	}
	if err := h.Unsafe(t.Context()).Unscoped().Model(&account.User{}).
		Where("email LIKE ?", "legacy-%@matrix.test").
		Order("email").Scan(&rows).Error; err != nil {
		t.Fatalf("read account backfill rows: %v", err)
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[row.Email] = row.HasPassword
	}
	return out
}

func accountBehaviorTrace(t testing.TB, h *db.DB) testseq.Trace {
	t.Helper()
	ctx := t.Context()
	users := store.New[account.User](h, log.Empty(),
		store.WithQueryFields("id", "email"),
		store.WithUpdateFields("email"),
	)
	identities := store.New[account.Identity](h, log.Empty(),
		store.WithQueryFields("id", "provider", "provider_account_id"),
		store.WithUpdateFields("profile"),
	)
	trace := testseq.Trace{}
	var priorMax uint
	if err := h.Unsafe(ctx).Unscoped().Model(&account.User{}).Select("COALESCE(MAX(id), 0)").Scan(&priorMax).Error; err != nil {
		t.Fatalf("account behavior read prior auto-increment frontier: %v", err)
	}

	first := &account.User{Email: "matrix-user@example.test", PasswordHash: "hash", Name: "A", Active: true}
	if err := users.Create(ctx, first); err != nil {
		t.Fatalf("account behavior create first user: %v", err)
	}
	continued := first.ID > priorMax
	if !continued {
		t.Fatalf("account behavior first new ID did not continue past stored frontier")
	}
	trace = append(trace, testseq.Observation{Step: "user_auto_increment_continuation", OK: continued})
	err := users.Create(ctx, &account.User{Email: first.Email, PasswordHash: "hash", Name: "duplicate", Active: true})
	trace = append(trace, duplicateObservation(t, "user_live_duplicate", err))
	if err := users.Delete(ctx, store.RID(first.RID)); err != nil {
		t.Fatalf("account behavior delete first user: %v", err)
	}
	second := &account.User{Email: first.Email, PasswordHash: "hash", Name: "B", Active: true}
	if err := users.Create(ctx, second); err != nil {
		t.Fatalf("account behavior reclaim user slot: %v", err)
	}
	err = users.Restore(ctx, store.RID(first.RID))
	trace = append(trace, duplicateObservation(t, "user_restore_taken", err))
	if err := users.Delete(ctx, store.RID(second.RID)); err != nil {
		t.Fatalf("account behavior delete second user: %v", err)
	}
	if err := users.Restore(ctx, store.RID(first.RID)); err != nil {
		t.Fatalf("account behavior restore released user slot: %v", err)
	}
	trace = append(trace, testseq.Observation{Step: "user_restore_released", OK: true})

	identityA := &account.Identity{UserID: "usr_matrixOwner", Provider: "matrix", ProviderAccountID: "acct", Email: "matrix@id.test"}
	if err := identities.Create(ctx, identityA); err != nil {
		t.Fatalf("account behavior create first identity: %v", err)
	}
	err = identities.Create(ctx, &account.Identity{UserID: "usr_matrixOther", Provider: "matrix", ProviderAccountID: "acct"})
	trace = append(trace, duplicateObservation(t, "identity_live_duplicate", err))
	if err := identities.Delete(ctx, store.RID(identityA.RID)); err != nil {
		t.Fatalf("account behavior delete first identity: %v", err)
	}
	identityB := &account.Identity{UserID: "usr_matrixOther", Provider: "matrix", ProviderAccountID: "acct"}
	if err := identities.Create(ctx, identityB); err != nil {
		t.Fatalf("account behavior reclaim identity slot: %v", err)
	}
	err = identities.Restore(ctx, store.RID(identityA.RID))
	trace = append(trace, duplicateObservation(t, "identity_restore_taken", err))
	if err := identities.Delete(ctx, store.RID(identityB.RID)); err != nil {
		t.Fatalf("account behavior delete second identity: %v", err)
	}
	if err := identities.Restore(ctx, store.RID(identityA.RID)); err != nil {
		t.Fatalf("account behavior restore released identity slot: %v", err)
	}
	trace = append(trace, testseq.Observation{Step: "identity_restore_released", OK: true})

	ids := make([]uint, 0, 3)
	for i := range 3 {
		user := &account.User{
			Email:        "matrix-auto-" + string(rune('a'+i)) + "@example.test",
			PasswordHash: "hash", Name: "Auto", Active: true,
		}
		if err := users.Create(ctx, user); err != nil {
			t.Fatalf("account behavior auto id %d: %v", i, err)
		}
		ids = append(ids, user.ID)
	}
	strict := ids[0] < ids[1] && ids[1] < ids[2]
	if !strict {
		t.Fatalf("account behavior auto IDs are not strictly increasing")
	}
	trace = append(trace, testseq.Observation{Step: "user_auto_increment", OK: strict, Rank: 3})

	payload := datatypes.JSON(`{"中文":"值","emoji":"🔋","nested":{"nil":null,"list":[1,true]}}`)
	jsonIdentity := &account.Identity{
		UserID: "usr_matrixJson", Provider: "matrix-json", ProviderAccountID: "payload", Profile: payload,
	}
	if err := identities.Create(ctx, jsonIdentity); err != nil {
		t.Fatalf("account behavior create JSON identity: %v", err)
	}
	gotJSON, err := identities.Get(ctx, store.RID(jsonIdentity.RID))
	if err != nil {
		t.Fatalf("account behavior read JSON identity: %v", err)
	}
	trace = append(trace, testseq.Observation{Step: "identity_json", OK: true, JSON: testseq.CanonicalJSON(t, gotJSON.Profile)})

	nullIdentity := &account.Identity{UserID: "usr_matrixNull", Provider: "matrix-json", ProviderAccountID: "null"}
	emptyIdentity := &account.Identity{UserID: "usr_matrixEmpty", Provider: "matrix-json", ProviderAccountID: "empty", Profile: datatypes.JSON(`{}`)}
	if err := identities.Create(ctx, nullIdentity); err != nil {
		t.Fatalf("account behavior create NULL JSON identity: %v", err)
	}
	if err := identities.Create(ctx, emptyIdentity); err != nil {
		t.Fatalf("account behavior create empty JSON identity: %v", err)
	}
	gotNull, err := identities.Get(ctx, store.RID(nullIdentity.RID))
	if err != nil {
		t.Fatalf("account behavior read NULL JSON identity: %v", err)
	}
	gotEmpty, err := identities.Get(ctx, store.RID(emptyIdentity.RID))
	if err != nil {
		t.Fatalf("account behavior read empty JSON identity: %v", err)
	}
	separated := len(gotNull.Profile) == 0 && testseq.CanonicalJSON(t, gotEmpty.Profile) == "{}"
	if !separated {
		t.Fatalf("account behavior SQL NULL and empty JSON object collapsed: null=%q empty=%q", gotNull.Profile, gotEmpty.Profile)
	}
	trace = append(trace, testseq.Observation{Step: "identity_json_null_vs_empty", OK: separated, Business: "null|{}"})

	return trace
}

func duplicateObservation(t testing.TB, step string, err error) testseq.Observation {
	t.Helper()
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("%s error = %v, want store.ErrDuplicate", step, err)
	}
	return testseq.Observation{Step: step, Error: "duplicate"}
}
