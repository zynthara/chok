package identity

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/tracectx"
)

// The results and actor types an Event records. The values are stored and
// filtered on; they never change.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
	ResultDenied  = "denied"

	ActorUser   = "user"
	ActorSystem = "system"
	ActorAPIKey = "api_key"
)

// Event is one audit record: who did what to which resource, and how it
// ended. Log takes one — Action is required, the rest optional — and fills
// the blanks it can: the actor from the request's principal, its address
// and request id, the trace id when the binary links telemetry, Result
// "success", OccurredAt now. ListFromQuery returns events with ID and
// OccurredAt set.
type Event struct {
	// ID is the record's id (aud_…), set when it is written.
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	// Action names what happened, verb last: "post.publish", "user.role.set".
	Action string `json:"action"`
	// Result is ResultSuccess, ResultFailure or ResultDenied.
	Result     string `json:"result"`
	Resource   string `json:"resource,omitzero"`
	ResourceID string `json:"resource_id,omitzero"`
	// ActorID is who acted: a user id, or an API key's id with ActorType
	// ActorAPIKey; ActorType defaults to ActorUser when ActorID is set.
	ActorID   string `json:"actor_id,omitzero"`
	ActorType string `json:"actor_type,omitzero"`
	ActorIP   string `json:"actor_ip,omitzero"`
	// Before and After are the resource's state around the change, stored
	// as JSON; leave out what must not be kept, the log stores it all.
	Before any `json:"before,omitzero"`
	After  any `json:"after,omitzero"`
	// Reason is a note for auditors, at most 512 bytes.
	Reason    string         `json:"reason,omitzero"`
	Metadata  map[string]any `json:"metadata,omitzero"`
	TraceID   string         `json:"trace_id,omitzero"`
	RequestID string         `json:"request_id,omitzero"`
}

// Audit is the audit log: records are written in the background and only
// ever added — the log has no update or delete, and the records older
// than auth.audit.retention are purged every hour. With auth.audit off,
// Log and LogSync do nothing.
type Audit struct {
	enabled    bool
	d          *db.DB
	logger     *slog.Logger
	dropOnFull bool
	retention  time.Duration
	batchSize  int
	flushEvery time.Duration
	purgeEvery time.Duration
	now        func() time.Time

	mu        sync.RWMutex // Log holds it to send; close takes it to stop sends
	closed    bool
	in        chan Event
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	pending atomic.Int64
	dropped atomic.Uint64
	written atomic.Uint64
	failed  atomic.Uint64
	lastErr atomic.Pointer[failure]
}

type failure struct {
	at  time.Time
	err error
}

const (
	auditBatchSize  = 100         // rows per INSERT
	auditFlushEvery = time.Second // a partial batch waits at most this long
	auditPurgeEvery = time.Hour   // retention sweeps
	auditPurgeBatch = 1000        // rows per DELETE, so a sweep never holds long locks
	auditFailWindow = time.Minute // Health reports a write failure this recent
	auditPageSize   = 50          // ListFromQuery's default page
	auditMaxPage    = 500         // and its largest
	auditReasonMax  = 512         // the reason column's size
)

func newAudit(d *db.DB, c AuditConfig, logger *slog.Logger) *Audit {
	a := &Audit{
		enabled: c.Enabled, d: d, logger: logger, dropOnFull: c.DropOnFull, retention: c.Retention,
		batchSize: auditBatchSize, flushEvery: auditFlushEvery, purgeEvery: auditPurgeEvery, now: time.Now,
	}
	if a.enabled {
		a.start(max(c.AsyncBuffer, 1))
	}
	return a
}

// start runs the writer. Its context is the background's: the writer
// outlives the call that built the Identity, and Close ends it.
func (a *Audit) start(buffer int) {
	a.in = make(chan Event, buffer)
	a.done = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.run(ctx)
}

// Enabled reports whether the log records anything.
func (a *Audit) Enabled() bool { return a.enabled }

