package jobs

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/conf"
)

// Jobs is the jobs battery: periodic jobs on cron schedules, and workers
// that deliver the messages Enqueue writes. Register it with app.Use:
// its Start runs the jobs and the workers, Close stops them.
type Jobs struct {
	cfg    Config
	logger *slog.Logger
	core   *core
	sched  *scheduler

	periodic []*entry
	workers  []*entry
	relays   []*relay // relays[i] sweeps for workers[i]
	cleanup  *entry   // nil when Retention is 0
	deleted  atomic.Int64

	mu       sync.Mutex
	started  bool
	closed   bool
	cancel   context.CancelFunc // ends the run context of Start
	deadline time.Time          // when the stop budget of the first stop runs out

	saveMu sync.Mutex
	saved  bool // the workers' delivered sets are written
}

// Option configures New.
type Option func(*options)

type options struct {
	periodic  []periodicOption
	workers   []workerOption
	observers []func(ev Event)
	logger    *slog.Logger
}

type periodicOption struct {
	name, spec string
	fn         func(ctx context.Context) error
	overlap    Overlap
}

type workerOption struct {
	topic string
	fn    func(ctx context.Context, m rawMessage) error
}

// Overlap says what a periodic job does when its schedule fires while
// its previous run is still going.
type Overlap int

const (
	// OverlapSkip drops the trigger: for frequent jobs, where the next
	// trigger comes soon anyway.
	OverlapSkip Overlap = iota
	// OverlapQueue runs once more as soon as the current run ends, for
	// jobs that must not miss a trigger; further triggers during the run
	// fold into that one.
	OverlapQueue
	// OverlapAllow starts every trigger at once, beside the runs still
	// going: for jobs that tolerate running concurrently with themselves.
	OverlapAllow
)

// String names the policy: skip, queue or allow.
func (o Overlap) String() string {
	switch o {
	case OverlapSkip:
		return "skip"
	case OverlapQueue:
		return "queue"
	case OverlapAllow:
		return "allow"
	}
	return fmt.Sprintf("Overlap(%d)", int(o))
}

// Periodic runs fn on the cron schedule spec — five fields (minute hour
// day-of-month month day-of-week), six with leading seconds, or a
// descriptor such as @hourly or "@every 10m" — in the machine's time
// zone unless spec starts with CRON_TZ=<zone>. overlap decides what a
// trigger does while the previous run is still going. fn gets a context
// that ends when the jobs stop; a panic is recovered and counts as a
// failed run. The name must be unique: Stats and the logs use it.
func Periodic(name, spec string, fn func(ctx context.Context) error, overlap Overlap) Option {
	return func(o *options) {
		o.periodic = append(o.periodic, periodicOption{name: name, spec: spec, fn: fn, overlap: overlap})
	}
}

// Worker delivers the messages enqueued under topic to fn, one at a time
// in (created_at, id) order, the payload decoded into T with
// encoding/json/v2 (Worker[jsontext.Value] hands on the JSON as stored).
// Delivery is at least once: a message comes again after fn fails on it,
// and after a crash within the settle window, so fn must be idempotent —
// dedupe on Message.ID or on a key in the payload. A graceful stop
// (Close) remembers what was delivered, so a restart does not repeat it.
//
// While fn fails on a message (returns an error or panics), the messages
// behind it in the topic wait; other topics keep flowing. The worker
// tries the message again after Config.RetryBackoff, doubling the wait
// up to Config.MaxBackoff; the Config.MaxAttempts-th failure parks the
// message in the dead letters (see Jobs.Dead) and the topic moves on. A
// payload that does not decode into T is parked at once, without calling
// fn. One worker per topic.
func Worker[T any](topic string, fn func(ctx context.Context, m Message[T]) error) Option {
	w := workerOption{topic: topic}
	if fn != nil {
		w.fn = func(ctx context.Context, m rawMessage) error {
			var payload T
			if err := json.Unmarshal(m.Payload, &payload); err != nil {
				return decodeError{err}
			}
			return fn(ctx, Message[T]{ID: m.ID, Topic: m.Topic, Payload: payload, CreatedAt: m.CreatedAt, Attempt: m.Attempt})
		}
	}
	return func(o *options) { o.workers = append(o.workers, w) }
}

