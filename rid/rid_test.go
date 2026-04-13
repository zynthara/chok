package rid

import (
	"strings"
	"testing"
)

func TestNew_ValidPrefix(t *testing.T) {
	r := New("usr")
	if !strings.HasPrefix(r, "usr_") {
		t.Fatalf("expected usr_ prefix, got %s", r)
	}
	// Total length: 3 + 1 + 12 = 16 ≤ 23.
	if len(r) != 16 {
		t.Fatalf("expected length 16, got %d (%s)", len(r), r)
	}
}

func TestNew_InvalidPrefix_Empty(t *testing.T) {
	defer expectPanic(t, "empty prefix")
	New("")
}

func TestNew_InvalidPrefix_TooLong(t *testing.T) {
	defer expectPanic(t, "prefix > 10")
	New("abcdefghijk") // 11 chars
}

func TestNew_InvalidPrefix_UpperCase(t *testing.T) {
	defer expectPanic(t, "uppercase")
	New("Usr")
}

func TestNew_InvalidPrefix_StartsWithDigit(t *testing.T) {
	defer expectPanic(t, "starts with digit")
	New("1usr")
}

func TestNew_InvalidPrefix_SpecialChar(t *testing.T) {
	defer expectPanic(t, "special char")
	New("us-r")
}

func TestNewWithLength_Valid(t *testing.T) {
	r := NewWithLength("ab", 5)
	if !strings.HasPrefix(r, "ab_") {
		t.Fatalf("expected ab_ prefix, got %s", r)
	}
	if len(r) != 8 { // 2 + 1 + 5
		t.Fatalf("expected length 8, got %d", len(r))
	}
}

func TestNewWithLength_TotalExceeds23_Panics(t *testing.T) {
	defer expectPanic(t, "total > 23")
	// prefix=10 + 1 + n=13 = 24 > 23
	NewWithLength("abcdefghij", 13)
}

func TestNewWithLength_ZeroLength_Panics(t *testing.T) {
	defer expectPanic(t, "n < 1")
	NewWithLength("usr", 0)
}

func TestNewRaw(t *testing.T) {
	r := NewRaw()
	if len(r) != 12 {
		t.Fatalf("expected length 12, got %d", len(r))
	}
	if strings.Contains(r, "_") {
		t.Fatalf("raw RID should not contain underscore: %s", r)
	}
}

func TestNewRaw_Uniqueness(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		r := NewRaw()
		if seen[r] {
			t.Fatalf("duplicate raw RID: %s", r)
		}
		seen[r] = true
	}
}

func TestParse_Valid(t *testing.T) {
	prefix, id, err := Parse("usr_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "usr" || id != "abc123" {
		t.Fatalf("got prefix=%q id=%q", prefix, id)
	}
}

func TestParse_NoSeparator(t *testing.T) {
	_, _, err := Parse("nounderscore")
	if err == nil {
		t.Fatal("expected error for missing separator")
	}
}

func TestPrefix(t *testing.T) {
	if got := Prefix("usr_abc"); got != "usr" {
		t.Fatalf("expected usr, got %s", got)
	}
	if got := Prefix("nounderscore"); got != "" {
		t.Fatalf("expected empty, got %s", got)
	}
}

func TestHasPrefix(t *testing.T) {
	if !HasPrefix("usr_abc123", "usr") {
		t.Fatal("expected true")
	}
	if HasPrefix("usr_abc123", "us") {
		t.Fatal("expected false for partial prefix")
	}
	if HasPrefix("u", "usr") {
		t.Fatal("expected false for short RID")
	}
}

func TestValidatePrefix_LowerAlphaNum(t *testing.T) {
	// Valid: lowercase letter start, lowercase + digits after.
	if err := ValidatePrefix("a1b2", 12); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	// Invalid: uppercase.
	if err := ValidatePrefix("Abc", 12); err == nil {
		t.Fatal("expected error for uppercase")
	}
}

func expectPanic(t *testing.T, context string) {
	t.Helper()
	if r := recover(); r == nil {
		t.Fatalf("expected panic (%s)", context)
	}
}