// Log queues e for the writer and returns. When the queue is full it
// waits for room, or drops e if the caller's context ends first; with
// auth.audit.drop_on_full it drops e at once. A dropped event is logged
// as a warning. Log does not join the caller's transaction: it records
// what was attempted even when the transaction rolls back.
func (a *Audit) Log(ctx context.Context, e Event) {
	if !a.enabled {
		return
	}
	e = mergeContext(ctx, e)
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		a.drop(ctx, e, "the audit log is closed")
		return
	}
	if a.dropOnFull {
		select {
		case a.in <- e:
			a.pending.Add(1)
		default:
			a.drop(ctx, e, "the queue is full (drop_on_full)")
		}
		return
	}
	select {
	case a.in <- e:
		a.pending.Add(1)
	case <-ctx.Done():
		a.drop(ctx, e, "the queue is full and the caller's context ended")
	}
}

func (a *Audit) drop(ctx context.Context, e Event, why string) {
	a.dropped.Add(1)
	chok.LoggerFrom(ctx).WarnContext(ctx, "identity: audit event dropped: "+why, "action", e.Action, "resource", e.Resource)
}

// LogSync writes e before it returns, inside the caller's transaction if
// ctx carries one: use it when the record must exist for the operation
// to count.
func (a *Audit) LogSync(ctx context.Context, e Event) error {
	if !a.enabled {
		return nil
	}
	row, err := a.row(mergeContext(ctx, e))
	if err != nil {
		return err
	}
	if err := a.d.Unsafe(ctx).Create(&row).Error; err != nil {
		a.fail(err)
		return fmt.Errorf("identity: audit: %w", err)
	}
	a.written.Add(1)
	return nil
}

// mergeContext fills the actor, its address, the request id and, when
// telemetry is linked, the trace id from ctx where e leaves them blank;
// what the caller set stays.
func mergeContext(ctx context.Context, e Event) Event {
	if e.ActorID == "" {
		if p, ok := chok.PrincipalFrom(ctx); ok && p.UserID != "" {
			e.ActorID = p.UserID
			if e.ActorType == "" {
				e.ActorType = ActorUser
			}
		}
	}
	if e.ActorIP == "" {
		e.ActorIP = chok.ClientIP(ctx)
	}
	if e.RequestID == "" {
		e.RequestID = chok.RequestID(ctx)
	}
	if e.TraceID == "" && tracectx.TraceID != nil {
		e.TraceID = tracectx.TraceID(ctx)
	}
	return e
}

var errNoAction = errors.New("identity: an audit event needs an Action")

// row turns an event into a record, applying the defaults.
func (a *Audit) row(e Event) (auditRow, error) {
	if e.Action == "" {
		return auditRow{}, errNoAction
	}
	r := auditRow{
		ID: db.NewRID("aud"), OccurredAt: e.OccurredAt.UTC(), Action: e.Action, Result: e.Result,
		Resource: e.Resource, ResourceID: e.ResourceID, ActorID: e.ActorID, ActorType: e.ActorType, ActorIP: e.ActorIP,
		Reason: e.Reason, TraceID: e.TraceID, RequestID: e.RequestID,
	}
	if e.OccurredAt.IsZero() {
		r.OccurredAt = a.now().UTC()
	}
	if r.Result == "" {
		r.Result = ResultSuccess
	}
	if r.ActorType == "" && r.ActorID != "" {
		r.ActorType = ActorUser
	}
	if len(r.Reason) > auditReasonMax {
		r.Reason = r.Reason[:auditReasonMax]
	}
	var err error
	if r.BeforeData, err = marshalJSON(e.Before); err != nil {
		return auditRow{}, fmt.Errorf("identity: audit Before: %w", err)
	}
	if r.AfterData, err = marshalJSON(e.After); err != nil {
		return auditRow{}, fmt.Errorf("identity: audit After: %w", err)
	}
	if len(e.Metadata) > 0 {
		if r.Metadata, err = marshalJSON(e.Metadata); err != nil {
			return auditRow{}, fmt.Errorf("identity: audit Metadata: %w", err)
		}
	}
	return r, nil
}

