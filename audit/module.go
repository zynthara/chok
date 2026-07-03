package audit

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/middleware"
	"github.com/zynthara/chok/v2/scheduler"
)

// Module returns the audit component for chok.Use. It owns the async
// DB sink, the retention purge job (soft scheduler dependency) and
// the admin query API (fail-closed behind RequireAuthz).
//
// Descriptor note: the component does NOT declare a Needs edge to
// authz even though the admin API is authz-gated — authz soft-depends
// on audit (decision audit must be live before bootstrap seeding), so
// a reciprocal edge would be a topology cycle. The admin API's authz
// relationship is request-time: middleware.RequireAuthz reads the
// request context and rejects every request when no Authorizer is
// attached (SPEC §6 deviation recorded in the M4 mini-SPEC).
func Module() kernel.Component { return &Component{} }

// Component owns the application-wide audit sink.
type Component struct {
	k      kernel.Kernel
	opts   atomic.Pointer[Options] // hot fields re-read by the purge job
	logger *DBLogger
	chok   log.Logger

	h    *db.DB
	mode string // db migrate mode captured at Init

	purgeWired bool // scheduler present, job registered
}

// Describe implements kernel.Component.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "audit",
		ConfigKey: "audit",
		Options:   Options{},
		Needs: []kernel.Dep{
			{Kind: "db"},
			{Kind: "scheduler", Optional: true},
			{Kind: "http", Optional: true},
			{Kind: "log", Optional: true},
		},
	}
}

// Init captures the db handle, starts the async sink and registers
// the purge job when the scheduler module is assembled. Table
// creation belongs to Migrate.
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	c.k = k
	var opts Options
	if err := k.Config().Section("audit", &opts); err != nil {
		return fmt.Errorf("audit: decode section: %w", err)
	}
	c.opts.Store(&opts)
	if l, ok := k.Logger().(log.Logger); ok {
		c.chok = l.With("component", "audit")
	} else {
		c.chok = log.Empty()
	}

	dbc, ok := kernel.Get[interface {
		Handle() *db.DB
		MigrateMode() string
	}](k, "db")
	if !ok {
		return fmt.Errorf("audit: db module not available")
	}
	c.h = dbc.Handle()
	if c.h == nil {
		return fmt.Errorf("audit: db handle not initialised")
	}
	c.mode = dbc.MigrateMode()

	// The sink worker outlives Init (Registry cancels the Init ctx on
	// return); its lifetime is bounded by Close. WithoutCancel keeps
	// correlation values without inheriting cancellation.
	c.logger = NewDBLogger(
		context.WithoutCancel(ctx),
		c.h.Unsafe(context.WithoutCancel(ctx)),
		opts.AsyncBufferSize,
		opts.DropOnFull,
		c.chok,
	)

	// Purge wiring (7.D): ride the scheduler when it's assembled;
	// its absence disables retention enforcement with an explicit
	// note — the sink itself stays fully functional.
	if sc, ok := kernel.Get[interface{ Register(scheduler.Job) error }](k, "scheduler"); ok {
		if err := sc.Register(&purgeJob{c: c, interval: opts.PurgeInterval}); err != nil {
			return fmt.Errorf("audit: register purge job: %w", err)
		}
		c.purgeWired = true
	} else {
		c.chok.Warn("audit: scheduler module absent — retention purge disabled; " +
			"audit_logs will grow unbounded (assemble chok.Use(scheduler.Module()) to enforce retention_days)")
	}

	c.chok.Info("audit sink started",
		"async_buffer_size", opts.AsyncBufferSize,
		"drop_on_full", opts.DropOnFull,
		"retention_days", opts.RetentionDays,
		"purge_enabled", c.purgeWired,
	)
	return nil
}

