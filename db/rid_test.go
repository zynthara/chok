package db

import (
	"strings"
	"testing"
)

func TestNewRID_ValidPrefix(t *testing.T) {
	r := NewRID("pst")
	if !strings.HasPrefix(r, "pst_") || len(r) != len("pst_")+ridIDLen {
		t.Fatalf("got %q", r)
	}
	if p, ok := ParseRID(r); !ok || p != "pst" {
		t.Fatalf("ParseRID(%q) = %q %v", r, p, ok)
	}
}

func TestNewRID_InvalidPrefixPanics(t *testing.T) {
	for _, p := range []string{"", "abcdefghijk", "Post", "1st", "p-t", "p_t"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewRID(%q) must panic", p)
				}
			}()
			NewRID(p)
		}()
	}
}

func TestRandBase62_AlphabetAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		id := randBase62(ridIDLen)
		if len(id) != ridIDLen || strings.Trim(id, ridAlphabet) != "" {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestParseRID(t *testing.T) {
	cases := map[string]struct {
		prefix string
		ok     bool
	}{
		"pst_3fK9aQ2mZx7B":         {"pst", true},
		"3fK9aQ2mZx7B":             {"", true},
		"pst_":                     {"", false},
		"Pst_abc":                  {"", false},
		"pst_ab-c":                 {"", false},
		"":                         {"", false},
		"pst_0123456789abcdefghij": {"", false}, // over 23 bytes
	}
	for in, want := range cases {
		p, ok := ParseRID(in)
		if p != want.prefix || ok != want.ok {
			t.Errorf("ParseRID(%q) = %q %v, want %q %v", in, p, ok, want.prefix, want.ok)
		}
	}
}

func TestValidatePrefix_LowerAlphaNum(t *testing.T) {
	for _, ok := range []string{"a", "pst", "u2", "abcdefghij"} {
		if err := validatePrefix(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}
