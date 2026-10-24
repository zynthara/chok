package jobs

import (
	"fmt"
	"testing"
	"time"

	"github.com/zynthara/chok/v3/internal/dbtest"
)

// The three MySQL tests of v2's outbox/mysql_test.go, under their v2
// names. v2 ran them only against MySQL; here each runs on every lane its
// shape is meaningful on, which for two of them is every lane: the
// rounding-domain contract holds for any precision below Go's
// nanoseconds (Postgres keeps microseconds), and the round-4 shape is
// dialect-agnostic. The late-commit twin needs concurrent writers.

// TestOutboxRelay_MySQLWatermarkRoundTrip pins the rounding-domain
// contract on real MySQL: created_at lands in datetime(3) (stored value
// may differ from the Go-side nanosecond instant), and the watermark is
// persisted from read-back values, so keyset skips and the >= overlap
// always compare inside the same millisecond domain. A drift between
// domains would show up here as a duplicate delivery (watermark below the
// rows) or a lost row (watermark above them).
func TestOutboxRelay_MySQLWatermarkRoundTrip(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()

	// Enqueued rows carry Go nanosecond timestamps that MySQL rounds;
	// forced same-millisecond ties exercise the id tie-breaker.
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "mysql-rt", fmt.Sprintf("live%d", i))
	}
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	for i := 1; i <= 5; i++ {
		insertAt(t, c, ts, 0, "mysql-rt", fmt.Sprintf("tie%d", i))
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
	for range 2 {
		if err := r.run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	wantPayloads(t, cp.payloads(), want)

	// And new rows after the round-trip watermark still flow.
	enqueue(t, c, "mysql-rt", "after")
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), append(want, "after"))
}

// TestOutboxRelay_MySQLLateCommitRealTx is the MySQL twin of the round-3
// real-transaction late-commit shape (see lateCommitRealTx); like
// TestOutboxRelay_LateCommitRealTx it runs wherever writers can commit
// concurrently.
func TestOutboxRelay_MySQLLateCommitRealTx(t *testing.T) {
	if dbtest.Driver() == "sqlite" {
		t.Skip("SQLite has one writer: a second transaction waits for the open one, so no commit can land behind the relay's cursor")
	}
	lateCommitRealTx(t, openCore(t))
}

// TestOutboxRelay_MySQLWideSettledTie runs the round-4 starvation shape;
// on MySQL, datetime(3) rounding makes tie groups wider than a sweep
// budget the realistic path.
func TestOutboxRelay_MySQLWideSettledTie(t *testing.T) {
	wideSettledTie(t, openCore(t))
}