// decodeError is a payload Worker[T] could not decode into T: retrying
// cannot help, so the message is parked at once.
type decodeError struct{ err error }

func (e decodeError) Error() string { return "decode: " + e.err.Error() }
func (e decodeError) Unwrap() error { return e.err }

// Logger is where the jobs log; the default is the database handle's
// logger.
func Logger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// Observe calls fn at every run's start, end and failure, and at every
// trigger skipped because a run was in flight — of the periodic jobs, the
// workers' sweeps and the cleanup alike. fn runs synchronously in the
// run's goroutine, before the job's function and after it, so it must be
// quick: hand the event on, to realtime's Publish for instance. A panic
// in fn is recovered and logged at Warn; the run and its Stats are not
// affected. Observe may be given more than once: the functions are called
// in the order given.
func Observe(fn func(ev Event)) Option {
	return func(o *options) { o.observers = append(o.observers, fn) }
}

// Event is one moment of a run that Observe reports: a periodic job's
// run, a worker's sweep of its topic or the retention cleanup's run
// starting, finishing or failing, a trigger skipped while a run was in
// flight, or a worker parking a message in the dead letters. Kind and
// Type are its two enumerations, as strings so an observer can pass the
// event on as it is.
type Event struct {
	// Kind is run.started, run.finished, run.failed, run.skipped or
	// message.parked. A run that returned an error or panicked failed;
	// one stopped by shutdown finished. A sweep in which the worker
	// failed on a message failed; one that parked a message goes on.
	Kind string `json:"kind"`
	// Job is the periodic job's name, the worker's topic, or "cleanup".
	Job string `json:"job"`
	// Type is periodic, worker or cleanup.
	Type string `json:"type"`
	// StartedAt is when the run started, in UTC; for run.skipped, when
	// the trigger fired; for message.parked, when the message was parked.
	StartedAt time.Time `json:"started_at"`
	// Duration is how long the run took, on run.finished and run.failed.
	// Like JobStats.AvgDuration it has no default representation in
	// encoding/json/v2: chok's handlers and realtime write it as a string
	// ("1.5s").
	Duration time.Duration `json:"duration,omitzero"`
	// Error is the run's error on run.failed: "panic: ..." for a panic.
	// On message.parked it is the message's last error.
	Error string `json:"error,omitzero"`
	// Next is a periodic job's next trigger after the event, in UTC; zero
	// for the workers and the cleanup.
	Next time.Time `json:"next,omitzero"`
	// Messages counts, when a run ends, the messages a worker's sweep
	// handed to its function and the function took, or the rows the
	// cleanup deleted.
	Messages int `json:"messages,omitzero"`
}

// The values of Event.Kind and Event.Type.
const (
	kindStarted  = "run.started"
	kindFinished = "run.finished"
	kindFailed   = "run.failed"
	kindSkipped  = "run.skipped"
	kindParked   = "message.parked"

	typePeriodic = "periodic"
	typeWorker   = "worker"
	typeCleanup  = "cleanup"
)

// Stats is a snapshot of the background work of this process.
type Stats struct {
	// Periodic has an entry per periodic job, in the order given to New.
	Periodic []JobStats `json:"periodic"`
	// Workers has an entry per worker, named by its topic; each run is a
	// sweep of the topic.
	Workers []JobStats `json:"workers"`
	// Cleanup is the retention cleanup's; zero when Retention is 0.
	Cleanup JobStats `json:"cleanup"`
}

