package handler

import (
	"net/url"
	"testing"
	"time"
)

type formTarget struct {
	Name     string        `form:"name"`
	Page     int           `form:"page"`
	Ratio    float64       `form:"ratio"`
	Active   bool          `form:"active"`
	Wait     time.Duration `form:"wait"`
	Since    time.Time     `form:"since"`
	Tags     []string      `form:"tags"`
	Counts   []int         `form:"counts"`
	OptSize  *int          `form:"opt_size"`
	Skipped  string        `form:"-"`
	Untagged string
	WithOpts string `form:"aliased,omitempty"`
}

// FormInner is exported: fields promoted through an unexported
// embedded type are read-only to reflect (gin skipped them too).
type FormInner struct {
	Inner string `form:"inner"`
}

type formEmbedded struct {
	FormInner
	Own string `form:"own"`
}

func TestDecodeForm_AllSupportedKinds(t *testing.T) {
	v := url.Values{
		"name":     {"alice"},
		"page":     {"3"},
		"ratio":    {"1.5"},
		"active":   {"true"},
		"wait":     {"1500ms"},
		"since":    {"2026-07-03T10:00:00Z"},
		"tags":     {"a", "b"},
		"counts":   {"1", "2", "3"},
		"opt_size": {"9"},
		"aliased":  {"opt"},
	}
	var out formTarget
	if err := decodeForm(v, &out, "form"); err != nil {
		t.Fatalf("decodeForm: %v", err)
	}
	if out.Name != "alice" || out.Page != 3 || out.Ratio != 1.5 || !out.Active {
		t.Fatalf("scalars wrong: %+v", out)
	}
	if out.Wait != 1500*time.Millisecond {
		t.Fatalf("duration wrong: %v", out.Wait)
	}
	if out.Since.UTC().Hour() != 10 {
		t.Fatalf("time wrong: %v", out.Since)
	}
	if len(out.Tags) != 2 || len(out.Counts) != 3 || out.Counts[2] != 3 {
		t.Fatalf("slices wrong: %+v", out)
	}
	if out.OptSize == nil || *out.OptSize != 9 {
		t.Fatalf("pointer wrong: %v", out.OptSize)
	}
	if out.WithOpts != "opt" {
		t.Fatalf("tag options must be stripped: %+v", out)
	}
}

func TestDecodeForm_ScalarTakesFirstValue(t *testing.T) {
	var out formTarget
	if err := decodeForm(url.Values{"page": {"7", "8"}}, &out, "form"); err != nil {
		t.Fatal(err)
	}
	if out.Page != 7 {
		t.Fatalf("scalar must take the first value, got %d", out.Page)
	}
}

func TestDecodeForm_SkipsDashAndUntagged(t *testing.T) {
	var out formTarget
	err := decodeForm(url.Values{"-": {"x"}, "Untagged": {"y"}, "untagged": {"y"}}, &out, "form")
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != "" || out.Untagged != "" {
		t.Fatalf("dash/untagged fields must not bind: %+v", out)
	}
}

func TestDecodeForm_EmbeddedStructs(t *testing.T) {
	var out formEmbedded
	if err := decodeForm(url.Values{"inner": {"i"}, "own": {"o"}}, &out, "form"); err != nil {
		t.Fatal(err)
	}
	if out.Inner != "i" || out.Own != "o" {
		t.Fatalf("embedded binding wrong: %+v", out)
	}
}

func TestDecodeForm_TypeErrorsSurface(t *testing.T) {
	cases := []url.Values{
		{"page": {"NaN"}},
		{"active": {"maybe"}},
		{"wait": {"fast"}},
		{"since": {"yesterday"}},
	}
	for _, v := range cases {
		var out formTarget
		if err := decodeForm(v, &out, "form"); err == nil {
			t.Fatalf("expected error for %v", v)
		}
	}
}

func TestDecodeForm_NonStructTargetIsNoop(t *testing.T) {
	m := map[string]any{}
	if err := decodeForm(url.Values{"x": {"1"}}, &m, "form"); err != nil {
		t.Fatalf("non-struct target must be a no-op, got %v", err)
	}
}

func TestDecodeForm_MissingKeysLeaveZeroValues(t *testing.T) {
	out := formTarget{Page: 42}
	if err := decodeForm(url.Values{}, &out, "form"); err != nil {
		t.Fatal(err)
	}
	if out.Page != 42 {
		t.Fatalf("absent keys must not touch fields, got %d", out.Page)
	}
}
