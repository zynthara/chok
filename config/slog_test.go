package config

import (
	"strings"
	"testing"
)

// TestSlogOptions_Validate_RejectsBlankOutput covers #5: a blank string
// inside Output bypassed validation and reached buildWriter, which would
// then either skip silently or — before round-14 — feed lumberjack a
// blank Filename and write to a temp directory the operator never asked
// for. Validation must catch the configuration error up front.
func TestSlogOptions_Validate_RejectsBlankOutput(t *testing.T) {
	o := &SlogOptions{
		Level:  "info",
		Format: "json",
		Output: []string{""},
	}
	err := o.Validate()
	if err == nil {
		t.Fatal("blank Output entry must fail validation")
	}
	if !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("error should explain blank entry, got %v", err)
	}
}

// TestSlogOptions_Validate_AllBlankFilesIsEmpty covers #5: when every
// Files entry has a blank Path (each LogFileOptions reports "disabled"
// and validates fine), the *effective* sink count is zero. The previous
// len(Files)>0 check accepted this, leaving runtime to silently fall
// back to stdout — surprising for an operator who thinks they wrote a
// file config.
func TestSlogOptions_Validate_AllBlankFilesIsEmpty(t *testing.T) {
	o := &SlogOptions{
		Level:  "info",
		Format: "json",
		Output: nil,
		Files:  []LogFileOptions{{Path: ""}, {Path: ""}},
	}
	err := o.Validate()
	if err == nil {
		t.Fatal("all-blank Files with no Output must fail validation")
	}
	if !strings.Contains(err.Error(), "must not both be empty") {
		t.Fatalf("error should mention both being empty, got %v", err)
	}
}

// TestDatabaseOptions_SelfValidating documents the marker contract: the
// type asserts to SelfValidating so the framework's recursive validator
// stops after Validate succeeds. A regression that drops the marker
// would re-introduce the blog quickstart break (driver=sqlite tripping
// MySQL's `database` requirement).
func TestDatabaseOptions_SelfValidating(t *testing.T) {
	var v Validatable = &DatabaseOptions{Driver: "sqlite", SQLite: SQLiteOptions{Enabled: true, Path: "x.db"}}
	if _, ok := v.(SelfValidating); !ok {
		t.Fatal("*DatabaseOptions must satisfy SelfValidating")
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("driver=sqlite + path set should validate cleanly, got %v", err)
	}
	bad := &DatabaseOptions{Driver: "mysql"}
	if err := bad.Validate(); err == nil {
		t.Fatalf("driver=mysql with empty fields must fail; got %v", err)
	}
}

// TestAccountOptions_LoginRateValidation guards the pair-or-zero rule
// for the new limiter fields. A half-configured limiter (one set, one
// zero) is treated as an operator mistake — silently disabling would
// be a worse outcome than failing fast at startup.
func TestAccountOptions_LoginRateValidation(t *testing.T) {
	const validKey = "01234567890123456789012345678901"
	cases := []struct {
		name      string
		opts      AccountOptions
		wantError bool
	}{
		{
			name: "both_zero_disabled",
			opts: AccountOptions{Enabled: true, SigningKey: validKey},
		},
		{
			name: "both_set_enabled",
			opts: AccountOptions{Enabled: true, SigningKey: validKey, LoginRateWindow: 15 * 60 * 1e9, LoginRateLimit: 10},
		},
		{
			name:      "window_only",
			opts:      AccountOptions{Enabled: true, SigningKey: validKey, LoginRateWindow: 15 * 60 * 1e9},
			wantError: true,
		},
		{
			name:      "limit_only",
			opts:      AccountOptions{Enabled: true, SigningKey: validKey, LoginRateLimit: 10},
			wantError: true,
		},
		{
			name:      "negative_window",
			opts:      AccountOptions{Enabled: true, SigningKey: validKey, LoginRateWindow: -1},
			wantError: true,
		},
		{
			name:      "negative_limit",
			opts:      AccountOptions{Enabled: true, SigningKey: validKey, LoginRateLimit: -1},
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.wantError && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}