// Migrate implements kernel.Migrator: create audit_logs, honouring
// the framework migrate mode (SPEC §5.3 — off touches no schema,
// battery tables included; a missing table then surfaces on the
// first write, and the authz switch-on probe turns that into a
// startup failure for audit-mandatory deployments).
func (c *Component) Migrate(ctx context.Context) error {
	if c.mode == db.MigrateOff {
		c.chok.Info("audit: migrate mode off — audit_logs schema untouched (operations own DDL)")
		return nil
	}
	// audit_logs is rid-keyed with its own shape (no db.Model
	// embedding), so it rides raw AutoMigrate through the sanctioned
	// escape hatch rather than the db.Table spec path.
	if err := c.h.Unsafe(ctx).AutoMigrate(&Log{}); err != nil {
		return fmt.Errorf("audit: migrate audit_logs: %w", err)
	}
	return nil
}

// Mount implements kernel.Mounter: the admin query API. Fail-closed:
// RequireAuthz rejects every request when no principal (401) or no
// Authorizer (500) is attached to the request context — an assembly
// without the authz module therefore serves no audit data, ever.
func (c *Component) Mount(r kernel.Router) error {
	if !c.opts.Load().EnableAdminAPI {
		return nil
	}
	r.Handle(http.MethodGet, "/audit/logs",
		handler.HandleRequest(c.queryLogs,
			handler.WithSummary("Query audit logs"),
			handler.WithTags("audit"),
		),
		middleware.RequireAuthz("audit", "read"),
	)
	return nil
}

// Reload implements kernel.Reloader for the hot fields
// (retention_days, purge_batch_size): the purge job reads the live
// snapshot each sweep, so applying the change is re-decoding into the
// atomic pointer. Restart-only field changes are warned about by the
// conf diff layer — no hand-written warnings here (SPEC §3.4).
func (c *Component) Reload(ctx context.Context) error {
	var opts Options
	if err := c.k.Config().Section("audit", &opts); err != nil {
		return fmt.Errorf("audit: decode section: %w", err)
	}
	c.opts.Store(&opts)
	return nil
}

// Close drains and stops the async sink.
func (c *Component) Close(ctx context.Context) error {
	if c.logger == nil {
		return nil
	}
	c.logger.Close()
	return nil
}

// Logger returns the blessed audit Logger. nil before Init.
func (c *Component) Logger() Logger {
	if c.logger == nil {
		return nil
	}
	return c.logger
}

// Stats surfaces the async sink counters (pending/dropped/written/
// failed) for debug and metrics surfaces.
func (c *Component) Stats() Stats {
	if c.logger == nil {
		return Stats{}
	}
	return c.logger.Stats()
}

// PurgeEnabled reports whether the retention purge job is riding a
// scheduler — false means the scheduler module is absent and
// retention_days is unenforced (the Init-time warning's queryable
// counterpart).
func (c *Component) PurgeEnabled() bool { return c.purgeWired }

// --- primitive emit face (authz.AuditSink shape) -----------------------

// LogEvent enqueues an audit entry described by primitive values on
// the async sink. It is the cross-battery emit surface: consumers
// assert it structurally (no audit import) — the authz module's
// policy-mutation hook rides it.
func (c *Component) LogEvent(ctx context.Context, action, resource, result string, metadata map[string]string) error {
	if c.logger == nil {
		return fmt.Errorf("audit: sink not initialised")
	}
	c.logger.Log(ctx, c.entryFrom(ctx, action, resource, result, metadata))
	return nil
}

// LogEventSync writes an audit entry through to storage before
// returning — the probe-grade variant (authz uses it to fail startup
// when decision audit cannot land entries).
func (c *Component) LogEventSync(ctx context.Context, action, resource, result string, metadata map[string]string) error {
	if c.logger == nil {
		return fmt.Errorf("audit: sink not initialised")
	}
	return c.logger.LogSync(ctx, c.entryFrom(ctx, action, resource, result, metadata))
}

