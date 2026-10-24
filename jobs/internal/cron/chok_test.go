package cron

import (
	"strings"
	"testing"
	"time"
)

// Tests chok adds to the ported ones.

// TestParse_TimeZoneWithoutSchedule: v3.0.1 panicked slicing a zone prefix
// that has no space after it; the port reports an error.
func TestParse_TimeZoneWithoutSchedule(t *testing.T) {
	p := NewParser(SecondOptional | Minute | Hour | Dom | Month | Dow | Descriptor)
	for _, spec := range []string{"CRON_TZ=UTC", "TZ=Asia/Tokyo", "CRON_TZ="} {
		sched, err := p.Parse(spec)
		if err == nil || !strings.Contains(err.Error(), "without a schedule") {
			t.Errorf("%q => %v, want the missing schedule reported", spec, err)
		}
		if sched != nil {
			t.Errorf("%q => schedule %v on error", spec, sched)
		}
	}
	// A zone followed by a schedule still parses.
	sched, err := p.Parse("CRON_TZ=UTC 0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	if got, want := sched.Next(from), time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}
