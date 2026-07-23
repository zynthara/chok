package outbox

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/zynthara/chok/v2/db"
)

// Baseline fingerprints under migrations/baseline/ are generated from a
// fresh AutoMigrate database; the regeneration recipe (CHOK_UPDATE_BASELINES
// two-pass flow) is documented on account's migrationAssets.
//
//go:embed migrations/*/*.sql migrations/baseline/*.json
var migrationAssets embed.FS

var migrationSequence = mustMigrationSequence()

// MigrationSequence returns the outbox's immutable, dialect-resolved
// migration history. Runtime modules and the chok CLI consume this same
// descriptor.
func MigrationSequence() db.Sequence { return migrationSequence }

func mustMigrationSequence() db.Sequence {
	root, err := fs.Sub(migrationAssets, "migrations")
	if err != nil {
		panic(err)
	}
	fingerprints := make(map[string]string, 3)
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		raw, err := fs.ReadFile(root, "baseline/"+dialect+".json")
		if err != nil {
			panic(err)
		}
		fingerprints[dialect] = strings.TrimSpace(string(raw))
	}
	seq, err := db.OwnedSequence("outbox", root, db.Baseline{
		EquivalentVersion: 1,
		Tables:            []string{Record{}.TableName(), relayState{}.TableName()},
		Fingerprints:      fingerprints,
	}, db.SequenceOwner("github.com/zynthara/chok/v2/outbox"))
	if err != nil {
		panic(err)
	}
	return seq
}

// scanIndexName is the composite (created_at, id) index behind the
// relay's per-tick predicate — the AppendOnlyModel base deliberately
// carries no index, and an unindexed created_at range scan would walk
// the whole table every poll_interval.
const scanIndexName = "idx_outbox_messages_scan"

// MigrateSchema creates the outbox tables — the migrate:auto primitive
// (versioned mode uses MigrationSequence instead; kernel-less
// embedders can call it directly). Record rides the declared-spec door
// (db.Table validates the append model); relayState is not a chok
// model and enters through db.ForeignTable, the door for battery-shaped
// tables with their own primary key.
func MigrateSchema(ctx context.Context, h *db.DB) error {
	if err := h.Migrate(ctx, db.Table(&Record{}), db.ForeignTable(&relayState{})); err != nil {
		return fmt.Errorf("outbox: migrate tables: %w", err)
	}
	gdb := h.Unsafe(ctx)
	if !gdb.Migrator().HasIndex(&Record{}, scanIndexName) {
		ddl := fmt.Sprintf("CREATE INDEX %s ON %s (created_at, id)", scanIndexName, Record{}.TableName())
		if err := gdb.Exec(ddl).Error; err != nil {
			return fmt.Errorf("outbox: create scan index: %w", err)
		}
	}
	return nil
}