func (c *Component) entryFrom(ctx context.Context, action, resource, result string, metadata map[string]string) Entry {
	e := Entry{Action: action, Resource: resource, Result: result}
	if len(metadata) > 0 {
		md := make(map[string]any, len(metadata))
		for k, v := range metadata {
			md[k] = v
		}
		e.Metadata = md
	}
	return MergeContext(ctx, e)
}

// --- admin query API -----------------------------------------------------

type queryLogsRequest struct {
	ActorID    string `form:"actor_id"`
	Resource   string `form:"resource"`
	ResourceID string `form:"resource_id"`
	Action     string `form:"action"`
	Result     string `form:"result"`
	From       string `form:"from"` // RFC3339; empty = unbounded
	To         string `form:"to"`   // RFC3339; empty = unbounded
	Page       int    `form:"page" binding:"omitempty,min=1"`
	Size       int    `form:"size" binding:"omitempty,min=1,max=500"`
}

type queryLogsResponse struct {
	Items []Log `json:"items"`
	Total int64 `json:"total"`
}

func (c *Component) queryLogs(ctx context.Context, req *queryLogsRequest) (*queryLogsResponse, error) {
	q := Query{
		ActorID:    req.ActorID,
		Resource:   req.Resource,
		ResourceID: req.ResourceID,
		Action:     req.Action,
		Result:     req.Result,
		Page:       req.Page,
		Size:       req.Size,
	}
	if req.From != "" {
		t, err := time.Parse(time.RFC3339, req.From)
		if err != nil {
			return nil, fmt.Errorf("audit: invalid from time %q: %w", req.From, err)
		}
		q.From = t
	}
	if req.To != "" {
		t, err := time.Parse(time.RFC3339, req.To)
		if err != nil {
			return nil, fmt.Errorf("audit: invalid to time %q: %w", req.To, err)
		}
		q.To = t
	}
	items, total, err := c.logger.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []Log{}
	}
	return &queryLogsResponse{Items: items, Total: total}, nil
}

// --- retention purge job -------------------------------------------------

// purgeJob deletes audit rows older than the retention window in
// bounded batches. It reads retention_days / purge_batch_size from
// the live options snapshot each sweep (reload:"hot").
type purgeJob struct {
	c        *Component
	interval time.Duration
}

func (j *purgeJob) Name() string             { return "audit-purge" }
func (j *purgeJob) Spec() string             { return "@every " + j.interval.String() }
func (j *purgeJob) Policy() scheduler.Policy { return scheduler.PolicySkipIfRunning }

func (j *purgeJob) Run(ctx context.Context) error {
	opts := j.c.opts.Load()
	deleted, err := j.c.purgeOnce(ctx, opts.RetentionDays, opts.PurgeBatchSize)
	if err != nil {
		return err
	}
	if deleted > 0 {
		j.c.chok.Info("audit: purge swept expired rows",
			"deleted", deleted,
			"retention_days", opts.RetentionDays,
		)
	}
	return nil
}

// purgeOnce removes every row older than the retention cutoff in
// batches of batchSize, returning the total rows deleted. The
// two-step select-ids-then-delete keeps the statement portable
// (DELETE ... LIMIT is not cross-dialect) and the per-batch bound
// keeps row locks short so INSERT never stalls behind the sweep.
func (c *Component) purgeOnce(ctx context.Context, retentionDays, batchSize int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	gdb := c.h.Unsafe(ctx)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var ids []string
		if err := gdb.Model(&Log{}).
			Where("occurred_at < ?", cutoff).
			Order("occurred_at").
			Limit(batchSize).
			Pluck("id", &ids).Error; err != nil {
			return total, fmt.Errorf("audit purge: select batch: %w", err)
		}
		if len(ids) == 0 {
			return total, nil
		}
		res := gdb.Where("id IN ?", ids).Delete(&Log{})
		if res.Error != nil {
			return total, fmt.Errorf("audit purge: delete batch: %w", res.Error)
		}
		total += res.RowsAffected
		if res.RowsAffected == 0 {
			return total, nil // defensive: avoid a spin if delete matched nothing
		}
	}
}
