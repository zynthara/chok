package store

import (
	"context"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/store/where"
)

// Arch-backlog #17 regression tests: MySQL writes on a UTC baseline,
// pinned on both halves. The driver half (Config.Loc = time.UTC)
// governs the wall clock DATETIME stores and how it parses back; the
// session half (time_zone pinned to +00:00 on every pooled connection)
// governs what the driver cannot reach — SQL-evaluated timestamps like
// the CURRENT_TIMESTAMP soft delete writes into deleted_at, and
// TIMESTAMP-column conversion. Before #17 the driver half rode
// time.Local and the session half rode the server's zone, so
// correctness hung on the deployment environment and the two halves
// forked whenever process and server zones differed.

// TestMySQLUTCBaseline_RoundTripAnyZone pins the driver half: a
// time.Time carrying ANY Go-side zone stores the UTC wall clock and
// reads back as the same instant, in UTC. The stored text is asserted
// against the column itself — a symmetric conversion bug (write +X,
// parse +X) survives a round trip, not this.
func TestMySQLUTCBaseline_RoundTripAnyZone(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	inst := time.Date(2026, 7, 4, 6, 0, 0, 0, time.UTC)
	zones := map[string]*time.Location{
		"utc":       time.UTC,
		"local":     time.Local,
		"east3":     time.FixedZone("east3", 3*3600),
		"kathmandu": time.FixedZone("kathmandu", 5*3600+45*60),
	}
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, zone := range zones {
		if err := s.Create(ctx, &AggSale{Status: name, Qty: 1, Price: 1, Flag: true, At: inst.In(zone)}); err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := gdb.Raw("SELECT DATE_FORMAT(at, '%Y-%m-%d %H:%i:%s') FROM agg_sales WHERE status = ?", name).Row().Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if want := "2026-07-04 06:00:00"; stored != want {
			t.Fatalf("zone %s: stored wall clock = %q, want the UTC wall clock %q", name, stored, want)
		}
		got, err := s.Get(ctx, Where(where.WithFilter("status", name)))
		if err != nil {
			t.Fatal(err)
		}
		if !got.At.Equal(inst) {
			t.Fatalf("zone %s: read back %v, want the instant %v", name, got.At, inst)
		}
		// Location identity, not offset: on a UTC host every wall-clock
		// assertion above degrades to no-signal under a Loc revert
		// (Local and UTC coincide there), and this is then the ONE
		// check that still goes red — the driver reconstructs times
		// with the exact *Location it was configured with, and
		// time.Local is never the time.UTC singleton. Do not soften it
		// to an offset comparison.
		if got.At.Location() != time.UTC {
			t.Fatalf("zone %s: read back in %v, want time.UTC", name, got.At.Location())
		}
	}
}

// TestMySQLUTCBaseline_SoftDeleteSharesDriverBaseline pins the session
// half: time_zone is +00:00 on every pooled connection, so the
// CURRENT_TIMESTAMP that soft delete writes into deleted_at lands on
// the same baseline as the driver-written created_at instead of forking
// onto the server's zone. The wall-clock comparison tolerates clock
// skew (the DB server stamps deleted_at, the app host stamps
// created_at) but not a zone offset.
func TestMySQLUTCBaseline_SoftDeleteSharesDriverBaseline(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sessionTZ string
	if err := gdb.Raw("SELECT @@session.time_zone").Row().Scan(&sessionTZ); err != nil {
		t.Fatal(err)
	}
	// Load-bearing for the dropped-param regression on a UTC host:
	// stock MySQL reports the literal SYSTEM when no session zone was
	// set (never a resolved offset), so the comparison fires wherever
	// the pin is missing. Known blind spot, accepted: a server booted
	// with default_time_zone=+00:00 would report the pinned value with
	// no pin in place — stock images default to SYSTEM.
	if sessionTZ != "+00:00" {
		t.Fatalf("@@session.time_zone = %q, want the pinned +00:00 (the params SET must reach every pooled connection)", sessionTZ)
	}

	if err := s.Create(ctx, &AggSale{Status: "sd", Qty: 1, Price: 1, Flag: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, Where(where.WithFilter("status", "sd"))); err != nil {
		t.Fatal(err)
	}

	var createdText, deletedText string
	row := gdb.Raw("SELECT DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s'), DATE_FORMAT(deleted_at, '%Y-%m-%d %H:%i:%s') FROM agg_sales WHERE status = 'sd'").Row()
	if err := row.Scan(&createdText, &deletedText); err != nil {
		t.Fatal(err)
	}
	parse := func(text string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04:05", text, time.UTC)
		if err != nil {
			t.Fatalf("parse %q: %v", text, err)
		}
		return v
	}
	created, deleted := parse(createdText), parse(deletedText)
	if d := deleted.Sub(created); d < -5*time.Minute || d > 5*time.Minute {
		t.Fatalf("deleted_at %s vs created_at %s: %v apart — the SQL-evaluated and driver baselines have forked", deletedText, createdText, d)
	}
	// And the driver half sits on UTC in absolute terms: read back as
	// UTC, the stored clock must be near now. Under the old Local
	// baseline on a non-UTC host this is off by the whole zone offset.
	if d := time.Since(created); d < -5*time.Minute || d > 5*time.Minute {
		t.Fatalf("created_at %s parsed as UTC is %v from now — not on the UTC baseline", createdText, d)
	}
}
