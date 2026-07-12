package testschema

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBaselineUpdateTarget_AllowlistsDialects(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		path, err := baselineUpdateTarget(`{"dialect":"` + dialect + `"}`)
		if err != nil {
			t.Fatalf("%s: %v", dialect, err)
		}
		if want := filepath.Join("migrations", "baseline", dialect+".json"); path != want {
			t.Fatalf("%s path = %q, want %q", dialect, path, want)
		}
	}
}

func TestBaselineUpdateTarget_RejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"invalid json":      `not json`,
		"empty dialect":     `{"dialect":""}`,
		"unknown dialect":   `{"dialect":"oracle"}`,
		"traversal attempt": `{"dialect":"../../unexpected"}`,
	}
	for name, fingerprint := range cases {
		if _, err := baselineUpdateTarget(fingerprint); err == nil {
			t.Errorf("%s: want error, got nil", name)
		} else if name != "invalid json" && !strings.Contains(err.Error(), "not one of sqlite|mysql|postgres") {
			t.Errorf("%s: want allowlist rejection, got %v", name, err)
		}
	}
}
