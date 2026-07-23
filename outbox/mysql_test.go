package outbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/log"
)

// TestOutboxRelay_MySQLWatermarkRoundTrip pins the rounding-domain
// contract on real MySQL: created_at lands in datetime(3) (stored
// value may differ from the Go-side nanosecond instant), and the
// watermark is persisted from read-back values, so keyset skips and
// the Gte overlap always compare inside the same millisecond domain.
// A drift between domains would show up here as a duplicate delivery
// (watermark below the rows) or a lost row (watermark above them).
func TestOutboxRelay_MySQLWatermarkRoundTrip(t *testing.T) {
	h := dbtest.OpenMySQL(t)
	ctx := context.Background()
	if err := MigrateSchema(ctx, h); err != nil {
		t.Fatal(err)
	}
	c := newCore(h, log.Empty())

	// Enqueued rows carry Go nanosecond timestamps that MySQL rounds;
	// forced same-millisecond ties exercise the id tie-breaker.
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "t", fmt.Sprintf("live%d", i))
	}
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	for i := 1; i <= 5; i++ {
		insertAt(t, c, ts, 0, "t", fmt.Sprintf("tie%d", i))
	}

	cp := &capture{}
	r := mkRelay(t, c, "mysql-rt", cp, 0, 2)
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"tie1", "tie2", "tie3", "tie4", "tie5", "live1", "live2", "live3"}
	wantPayloads(t, cp.payloads(), want)

	w, err := c.states.load(ctx, "mysql-rt")
	if err != nil {
		t.Fatal(err)
	}
	if !w.ok {
		t.Fatal("watermark not persisted")
	}
	// Second and third sweeps: pure keyset skip against the stored
	// millisecond values — any rounding drift would deliver again.
	for i := 0; i < 2; i++ {
		if err := r.run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	wantPayloads(t, cp.payloads(), want)

	// And new rows after the round-trip watermark still flow.
	enqueue(t, c, "t", "after")
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), append(want, "after"))
}

// TestOutboxRelay_MySQLLateCommitRealTx is the MySQL twin of the
// round-3 real-transaction late-commit shape (see lateCommitRealTx).
func TestOutboxRelay_MySQLLateCommitRealTx(t *testing.T) {
	h := dbtest.OpenMySQL(t)
	if err := MigrateSchema(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	lateCommitRealTx(t, newCore(h, log.Empty()))
}
