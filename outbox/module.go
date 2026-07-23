package outbox

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/scheduler"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/store/where"
)

// Options is the "outbox" yaml section.
//
// Reload semantics: batch_size and retention are hot (each sweep reads
// the live snapshot). poll_interval and cleanup_interval are
// restart-only (cron specs are fixed at registration). settle_window
// is restart-only on purpose — it is a correctness bound (§ package
// doc), and shrinking it live would instantly narrow the safety
// window under the feet of in-flight transactions.
type Options struct {
	Enabled bool `mapstructure:"enabled" default:"true"`

	// PollInterval is each relay's sweep period — the upper bound on
	// delivery latency for a healthy relay.
	PollInterval time.Duration `mapstructure:"poll_interval" default:"1s"`

	// BatchSize bounds one scan page (and one cleanup delete batch).
	// Capped at 10000 (the where DSL's MaxPageSize).
	BatchSize int `mapstructure:"batch_size" default:"100" reload:"hot"`

	// SettleWindow is how long a row is considered unsettled: the
	// persisted watermark never advances past rows younger than this,
	// so a transaction that commits within SettleWindow of its Enqueue
	// INSERT can never be skipped. Raise it if enqueueing transactions
	// run longer (the cost is a wider crash-replay window, not higher
	// delivery latency).
	SettleWindow time.Duration `mapstructure:"settle_window" default:"30s"`

	// Retention enables the cleanup sweep when positive: messages that
	// are BOTH older than Retention AND behind every Record relay's
	// watermark (WithRelay — the relays that actually scan
	// outbox_messages; WithRelayFor watermarks track user tables and
	// neither authorise nor block this sweep) are deleted in batches.
	// Zero (the default) keeps rows forever.
	Retention time.Duration `mapstructure:"retention" default:"0" reload:"hot"`

	// CleanupInterval is the cleanup job's sweep period.
	CleanupInterval time.Duration `mapstructure:"cleanup_interval" default:"1h"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.PollInterval <= 0 {
		return fmt.Errorf("outbox: poll_interval must be positive, got %s", o.PollInterval)
	}
	if o.BatchSize < 1 || o.BatchSize > where.MaxPageSize {
		return fmt.Errorf("outbox: batch_size must be in [1, %d], got %d", where.MaxPageSize, o.BatchSize)
	}
	if o.SettleWindow <= 0 {
		return fmt.Errorf("outbox: settle_window must be positive, got %s", o.SettleWindow)
	}
	if o.Retention < 0 {
		return fmt.Errorf("outbox: retention must be >= 0, got %s", o.Retention)
	}
	if o.CleanupInterval <= 0 {
		return fmt.Errorf("outbox: cleanup_interval must be positive, got %s", o.CleanupInterval)
	}
	return nil
}

// Option configures Module at assembly time.
type Option func(*moduleConfig)

type moduleConfig struct {
	relays []relayReg
}

type relayReg struct {
	name string
	// record marks relays that scan outbox_messages (WithRelay). Only
	// their watermarks may authorise the retention sweep to delete
	// messages; WithRelayFor relays track user-owned tables and are
	// excluded from that floor (round-1 review).
	record bool
	build  func(c *core, settle time.Duration, batch func() int) (runner, error)
}

// RelayOption configures one WithRelay registration.
type RelayOption func(*relayCfg)

type relayCfg struct {
	topics []string
}

// OnTopics narrows a WithRelay registration to the given topics (exact
// match, SQL IN). Without it the relay sees every Record. Each relay
// keeps its own watermark either way, so a slow topic-filtered relay
// never holds back the others.
func OnTopics(topics ...string) RelayOption {
	return func(cfg *relayCfg) { cfg.topics = append(cfg.topics, topics...) }
}

// WithRelay registers a named relay over the battery's Record table.
// The name keys the persisted watermark (outbox_relay_state row) and
// the scheduler job ("outbox-relay-<name>") — keep it stable across
// deploys, or the new name starts over from the beginning of the
// table. Registering relays requires the scheduler module: Init fails
// otherwise rather than leaving delivery silently dead.
func WithRelay(name string, handler Handler, opts ...RelayOption) Option {
	return func(mc *moduleConfig) {
		var cfg relayCfg
		for _, o := range opts {
			o(&cfg)
		}
		mc.relays = append(mc.relays, relayReg{
			name:   name,
			record: true,
			build: func(c *core, settle time.Duration, batch func() int) (runner, error) {
				return newRelay[Record](name, HandlerFor[Record](handler), c, cfg, settle, batch)
			},
		})
	}
}

// WithRelayFor registers a relay over a user-owned append-only model —
// the generic escape hatch: the same watermark engine, scanning T's
// table instead of outbox_messages. The rows are the caller's to write
// (through store.NewAppend or Store transactions) and the table is the
// caller's to declare (db.Table); the relay only ever reads it and
// records progress in outbox_relay_state. Topic filtering does not
// apply (it is a Record column); the model's created_at column must
// keep its default name. The relay's watermark plays no part in the
// Retention sweep over outbox_messages — retention for the user table
// stays with the table's owner (this battery never deletes from it).
func WithRelayFor[T db.AppendModeler](name string, handler HandlerFor[T]) Option {
	return func(mc *moduleConfig) {
		mc.relays = append(mc.relays, relayReg{
			name: name,
			build: func(c *core, settle time.Duration, batch func() int) (runner, error) {
				return newRelay[T](name, handler, c, relayCfg{}, settle, batch)
			},
		})
	}
}

// Module returns the outbox component for chok.Use.
func Module(opts ...Option) kernel.Component {
	var mc moduleConfig
	for _, o := range opts {
		o(&mc)
	}
	return &Component{mc: mc}
}

// Component owns the application-wide outbox: the enqueue face, the
// battery schema, and the relay/cleanup scheduler jobs.
type Component struct {
	mc   moduleConfig
	k    kernel.Kernel
	opts atomic.Pointer[Options]
	chok log.Logger

	h    *db.DB
	mode string // db migrate mode captured at Init
	dbc  interface {
		ApplyOwnedMigrations(context.Context, db.Sequence) (*db.ApplyReport, error)
	}

	core         *core
	relays       []runner
	recordNames  []string // relays scanning outbox_messages — the only retention authority
	genericNames []string // WithRelayFor relays — user tables, excluded from the floor
	relayWired   bool     // scheduler present, relay jobs registered
}

// Describe implements kernel.Component.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "outbox",
		ConfigKey: "outbox",
		Options:   Options{},
		Schema: kernel.SchemaOwner{Tables: []string{
			Record{}.TableName(), relayState{}.TableName(), "schema_migrations_chok_outbox",
		}},
		Needs: []kernel.Dep{
			{Kind: "db"},
			{Kind: "scheduler", Optional: true},
			{Kind: "log", Optional: true},
		},
	}
}

// Init captures the db handle, builds the relays and registers the
// scheduler jobs. Table creation belongs to Migrate.
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	c.k = k
	var opts Options
	if err := k.Config().Section("outbox", &opts); err != nil {
		return fmt.Errorf("outbox: decode section: %w", err)
	}
	c.opts.Store(&opts)
	if l, ok := k.Logger().(log.Logger); ok {
		c.chok = l.With("component", "outbox")
	} else {
		c.chok = log.Empty()
	}

	dbc, ok := kernel.Get[interface {
		Handle() *db.DB
		MigrateMode() string
		ApplyOwnedMigrations(context.Context, db.Sequence) (*db.ApplyReport, error)
	}](k, "db")
	if !ok {
		return fmt.Errorf("outbox: db module not available")
	}
	c.h = dbc.Handle()
	if c.h == nil {
		return fmt.Errorf("outbox: db handle not initialised")
	}
	if c.h.ReadOnly() {
		return fmt.Errorf("outbox: db instance is read_only — the outbox requires a writable database")
	}
	c.mode = dbc.MigrateMode()
	c.dbc = dbc
	c.core = newCore(c.h, c.chok)

	// Build relays. batch_size is hot — each sweep reads the live
	// snapshot; settle_window is fixed for the process lifetime.
	batch := func() int { return c.opts.Load().BatchSize }
	seen := make(map[string]struct{}, len(c.mc.relays))
	for _, reg := range c.mc.relays {
		if _, dup := seen[reg.name]; dup {
			return fmt.Errorf("outbox: duplicate relay name %q", reg.name)
		}
		seen[reg.name] = struct{}{}
		r, err := reg.build(c.core, opts.SettleWindow, batch)
		if err != nil {
			return err
		}
		c.relays = append(c.relays, r)
		if reg.record {
			c.recordNames = append(c.recordNames, reg.name)
		} else {
			c.genericNames = append(c.genericNames, reg.name)
		}
	}

	// Delivery needs the scheduler. Registered relays with no
	// scheduler would be a silent half-battery (messages accumulate,
	// nothing delivers) — fail fast instead. An enqueue-only assembly
	// (zero relays) is legitimate staging and only warns.
	sc, haveScheduler := kernel.Get[interface{ Register(scheduler.Job) error }](k, "scheduler")
	if !haveScheduler && len(c.relays) > 0 {
		return fmt.Errorf("outbox: %d relay(s) registered but the scheduler module is absent — assemble chok.Use(scheduler.Module())", len(c.relays))
	}
	if haveScheduler {
		for _, r := range c.relays {
			if err := sc.Register(&relayJob{r: r, interval: opts.PollInterval}); err != nil {
				return fmt.Errorf("outbox: register relay job %q: %w", r.relayName(), err)
			}
		}
		// The cleanup job is always registered so a hot-reloaded
		// retention takes effect without a restart; retention == 0
		// makes each sweep a no-op.
		if err := sc.Register(&cleanupJob{c: c, interval: opts.CleanupInterval}); err != nil {
			return fmt.Errorf("outbox: register cleanup job: %w", err)
		}
		c.relayWired = true
	} else {
		c.chok.Warn("outbox: scheduler module absent — enqueue-only mode; nothing will be delivered or cleaned up in this process")
	}

	c.chok.Info("outbox initialised",
		"relays", len(c.relays),
		"poll_interval", opts.PollInterval,
		"settle_window", opts.SettleWindow,
		"retention", opts.Retention,
		"delivery_wired", c.relayWired,
	)
	return nil
}

// Migrate implements kernel.Migrator, honouring the framework migrate
// mode (SPEC §5.3 — off touches no schema, battery tables included).
func (c *Component) Migrate(ctx context.Context) error {
	if c.mode == db.MigrateOff {
		c.chok.Info("outbox: migrate mode off — outbox schema untouched (operations own DDL)")
		return nil
	}
	if c.mode == db.MigrateVersioned {
		if _, err := c.dbc.ApplyOwnedMigrations(ctx, MigrationSequence()); err != nil {
			return fmt.Errorf("outbox: migrate owned sequence: %w", err)
		}
		return nil
	}
	return MigrateSchema(ctx, c.h)
}

// Reload implements kernel.Reloader for the hot fields (batch_size,
// retention): sweeps read the live snapshot, so applying the change is
// re-decoding into the atomic pointer. Restart-only field changes are
// warned about by the conf diff layer.
func (c *Component) Reload(ctx context.Context) error {
	var opts Options
	if err := c.k.Config().Section("outbox", &opts); err != nil {
		return fmt.Errorf("outbox: decode section: %w", err)
	}
	c.opts.Store(&opts)
	return nil
}

// Close is a no-op: the component owns no goroutines — relays run on
// the scheduler, whose wind-down happens before any Close.
func (c *Component) Close(ctx context.Context) error { return nil }

// Health reports whether the outbox is constructed.
func (c *Component) Health(ctx context.Context) error {
	if c.core == nil {
		return fmt.Errorf("outbox: not initialised")
	}
	return nil
}

// DeliveryWired reports whether relay/cleanup jobs are riding a
// scheduler — false means the assembly is enqueue-only (the Init-time
// warning's queryable counterpart).
func (c *Component) DeliveryWired() bool { return c.relayWired }

// Enqueue implements Enqueuer (see the interface for the contract).
func (c *Component) Enqueue(ctx context.Context, topic string, payload []byte) error {
	if c.core == nil {
		return fmt.Errorf("outbox: not initialised")
	}
	return c.core.Enqueue(ctx, topic, payload)
}

// EnqueueJSON implements Enqueuer.
func (c *Component) EnqueueJSON(ctx context.Context, topic string, v any) error {
	if c.core == nil {
		return fmt.Errorf("outbox: not initialised")
	}
	return c.core.EnqueueJSON(ctx, topic, v)
}

// From returns the assembled outbox's enqueue face. It panics with
// instructions when the component is absent (fail-fast, mirroring
// db.From); handle absence gracefully with
// kernel.Get[outbox.Enqueuer](k, "outbox") instead.
func From(k kernel.Kernel) Enqueuer {
	e, ok := kernel.Get[Enqueuer](k, "outbox")
	if !ok {
		panic("outbox.From: outbox is not available — assemble it with chok.Use(outbox.Module(...)), check the outbox section is enabled, and only call From after Init")
	}
	return e
}

// --- scheduler jobs ----------------------------------------------------

// relayJob adapts one relay to the scheduler.
type relayJob struct {
	r        runner
	interval time.Duration
}

func (j *relayJob) Name() string             { return "outbox-relay-" + j.r.relayName() }
func (j *relayJob) Spec() string             { return "@every " + j.interval.String() }
func (j *relayJob) Policy() scheduler.Policy { return scheduler.PolicySkipIfRunning }
func (j *relayJob) Run(ctx context.Context) error {
	return j.r.run(ctx)
}

// cleanupJob deletes rows every relay has settled past, once they are
// older than the retention window.
type cleanupJob struct {
	c        *Component
	interval time.Duration
}

func (j *cleanupJob) Name() string             { return "outbox-cleanup" }
func (j *cleanupJob) Spec() string             { return "@every " + j.interval.String() }
func (j *cleanupJob) Policy() scheduler.Policy { return scheduler.PolicySkipIfRunning }

func (j *cleanupJob) Run(ctx context.Context) error {
	opts := j.c.opts.Load()
	if opts.Retention <= 0 {
		return nil
	}
	deleted, err := j.c.core.cleanupOnce(ctx, j.c.recordNames, j.c.genericNames, opts.Retention, opts.BatchSize)
	if err != nil {
		return err
	}
	if deleted > 0 {
		j.c.chok.Info("outbox: cleanup swept delivered rows", "deleted", deleted, "retention", opts.Retention)
	}
	return nil
}

// topicScope narrows a Record relay's scan to the given topics.
func topicScope(topics []string) store.ScopeFunc {
	return func(_ context.Context, gdb *gorm.DB) (*gorm.DB, error) {
		return gdb.Where("topic IN ?", topics), nil
	}
}