// JobStats counts the runs of a periodic job, a worker's sweeps, or the
// retention cleanup's runs.
type JobStats struct {
	// Name is the periodic job's name, the worker's topic, or "cleanup".
	Name string `json:"name"`
	// Spec is the schedule: the cron spec, or "@every" the interval.
	Spec string `json:"spec"`
	// Next is when the next run is due; zero while the jobs are not
	// running.
	Next time.Time `json:"next"`
	// LastStart and LastEnd bound the last finished run.
	LastStart time.Time `json:"last_start"`
	LastEnd   time.Time `json:"last_end"`
	// LastError is the error of the last finished run; empty when it
	// succeeded. A worker's sweep that only waits for a failing message's
	// next try leaves it as it was.
	LastError string `json:"last_error"`
	// Runs counts the finished runs, failed ones included; Failures
	// counts the runs that returned an error or panicked.
	Runs     int64 `json:"runs"`
	Failures int64 `json:"failures"`
	// Skipped counts the triggers that started no run because one was in
	// flight: every one of them under OverlapSkip, those folded into the
	// queued run under OverlapQueue.
	Skipped int64 `json:"skipped"`
	// AvgDuration is the mean duration of the finished runs. Returned
	// from a chok handler it is written as a string ("1.5s"), as chok's
	// JSON writes every time.Duration; encoding/json writes integer
	// nanoseconds, and encoding/json/v2 refuses it unless told how, having
	// no default representation for a Duration.
	AvgDuration time.Duration `json:"avg_duration"`
	// Messages counts, for a worker, the messages its function took
	// (returned nil for) since New; for the cleanup, the messages it
	// deleted.
	Messages int64 `json:"messages"`
	// Parked counts, for a worker, the messages it parked in the dead
	// letters since New.
	Parked int64 `json:"parked,omitzero"`
	// HeadAttempts is, for a worker whose topic waits on a failing
	// message, that message's failed deliveries so far; 0 while the
	// topic flows.
	HeadAttempts int `json:"head_attempts,omitzero"`
	// NextRetry is when that message is tried next; zero while the
	// topic flows.
	NextRetry time.Time `json:"next_retry,omitzero"`
}