func marshalJSON(v any) (jsonData, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	return jsonData(b), err
}

// run is the writer: it batches queued events, writes a batch when it is
// full or a second old, sweeps expired records every hour, and on close
// writes what is left.
func (a *Audit) run(ctx context.Context) {
	defer close(a.done)
	batch := make([]auditRow, 0, a.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := a.d.Unsafe(ctx).CreateInBatches(batch, len(batch)).Error; err != nil {
			a.failed.Add(uint64(len(batch)))
			a.fail(err)
			a.logger.Error("identity: audit batch not written; dropped", "size", len(batch), "error", err)
		} else {
			a.written.Add(uint64(len(batch)))
		}
		batch = batch[:0]
	}
	timer := time.NewTimer(a.flushEvery)
	defer timer.Stop()
	var purge <-chan time.Time
	if a.retention > 0 {
		t := time.NewTicker(a.purgeEvery)
		defer t.Stop()
		purge = t.C
	}
	for {
		select {
		case e, ok := <-a.in:
			if !ok {
				flush()
				return
			}
			a.pending.Add(-1)
			r, err := a.row(e)
			if err != nil {
				a.failed.Add(1)
				a.fail(err)
				a.logger.Error("identity: audit event not recorded", "action", e.Action, "error", err)
				continue
			}
			batch = append(batch, r)
			if len(batch) >= a.batchSize {
				flush()
				timer.Reset(a.flushEvery)
			}
		case <-timer.C:
			flush()
			timer.Reset(a.flushEvery)
		case <-purge:
			if n, err := a.purge(ctx, a.now().Add(-a.retention)); err != nil {
				a.logger.Warn("identity: audit retention sweep failed", "error", err)
			} else if n > 0 {
				a.logger.Info("identity: audit retention swept expired records", "deleted", n, "retention", a.retention)
			}
		case <-ctx.Done():
			return
		}
	}
}

// purge deletes the records that occurred before cutoff, a batch at a
// time, and returns how many went.
func (a *Audit) purge(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var ids []string
		if err := a.d.Unsafe(ctx).Model(&auditRow{}).Where("occurred_at < ?", cutoff.UTC()).
			Order("occurred_at").Limit(auditPurgeBatch).Pluck("id", &ids).Error; err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		res := a.d.Unsafe(ctx).Where("id IN ?", ids).Delete(&auditRow{})
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
		if len(ids) < auditPurgeBatch {
			return total, nil
		}
	}
}

func (a *Audit) fail(err error) { a.lastErr.Store(&failure{at: a.now(), err: err}) }

// health fails when a write failed within the last minute.
func (a *Audit) health() error {
	if f := a.lastErr.Load(); f != nil && a.now().Sub(f.at) < auditFailWindow {
		return fmt.Errorf("audit: a write failed %s ago: %w", a.now().Sub(f.at).Round(time.Second), f.err)
	}
	return nil
}

// close stops the writer once the queued events are written, or when ctx
// ends; the events still queued then are lost and reported.
func (a *Audit) close(ctx context.Context) error {
	if !a.enabled {
		return nil
	}
	var err error
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		close(a.in)
		a.mu.Unlock()
		select {
		case <-a.done:
		case <-ctx.Done():
			a.cancel()
			<-a.done
			err = fmt.Errorf("identity: audit closed before %d queued event(s) were written: %w", a.pending.Load(), ctx.Err())
		}
		a.cancel()
	})
	return err
}

// auditColumns are the fields ListFromQuery filters and orders on, with
// their columns.
var auditColumns = map[string]string{
	"action": "action", "actor_id": "actor_id", "actor_type": "actor_type", "occurred_at": "occurred_at",
	"request_id": "request_id", "resource": "resource", "resource_id": "resource_id", "result": "result",
}

