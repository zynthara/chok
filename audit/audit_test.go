package audit_test

import (
	"encoding/json"
	"testing"

	"gorm.io/datatypes"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/zynthara/chok/v2/audit"
)

// TestLog_TableName pins the storage table name. Auditors and
// downstream CDC pipelines depend on "audit_logs" — silently
// renaming the table would break their queries with no warning.
func TestLog_TableName(t *testing.T) {
	if got := (audit.Log{}).TableName(); got != "audit_logs" {
		t.Errorf("TableName() = %q, want %q", got, "audit_logs")
	}
}

// TestLog_AutoMigrate proves the gorm tag declarations parse and
// produce a working schema on SQLite (the in-test driver). A
// regression where a tag becomes invalid would surface as a
// migration error here, not at production startup.
func TestLog_AutoMigrate(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&audit.Log{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if !db.Migrator().HasTable("audit_logs") {
		t.Fatal("audit_logs table missing after AutoMigrate")
	}
	// Composite index names defined in audit.go — at least one
	// should exist; SQLite reports them via Migrator().HasIndex.
	for _, idx := range []string{"idx_audit_actor_time", "idx_audit_action_time", "idx_audit_resource_time"} {
		if !db.Migrator().HasIndex(&audit.Log{}, idx) {
			t.Errorf("expected composite index %q on audit_logs", idx)
		}
	}
}

// TestLog_RoundTrip writes a representative row through GORM and
// reads it back; pins that the Before/After datatypes.JSON columns
// preserve operator-supplied JSON without dropping fields.
func TestLog_RoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&audit.Log{}); err != nil {
		t.Fatal(err)
	}
	beforeRaw, _ := json.Marshal(map[string]any{"status": "active", "name": "Alice"})
	afterRaw, _ := json.Marshal(map[string]any{"status": "suspended", "name": "Alice"})

	row := audit.Log{
		ID:         "audit_test01abcdef0",
		ActorID:    "usr_admin",
		ActorType:  audit.ActorTypeUser,
		Action:     "user.suspend",
		Result:     audit.ResultSuccess,
		Resource:   "user",
		ResourceID: "usr_alice",
		Before:     datatypes.JSON(beforeRaw),
		After:      datatypes.JSON(afterRaw),
		TraceID:    "abc123",
		RequestID:  "req_xyz",
		Reason:     "policy violation",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("Create: %v", err)
	}

	var got audit.Log
	if err := db.First(&got, "id = ?", row.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.Action != row.Action || got.Resource != row.Resource || got.Reason != row.Reason {
		t.Errorf("scalar fields drifted: got %+v want %+v", got, row)
	}
	var beforeMap map[string]any
	if err := json.Unmarshal(got.Before, &beforeMap); err != nil {
		t.Fatalf("Before unmarshal: %v", err)
	}
	if beforeMap["status"] != "active" {
		t.Errorf("Before.status = %v, want active", beforeMap["status"])
	}
}

// TestEnums_Stable pins the canonical strings — changing them is a
// SPEC-level break (admin UI filters, downstream queries depend on
// these values).
func TestEnums_Stable(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ResultSuccess", audit.ResultSuccess, "success"},
		{"ResultFailure", audit.ResultFailure, "failure"},
		{"ResultDenied", audit.ResultDenied, "denied"},
		{"ActorTypeUser", audit.ActorTypeUser, "user"},
		{"ActorTypeSystem", audit.ActorTypeSystem, "system"},
		{"ActorTypeAPIKey", audit.ActorTypeAPIKey, "api_key"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q (changing this is a SPEC break)", tc.name, tc.got, tc.want)
		}
	}
}
