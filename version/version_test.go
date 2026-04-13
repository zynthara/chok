package version

import (
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.Version != "dev" {
		t.Fatalf("expected dev, got %s", info.Version)
	}
	if info.GoVersion == "" {
		t.Fatal("GoVersion should not be empty")
	}
}

func TestInfo_String(t *testing.T) {
	info := Get()
	s := info.String()
	if !strings.Contains(s, "dev") {
		t.Fatalf("String() missing version: %s", s)
	}
	if !strings.Contains(s, "go") {
		t.Fatalf("String() missing go version: %s", s)
	}
}
