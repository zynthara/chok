package jobs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Config is the jobs section of an application's configuration:
//
//	jobs:
//	  enabled: true
//	  poll_interval: 1s
//	  retention: 168h
//
// New gives a zero PollInterval, BatchSize, SettleWindow, CleanupInterval
// or StopBudget the defaults below, whether the Config comes from a file
// or from code. Enabled has no zero-value default: a Config built in code
// sets it.
type Config struct {
	// Enabled runs the periodic jobs and the workers in this process.
	// False makes an enqueue-only process: Enqueue works, nothing runs.
	// Run the workers in one process (one replica) of a deployment.
	Enabled bool `yaml:"enabled" default:"true"`
	// PollInterval is how often each worker sweeps its topic for new
	// messages: the delivery latency while it keeps up.
	PollInterval time.Duration `yaml:"poll_interval" default:"1s"`
	// BatchSize is how many messages a sweep reads per query, and how
	// many the cleanup deletes per statement; a sweep reads at most 100
	// batches, the rest wait for the next one. At most 10000.
	BatchSize int `yaml:"batch_size" default:"100"`
	// SettleWindow bounds how long a transaction that calls Enqueue may
	// take to commit: a worker's watermark never passes messages younger
	// than this, so a late commit is still delivered. Raise it if
	// enqueueing transactions run longer, and keep the clocks of the
	// processes that enqueue in sync.
	SettleWindow time.Duration `yaml:"settle_window" default:"30s"`
	// Retention deletes, every CleanupInterval, the messages older than
	// Retention that every worker has delivered; 0 keeps them forever.
	// The cleanup goes by the watermarks of the workers of its own
	// process: one without workers deletes nothing.
	Retention time.Duration `yaml:"retention"`
	// CleanupInterval is how often the retention cleanup runs.
	CleanupInterval time.Duration `yaml:"cleanup_interval" default:"1h"`
	// StopBudget is how long stopping waits for runs in flight, after
	// telling them to stop through their context.
	StopBudget time.Duration `yaml:"stop_budget" default:"15s"`
}

// The defaults New applies to the zero fields of a Config built in code.
const (
	defaultPollInterval    = time.Second
	defaultBatchSize       = 100
	defaultSettleWindow    = 30 * time.Second
	defaultCleanupInterval = time.Hour
	defaultStopBudget      = 15 * time.Second
	maxBatchSize           = 10_000
)

// withDefaults fills what a Config built in code leaves at zero; one from
// chok.Load already carries every default.
func (c Config) withDefaults() Config {
	if c.PollInterval == 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.BatchSize == 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.SettleWindow == 0 {
		c.SettleWindow = defaultSettleWindow
	}
	if c.CleanupInterval == 0 {
		c.CleanupInterval = defaultCleanupInterval
	}
	if c.StopBudget == 0 {
		c.StopBudget = defaultStopBudget
	}
	return c
}

// Validate checks the values a running process uses; a disabled section
// runs nothing, so it is not checked. It runs when chok.Load reads the
// configuration and again in New.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	var errs []string
	for name, d := range map[string]time.Duration{
		"poll_interval": c.PollInterval, "settle_window": c.SettleWindow,
		"cleanup_interval": c.CleanupInterval, "stop_budget": c.StopBudget,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Sprintf("%s must be positive, got %s", name, d))
		}
	}
	if c.BatchSize < 1 || c.BatchSize > maxBatchSize {
		errs = append(errs, fmt.Sprintf("batch_size must be between 1 and %d, got %d", maxBatchSize, c.BatchSize))
	}
	if c.Retention < 0 {
		errs = append(errs, fmt.Sprintf("retention must not be negative (0 keeps messages forever), got %s", c.Retention))
	}
	if len(errs) == 0 {
		return nil
	}
	slices.Sort(errs)
	return errors.New(strings.Join(errs, "; "))
}
