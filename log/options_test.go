package log

import (
	"strings"
	"testing"
)

// The two assertions below rode the v1 config package until M5 moved
// the options type in here (config/ retirement); they pin review-round
// fixes and must not regress across the move.

// A blank string inside Output bypassed validation historically and
// reached buildWriter, which would either skip silently or feed
// lumberjack a blank Filename and write to a temp directory the
// operator never asked for. Validation must catch it up front.
func TestOptions_Validate_RejectsBlankOutput(t *testing.T) {
	o := &Options{
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

// When every Files entry has a blank Path (each FileOptions reports
// "disabled" and validates fine), the *effective* sink count is zero.
// A len-only check accepted this, leaving runtime to silently fall
// back to stdout — surprising for an operator who thinks they wrote a
// file config.
func TestOptions_Validate_AllBlankFilesIsEmpty(t *testing.T) {
	o := &Options{
		Level:  "info",
		Format: "json",
		Output: nil,
		Files:  []FileOptions{{Path: ""}, {Path: ""}},
	}
	err := o.Validate()
	if err == nil {
		t.Fatal("all-blank Files with no Output must fail validation")
	}
	if !strings.Contains(err.Error(), "must not both be empty") {
		t.Fatalf("error should mention both being empty, got %v", err)
	}
}