// New builds the jobs battery on d: it checks cfg and every option, then
// registers the tables chok_jobs_messages, chok_jobs_watermarks,
// chok_jobs_dead and chok_jobs_delivered under the owner "jobs" (created
// now in migrate: auto; from its embedded SQL files in migrate:
// versioned). Nothing runs until Start. One Jobs per database handle.
func New(cfg Config, d *db.DB, opts ...Option) (*Jobs, error) {
	if d == nil {
		return nil, errors.New("jobs: nil *db.DB")
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg = cfg.withDefaults()
	if err := conf.Check(&cfg); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	if o.logger == nil {
		o.logger = d.Logger()
	}
	for _, fn := range o.observers {
		if fn == nil {
			return nil, errors.New("jobs: Observe with a nil function")
		}
	}
	j := &Jobs{cfg: cfg, logger: o.logger, sched: &scheduler{logger: o.logger, observers: o.observers}}
	// Every option is checked before anything touches the database.
	names := map[string]bool{}
	for _, p := range o.periodic {
		switch {
		case p.name == "":
			return nil, errors.New("jobs: a periodic job needs a name")
		case names[p.name]:
			return nil, fmt.Errorf("jobs: two periodic jobs are named %q", p.name)
		case p.fn == nil:
			return nil, fmt.Errorf("jobs: periodic job %q has a nil function", p.name)
		case p.overlap < OverlapSkip || p.overlap > OverlapAllow:
			return nil, fmt.Errorf("jobs: periodic job %q: %s is not an Overlap", p.name, p.overlap)
		}
		names[p.name] = true
		schedule, err := specParser.Parse(p.spec)
		if err != nil {
			return nil, fmt.Errorf("jobs: periodic job %q: spec %q: %w", p.name, p.spec, err)
		}
		j.periodic = append(j.periodic, newEntry(typePeriodic, p.name, p.spec, schedule, p.overlap, p.fn))
	}
	topics := map[string]bool{}
	for _, w := range o.workers {
		if err := checkTopic(w.topic); err != nil {
			return nil, fmt.Errorf("jobs: worker %q: %w", w.topic, err)
		}
		if topics[w.topic] {
			return nil, fmt.Errorf("jobs: two workers for topic %q", w.topic)
		}
		if w.fn == nil {
			return nil, fmt.Errorf("jobs: worker %q has a nil function", w.topic)
		}
		topics[w.topic] = true
	}
	if err := d.RegisterSchema("jobs", tables(), migrations()); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	j.core = newCore(d, o.logger)
	sweep := "@every " + cfg.PollInterval.String()
	var workerTopics []string
	for _, w := range o.workers {
		r, err := newRelay(w.topic, w.fn, j.core, cfg.SettleWindow, cfg.BatchSize)
		if err != nil {
			return nil, err // unreachable: the topic and fn were checked above
		}
		r.retry = retryPolicy{maxAttempts: cfg.MaxAttempts, backoff: cfg.RetryBackoff, maxBackoff: cfg.MaxBackoff}
		j.relays = append(j.relays, r)
		e := newEntry(typeWorker, w.topic, sweep, every(cfg.PollInterval), OverlapSkip, r.run)
		e.quiet, e.count = true, r.delivered.Load
		if len(o.observers) > 0 {
			r.observe = func(ctx context.Context, ev Event) { j.sched.observe(ctx, e, ev) }
		}
		j.workers = append(j.workers, e)
		workerTopics = append(workerTopics, w.topic)
	}
	if cfg.Retention > 0 {
		j.cleanup = newEntry(typeCleanup, "cleanup", "@every "+cfg.CleanupInterval.String(), every(cfg.CleanupInterval), OverlapSkip,
			func(ctx context.Context) error { return j.cleanupOnce(ctx, workerTopics) })
		j.cleanup.quiet, j.cleanup.count = true, j.deleted.Load
	}
	j.sched.entries = slices.Concat(j.periodic, j.workers)
	if j.cleanup != nil {
		j.sched.entries = append(j.sched.entries, j.cleanup)
	}
	if !cfg.Enabled && len(j.sched.entries) > 0 {
		o.logger.Info("jobs: disabled in this process: Enqueue works, the periodic jobs and the workers do not run",
			"periodic", len(j.periodic), "workers", len(j.workers))
	}
	return j, nil
}

// MustNew is New that panics on error.
func MustNew(cfg Config, d *db.DB, opts ...Option) *Jobs {
	j, err := New(cfg, d, opts...)
	if err != nil {
		panic(err)
	}
	return j
}

func (j *Jobs) cleanupOnce(ctx context.Context, topics []string) error {
	n, err := j.core.cleanupOnce(ctx, topics, j.cfg.Retention, j.cfg.BatchSize)
	j.deleted.Add(n)
	if n > 0 {
		j.logger.InfoContext(ctx, "jobs: cleanup deleted delivered messages", "deleted", n, "retention", j.cfg.Retention)
	}
	return err
}

// Enqueue writes a message to topic in the transaction of txCtx — the
// context db.RunInTx gives its function, on the database the jobs use —
// so the message commits or rolls back with the writes it describes.
// Outside such a transaction it returns ErrOutsideTx. payload is encoded
// with encoding/json/v2; the topic's worker gets the JSON as
// Message.Payload once the transaction commits. Enqueue works whether or
// not the workers run in this process.
func (j *Jobs) Enqueue(txCtx context.Context, topic string, payload any) error {
	return j.core.enqueueJSON(txCtx, topic, payload)
}

// Stats returns the counters of the periodic jobs, the workers and the
// cleanup.
func (j *Jobs) Stats() Stats {
	st := Stats{Periodic: []JobStats{}, Workers: []JobStats{}}
	for _, e := range j.periodic {
		st.Periodic = append(st.Periodic, e.snapshot())
	}
	for i, e := range j.workers {
		r := j.relays[i]
		ws := e.snapshot()
		ws.Messages, ws.Parked = r.delivered.Load(), r.parked.Load()
		head := r.headStats()
		ws.HeadAttempts, ws.NextRetry = head.attempts, head.next
		st.Workers = append(st.Workers, ws)
	}
	if j.cleanup != nil {
		st.Cleanup = j.cleanup.snapshot()
		st.Cleanup.Messages = j.deleted.Load()
	}
	return st
}

// Start runs the periodic jobs, the workers' sweeps and the cleanup until
// ctx ends, then stops them: their contexts end and Start waits for the
// runs in flight, up to Config.StopBudget. With Config.Enabled false it
// starts nothing and returns nil at once.
func (j *Jobs) Start(ctx context.Context) error {
	if !j.cfg.Enabled {
		return nil
	}
	j.mu.Lock()
	switch {
	case j.closed:
		j.mu.Unlock()
		return errors.New("jobs: Start after Close")
	case j.started:
		j.mu.Unlock()
		return errors.New("jobs: Start called twice")
	}
	runCtx, cancel := context.WithCancel(ctx)
	j.started, j.cancel = true, cancel
	// The loops start under the lock, so a concurrent Close either finds
	// nothing started or waits for every loop.
	j.sched.start(runCtx)
	j.mu.Unlock()

	j.logger.InfoContext(ctx, "jobs: started", "periodic", len(j.periodic), "workers", len(j.workers),
		"poll_interval", j.cfg.PollInterval, "settle_window", j.cfg.SettleWindow, "retention", j.cfg.Retention)
	<-runCtx.Done()
	// ctx has ended: the wait gets its own budget, still carrying ctx's
	// values (shutdown paths derive from WithoutCancel).
	return j.stop(context.WithoutCancel(ctx))
}

// Close stops what Start runs, if it has not stopped yet, and waits for
// the runs in flight up to Config.StopBudget after the stop began, and
// within ctx. Start returns once they have stopped.
func (j *Jobs) Close(ctx context.Context) error {
	j.mu.Lock()
	j.closed = true
	j.mu.Unlock()
	return j.stop(ctx)
}

// stop ends the run context and waits for the loops and runs, until the
// stop budget of the first stop runs out or ctx ends; once they have
// stopped, it saves the workers' delivered sets.
func (j *Jobs) stop(ctx context.Context) error {
	j.mu.Lock()
	if !j.started {
		j.mu.Unlock()
		return nil
	}
	if j.deadline.IsZero() {
		j.deadline = time.Now().Add(j.cfg.StopBudget)
		j.cancel()
	}
	deadline := j.deadline
	j.mu.Unlock()
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := j.sched.wait(wctx); err != nil {
		running := strings.Join(j.sched.inFlight(), ", ")
		if ctx.Err() == nil {
			return fmt.Errorf("jobs: stop_budget (%s) ran out with runs still going: %s", j.cfg.StopBudget, running)
		}
		return fmt.Errorf("jobs: stop: %w, with runs still going: %s", ctx.Err(), running)
	}
	j.saveDelivered(wctx)
	return nil
}

// saveDelivered writes, once, what each worker delivered that its
// watermark has not passed, for its first sweep after a restart (see
// relay.saveDelivered). A failure costs those messages a second delivery.
func (j *Jobs) saveDelivered(ctx context.Context) {
	j.saveMu.Lock()
	defer j.saveMu.Unlock()
	if j.saved {
		return
	}
	j.saved = true
	for _, r := range j.relays {
		n, err := r.saveDelivered(ctx)
		if err != nil {
			j.logger.WarnContext(ctx, "jobs: saving the delivered set failed; the next start may deliver it again",
				"worker", r.topic, "error", err)
			continue
		}
		if n > 0 {
			j.logger.DebugContext(ctx, "jobs: saved the delivered set", "worker", r.topic, "messages", n)
		}
	}
}

// Health reports nil: a failing job or worker does not make the process
// unhealthy (a poison message must not pull it out of rotation). Failed
// runs are in Stats and in the log.
func (j *Jobs) Health(context.Context) error { return nil }

// Name is "jobs", its name in the health report.
func (j *Jobs) Name() string { return "jobs" }
