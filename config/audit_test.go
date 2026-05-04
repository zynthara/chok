package config

import (
	"strings"
	"testing"
	"time"
)

// TestAuditOptions_Validate_DisabledIsAlwaysOK pins the SPEC §8.5
// short-circuit: when Enabled=false the rest of the fields are
// irrelevant and Validate must return nil regardless. Operators
// shouldn't be forced to fill required fields just to keep the
// component disabled.
func TestAuditOptions_Validate_DisabledIsAlwaysOK(t *testing.T) {
	o := &AuditOptions{Enabled: false}
	if err := o.Validate(); err != nil {
		t.Fatalf("disabled AuditOptions should validate clean, got %v", err)
	}
}

func TestAuditOptions_Validate_AcceptsDefaults(t *testing.T) {
	o := &AuditOptions{
		Enabled:         true,
		AsyncBufferSize: 1024,
		DropOnFull:      false,
		RetentionDays:   180,
		PurgeInterval:   24 * time.Hour,
		PurgeBatchSize:  1000,
		EnableAdminAPI:  true,
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("default-shaped AuditOptions should validate clean, got %v", err)
	}
}

// TestAuditOptions_Validate_RejectsNonPositiveFields walks every
// numeric field that must be > 0 and asserts the error message
// names that field — operators should be able to map the failure
// back to the yaml key without reading source.
func TestAuditOptions_Validate_RejectsNonPositiveFields(t *testing.T) {
	base := AuditOptions{
		Enabled:         true,
		AsyncBufferSize: 1024,
		RetentionDays:   180,
		PurgeInterval:   24 * time.Hour,
		PurgeBatchSize:  1000,
	}
	cases := []struct {
		name     string
		mutate   func(*AuditOptions)
		contains string
	}{
		{"AsyncBufferSize=0", func(o *AuditOptions) { o.AsyncBufferSize = 0 }, "async_buffer_size"},
		{"AsyncBufferSize<0", func(o *AuditOptions) { o.AsyncBufferSize = -1 }, "async_buffer_size"},
		{"RetentionDays=0", func(o *AuditOptions) { o.RetentionDays = 0 }, "retention_days"},
		{"RetentionDays<0", func(o *AuditOptions) { o.RetentionDays = -1 }, "retention_days"},
		{"PurgeInterval=0", func(o *AuditOptions) { o.PurgeInterval = 0 }, "purge_interval"},
		{"PurgeInterval<0", func(o *AuditOptions) { o.PurgeInterval = -time.Second }, "purge_interval"},
		{"PurgeBatchSize=0", func(o *AuditOptions) { o.PurgeBatchSize = 0 }, "purge_batch_size"},
		{"PurgeBatchSize<0", func(o *AuditOptions) { o.PurgeBatchSize = -1 }, "purge_batch_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			tc.mutate(&o)
			err := o.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error should name %q, got %v", tc.contains, err)
			}
		})
	}
}
