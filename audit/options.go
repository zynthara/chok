package audit

import (
	"fmt"
	"time"
)

// Options is the "audit" yaml section. Audit log is the
// compliance-grade record of "who did what to which resource";
// distinct from access log (HTTP traffic) and metrics (aggregate
// counters). Disabled by default — enable when retention and
// queryability are explicit product requirements (SPEC §6 keeps the
// v1 opt-in default for this one module).
//
// Reload semantics: retention_days and purge_batch_size are hot (the
// purge job reads the live values each sweep). purge_interval is
// restart-only — the cron spec is fixed at registration. Buffer
// sizing and overflow behaviour are restart-only (channel capacity
// is not resizable mid-flight; flipping drop semantics live would
// surprise hot-path callers). enable_admin_api is restart-only
// (routes mount once).
type Options struct {
	Enabled bool `mapstructure:"enabled" default:"false"`

	// AsyncBufferSize is the in-memory channel capacity feeding the
	// background sink goroutine. When the channel saturates, behaviour
	// is governed by DropOnFull.
	AsyncBufferSize int `mapstructure:"async_buffer_size" default:"1024"`

	// DropOnFull controls overflow behaviour: false = block the calling
	// goroutine until space frees (use when audit is non-negotiable for
	// compliance), true = drop the entry and increment a counter (use
	// when audit must never delay business requests).
	DropOnFull bool `mapstructure:"drop_on_full" default:"false"`

	// RetentionDays caps how long records survive before the purge
	// job deletes them.
	RetentionDays int `mapstructure:"retention_days" default:"180" reload:"hot"`

	// PurgeInterval is how often the purge job sweeps. Requires the
	// scheduler module; without it purge is disabled (with a startup
	// note) and retention is unenforced.
	PurgeInterval time.Duration `mapstructure:"purge_interval" default:"24h"`

	// PurgeBatchSize bounds delete batch size to keep purge from
	// holding row locks long enough to stall INSERT.
	PurgeBatchSize int `mapstructure:"purge_batch_size" default:"1000" reload:"hot"`

	// EnableAdminAPI mounts GET /audit/logs (gated by
	// middleware.RequireAuthz("audit", "read")) for admin UIs and
	// compliance review. Defaults to true; turn off in headless
	// deployments that scrape the table directly.
	EnableAdminAPI bool `mapstructure:"enable_admin_api" default:"true"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.AsyncBufferSize <= 0 {
		return fmt.Errorf("audit: async_buffer_size must be > 0")
	}
	if o.RetentionDays <= 0 {
		return fmt.Errorf("audit: retention_days must be > 0")
	}
	if o.PurgeInterval <= 0 {
		return fmt.Errorf("audit: purge_interval must be > 0")
	}
	if o.PurgeBatchSize <= 0 {
		return fmt.Errorf("audit: purge_batch_size must be > 0")
	}
	return nil
}
