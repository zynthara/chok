package jobs

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zynthara/chok/internal/dbtest"
)

func TestMigration_AdoptsOnlyDurableLegacyEvidence(t *testing.T) {
	cfg := versionedConfig(t)
	d := dbtest.OpenConfig(t, cfg)
	path := dbtest.Driver() + "/20261025000001_create_jobs_tables.sql"
	body, err := fs.ReadFile(migrations(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterSchema("jobs", nil, fstest.MapFS{path: {Data: body}}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	for _, id := range []uint64{1, 2, 3} {
		if err := d.Unsafe(t.Context()).Table("chok_jobs_messages").Create(map[string]any{"id": id, "topic": "upgrade", "created_at": at, "payload": []byte("1"), "attempts": 0}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Unsafe(t.Context()).Create(&deliveredRow{Topic: "upgrade", MessageID: 1, CreatedAt: at}).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.Unsafe(t.Context()).Create(&deadRow{ID: 2, Topic: "upgrade", CreatedAt: at, ParkedAt: at, Attempts: 1, Payload: []byte("1")}).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.Unsafe(t.Context()).Create(&watermarkRow{Topic: "upgrade", WatermarkAt: at.Add(time.Minute), WatermarkID: 3, UpdatedAt: at}).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	upgraded := dbtest.OpenConfig(t, cfg)
	registerJobs(t, upgraded)
	c := newCore(upgraded, testLogger(t))
	var ids []uint64
	r, err := newRelay("upgrade", func(_ context.Context, m rawMessage) error { ids = append(ids, m.ID); return nil }, c, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 3 {
		t.Fatalf("legacy completion evidence: delivered %v, want only unconfirmed 3", ids)
	}
	// The parked original stays complete after its administrative row is removed.
	j := &Jobs{core: c}
	if err := j.Discard(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	r, _ = newRelay("upgrade", func(_ context.Context, m rawMessage) error { ids = append(ids, m.ID); return nil }, c, 0, 10)
	if err := r.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("terminal original revived: %v", ids)
	}
}