// QueryFields lists the fields ListFromQuery filters and orders on.
func (a *Audit) QueryFields() []string {
	names := make([]string, 0, len(auditColumns))
	for n := range auditColumns {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// ListFromQuery returns a page of records, newest first: q holds page and
// size (default 50, at most 500), order=field:asc|desc, from and to
// (RFC 3339, inclusive bounds on occurred_at) and equality filters on
// the fields QueryFields lists. An unknown or malformed parameter is a
// 400. It lets an *Audit serve a chok.List route.
func (a *Audit) ListFromQuery(ctx context.Context, q url.Values) (*chok.Page[Event], error) {
	page, size := 1, auditPageSize
	order, desc := "occurred_at", true
	tx := a.d.Unsafe(ctx).Model(&auditRow{})
	for key, vals := range q {
		if len(vals) != 1 {
			return nil, chok.ErrBadRequest.Msgf("give %s once", key).With("field", key)
		}
		v := vals[0]
		switch key {
		case "page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, chok.ErrBadRequest.Msgf("page must be a whole number from 1").With("field", key)
			}
			page = n
		case "size":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > auditMaxPage {
				return nil, chok.ErrBadRequest.Msgf("size must be a whole number from 1 to %d", auditMaxPage).With("field", key)
			}
			size = n
		case "order":
			field, dir, _ := strings.Cut(v, ":")
			if _, ok := auditColumns[field]; !ok || (dir != "" && dir != "asc" && dir != "desc") {
				return nil, chok.ErrBadRequest.Msgf("order is field:asc or field:desc over %s", strings.Join(a.QueryFields(), ", ")).With("field", key)
			}
			order, desc = auditColumns[field], dir != "asc"
		case "from", "to":
			at, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, chok.ErrBadRequest.Msgf("%s must be an RFC 3339 time such as 2026-10-24T08:00:00Z", key).With("field", key)
			}
			op := ">="
			if key == "to" {
				op = "<="
			}
			tx = tx.Where("occurred_at "+op+" ?", at.UTC())
		default:
			col, ok := auditColumns[key]
			if !ok {
				return nil, chok.ErrBadRequest.Msgf("unknown parameter %s; filter on %s, from or to", key, strings.Join(a.QueryFields(), ", ")).With("field", key)
			}
			if v != "" {
				tx = tx.Where(col+" = ?", v)
			}
		}
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, fmt.Errorf("identity: audit: %w", err)
	}
	dir := " DESC"
	if !desc {
		dir = " ASC"
	}
	var rows []auditRow
	if err := tx.Order(order + dir).Order("id" + dir).Limit(size).Offset((page - 1) * size).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: audit: %w", err)
	}
	out := &chok.Page[Event]{Items: make([]Event, 0, len(rows)), Total: total, Page: page, Size: size, HasMore: int64(page*size) < total}
	for _, r := range rows {
		out.Items = append(out.Items, r.event())
	}
	return out, nil
}

// event turns a record back into an Event; the JSON documents decode
// into generic values.
func (r auditRow) event() Event {
	e := Event{
		ID: r.ID, OccurredAt: r.OccurredAt.UTC(), Action: r.Action, Result: r.Result, Resource: r.Resource, ResourceID: r.ResourceID,
		ActorID: r.ActorID, ActorType: r.ActorType, ActorIP: r.ActorIP, Reason: r.Reason, TraceID: r.TraceID, RequestID: r.RequestID,
	}
	if len(r.BeforeData) > 0 {
		_ = json.Unmarshal(r.BeforeData, &e.Before)
	}
	if len(r.AfterData) > 0 {
		_ = json.Unmarshal(r.AfterData, &e.After)
	}
	if len(r.Metadata) > 0 {
		_ = json.Unmarshal(r.Metadata, &e.Metadata)
	}
	return e
}
