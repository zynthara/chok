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
//	  max_attempts: 5
//	  retention: 720h
//
// New gives a zero PollInterval, BatchSize, SettleWindow, MaxAttempts,
// RetryBackoff, MaxBackoff, CleanupInterval, StopBudget or HandlerTimeout
// the defaults below, whether the Config comes from a file or from code.
// Enabled and Retention have no zero-value default: a Config built in code
// sets them (a zero Retention keeps the messages forever).
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
	// SettleWindow delays advancement of the scan watermark; it is an
	// optimization, not a transaction deadline. Pending messages behind
	// the watermark are rescanned until durably acknowledged.
	SettleWindow time.Duration `yaml:"settle_window" default:"30s"`
	// MaxAttempts bounds durably recorded handler failures. A crash or
	// failed acknowledgement can repeat an Attempt; it is not a delivery
	// count. A message its function still fails on at the limit is
	// parked in the dead letters (Jobs.Dead) and the topic moves on.
	MaxAttempts int `yaml:"max_attempts" default:"10"`
	// RetryBackoff is how long a worker waits after a message's first
	// failure before it tries the message again; the wait doubles with
	// every failure. The topic's later messages wait with it.
	RetryBackoff time.Duration `yaml:"retry_backoff" default:"1s"`
	// MaxBackoff caps the wait between two tries of a message.
	MaxBackoff time.Duration `yaml:"max_backoff" default:"5m"`
	// Retention deletes, every CleanupInterval, the messages older than
	// Retention that every worker has delivered; 0 keeps them forever,
	// and is what a Config built in code without it gets. The cleanup
	// only deletes acknowledged messages of registered topics below
	// the conservative watermark floor. Unknown topics and pending rows
	// are retained; a process without workers deletes nothing. It never deletes the dead letters.
	Retention time.Duration `yaml:"retention" default:"168h"`
	// CleanupInterval is how often the retention cleanup runs.
	CleanupInterval time.Duration `yaml:"cleanup_interval" default:"1h"`
	// StopBudget is how long stopping waits for runs in flight, after
	// telling them to stop through their context.
	StopBudget time.Duration `yaml:"stop_budget" default:"15s"`
	// HandlerTimeout bounds one delivery: a worker's function gets a
	// context that ends HandlerTimeout after it was handed the message.
	// The worker logs a Warn then and still waits for the function to
	// return, so the next message of the topic never runs beside it; once
	// it returns, the delivery has failed, whatever it returned, and the
	// message is tried again or parked as after any failure. A stop that
	// began before the function returned decides instead, as without a
	// timeout. A zero takes the default, and no value turns it off: set it
	// above the longest a function may take. Periodic jobs have no timeout.
	HandlerTimeout time.Duration `yaml:"handler_timeout" default:"10m" validate:"gte=0"`
}

// The defaults New applies to the zero fields of a Config built in code.
const (
	defaultPollInterval    = time.Second
	defaultBatchSize       = 100
	defaultSettleWindow    = 30 * time.Second
	defaultMaxAttempts     = 10
	defaultRetryBackoff    = time.Second
	defaultMaxBackoff      = 5 * time.Minute
	defaultCleanupInterval = time.Hour
	defaultStopBudget      = 15 * time.Second
	defaultHandlerTimeout  = 10 * time.Minute
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
	if c.MaxAttempts == 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.RetryBackoff == 0 {
		c.RetryBackoff = defaultRetryBackoff
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = defaultMaxBackoff
	}
	if c.CleanupInterval == 0 {
		c.CleanupInterval = defaultCleanupInterval
	}
	if c.StopBudget == 0 {
		c.StopBudget = defaultStopBudget
	}
	if c.HandlerTimeout == 0 {
		c.HandlerTimeout = defaultHandlerTimeout
	}
	return c
}

// Validate checks the values a running process uses; a disabled section
// runs nothing, so it is not checked. It runs when chok.Load reads the
// configuration and again in New, after the validate tags: the one of
// HandlerTimeout refuses a negative value whatever Enabled says.
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
	if c.MaxAttempts < 0 {
		errs = append(errs, fmt.Sprintf("max_attempts must be at least 1 (0 takes the default), got %d", c.MaxAttempts))
	}
	switch {
	case c.RetryBackoff < 0:
		errs = append(errs, fmt.Sprintf("retry_backoff must not be negative, got %s", c.RetryBackoff))
	case c.MaxBackoff < 0:
		errs = append(errs, fmt.Sprintf("max_backoff must not be negative, got %s", c.MaxBackoff))
	default:
		// A zero takes its default in New: compare what will run.
		d := c.withDefaults()
		if d.RetryBackoff > d.MaxBackoff {
			errs = append(errs, fmt.Sprintf("retry_backoff (%s) must not exceed max_backoff (%s)", d.RetryBackoff, d.MaxBackoff))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	slices.Sort(errs)
	return errors.New(strings.Join(errs, "; "))
}
