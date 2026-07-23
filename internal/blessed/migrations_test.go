package blessed

import (
	"testing"
	"testing/fstest"

	"github.com/zynthara/chok/v2/db"
)

func TestMigrationSequences_MatchReservedKindOwners(t *testing.T) {
	want := map[string]string{
		"account": "github.com/zynthara/chok/v2/account",
		"audit":   "github.com/zynthara/chok/v2/audit",
		"authz":   "github.com/zynthara/chok/v2/authz",
		"outbox":  "github.com/zynthara/chok/v2/outbox",
	}
	for _, seq := range MigrationSequences() {
		if want[seq.Kind()] != seq.Owner() {
			t.Fatalf("reserved owner for %s = %q, want %q", seq.Kind(), seq.Owner(), want[seq.Kind()])
		}
		delete(want, seq.Kind())
	}
	if len(want) != 0 {
		t.Fatalf("reserved sequence kinds missing from blessed inventory: %v", want)
	}

	assets := fstest.MapFS{}
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		assets[dialect+"/0001_init.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;")}
	}
	for _, kind := range []string{"account", "audit", "authz", "outbox"} {
		if _, err := db.OwnedSequence(kind, assets, db.Baseline{}, db.SequenceOwner("example.com/not-chok/component")); err == nil {
			t.Fatalf("reserved kind %s accepted a third-party owner", kind)
		}
	}
}
