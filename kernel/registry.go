package kernel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel/event"
)

// Default lifecycle budgets (mini-SPEC §3.4); Config.Defaults
// overrides globally, Descriptor.Timeouts per component.
const (
	DefaultInitTimeout   = 30 * time.Second
	DefaultCloseTimeout  = 15 * time.Second
	DefaultReloadTimeout = 10 * time.Second

	// drainBroadcastTimeout bounds the parallel Drain() broadcast.
	drainBroadcastTimeout = 5 * time.Second
	// busCloseBudget bounds the subscriber drain after AppStopped.
	busCloseBudget = 5 * time.Second
)

// Config assembles a Registry.
type Config struct {
	Logger     Logger
	Store      *conf.Store
	Bus        *event.Bus
	Components []Component
	Routes     RoutesFunc     // optional user route callback
	PostReload PostReloadFunc // optional WithReloadFunc callback
	Defaults   Timeouts       // zero fields fall back to package defaults
	DrainDelay time.Duration
}

// Registry is the single-actor control plane: every lifecycle
// transition (start / stop / reload) executes on one goroutine, and
// the read path (Lookup / Health / Ready / Components) works off an
// atomically-published immutable view. There is no lock order to
// memorize because there are no competing writers (SPEC §3.3).
type Registry struct {
	logger     Logger
	store      *conf.Store
	bus        *event.Bus
	routes     RoutesFunc
	postReload PostReloadFunc
	defaults   Timeouts
	drainDelay time.Duration

	slots  []*slot // assembly order
	byKey  map[Key]*slot
	levels [][]*slot // topological layers (computed at start)

	cmds chan command
	view atomic.Pointer[view]

	reloadGate atomic.Bool // CAS gate: overlap → ErrReloadInProgress

	started  atomic.Bool // startup completed successfully
	stopped  atomic.Bool // stop completed (or startup rolled back)
	startRan bool        // actor-private: Start attempted (single-use)

	// serving state (owned by the actor between start and stop)
	serveCancel context.CancelFunc
	serveWG     sync.WaitGroup
	serveErrs   chan serveExit
	failedCh    chan error // one unexpected server exit → App stops
}

type slot struct {
	comp      Component
	desc      Descriptor
	key       Key
	configKey string // derived section key ("" = none)
	enabled   bool
	state     State
	lastErr   string
}

// SectionKeyOf resolves the config section key a Descriptor addresses
// (mini-SPEC §1): ConfigKey for the default instance,
// ConfigKey + ".instances." + Instance for named ones, "" when the
// component has no configuration.
func SectionKeyOf(d Descriptor) string { return derivedConfigKey(d) }

// derivedConfigKey applies the mini-SPEC §1 addressing rule.
func derivedConfigKey(d Descriptor) string {
	if d.ConfigKey == "" {
		return ""
	}
	if d.Instance == "" || d.Instance == DefaultInstance {
		return d.ConfigKey
	}
	return d.ConfigKey + "." + conf.ReservedInstancesKey + "." + d.Instance
}

type cmdKind int

const (
	cmdStart cmdKind = iota
	cmdStop
	cmdReload
)

type command struct {
	kind  cmdKind
	ctx   context.Context
	reply chan error
}

type serveExit struct {
	key Key
	err error
}

// view is the immutable read-path snapshot.
type view struct {
	entries  map[Key]*viewEntry
	order    []Key // assembly order for stable rendering
	draining bool
}

type viewEntry struct {
	comp    Component
	desc    Descriptor
	cfgKey  string
	state   State
	err     string
	enabled bool
}

// New assembles a Registry. Duplicate (kind, instance) keys fail fast
// — intentional replacement is the App's Override job, resolved before
// the Registry sees the list.
func New(cfg Config) (*Registry, error) {
	if cfg.Logger == nil {
		cfg.Logger = nopLogger{}
	}
	if cfg.Bus == nil {
		cfg.Bus = event.NewBus(event.WithLogger(cfg.Logger))
	}
	if cfg.Store == nil {
		return nil, errors.New("kernel: Config.Store is required")
	}
	r := &Registry{
		logger:     cfg.Logger,
		store:      cfg.Store,
		bus:        cfg.Bus,
		routes:     cfg.Routes,
		postReload: cfg.PostReload,
		defaults:   cfg.Defaults,
		drainDelay: cfg.DrainDelay,
		byKey:      make(map[Key]*slot),
		cmds:       make(chan command),
		serveErrs:  make(chan serveExit),
		failedCh:   make(chan error, 1),
	}
	for _, c := range cfg.Components {
		d := c.Describe()
		if d.Kind == "" {
			return nil, fmt.Errorf("kernel: component %T declares an empty Kind", c)
		}
		k := KeyOf(d)
		if _, dup := r.byKey[k]; dup {
			return nil, fmt.Errorf("kernel: duplicate component %s — use chok.Override to replace an assembled module", k)
		}
		s := &slot{comp: c, desc: d, key: k, configKey: derivedConfigKey(d), state: StatePending, enabled: true}
		r.byKey[k] = s
		r.slots = append(r.slots, s)
	}
	r.publishView(false)
	go r.loop()
	return r, nil
}

// --- public API (thin wrappers over the actor) ------------------------

// Start runs the full startup sequence and returns once every Server
// is ready (or with the startup error after rollback). Single-use.
func (r *Registry) Start(ctx context.Context) error {
	return r.send(ctx, cmdStart)
}

// Stop drains and closes everything. Safe to call at any time;
// idempotent once stopped.
func (r *Registry) Stop(ctx context.Context) error {
	return r.send(ctx, cmdStop)
}

// Reload executes config swap → component dispatch → post-reload
// callback, failing fast between stages. Overlapping calls receive
// ErrReloadInProgress immediately.
func (r *Registry) Reload(ctx context.Context) error {
	if !r.started.Load() || r.stopped.Load() {
		return ErrNotStarted
	}
	if !r.reloadGate.CompareAndSwap(false, true) {
		return ErrReloadInProgress
	}
	defer r.reloadGate.Store(false)
	return r.send(ctx, cmdReload)
}

// Failed delivers the error of an unexpected Server exit: the App's
// run loop treats it as a shutdown trigger.
func (r *Registry) Failed() <-chan error { return r.failedCh }

func (r *Registry) send(ctx context.Context, k cmdKind) error {
	reply := make(chan error, 1)
	select {
	case r.cmds <- command{kind: k, ctx: ctx, reply: reply}:
	case <-ctx.Done():
		return fmt.Errorf("kernel: %w while waiting for the control loop", ctx.Err())
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return fmt.Errorf("kernel: %w while command executes", ctx.Err())
	}
}

// loop is the single actor: commands execute strictly serially.
func (r *Registry) loop() {
	for c := range r.cmds {
		switch c.kind {
		case cmdStart:
			c.reply <- r.doStart(c.ctx)
		case cmdStop:
			c.reply <- r.doStop(c.ctx)
		case cmdReload:
			c.reply <- r.doReload(c.ctx)
		}
	}
}

// --- read path (never enters the actor) --------------------------------

// Config implements Kernel.
func (r *Registry) Config() *conf.Snapshot { return r.store.Snapshot() }

// Logger implements Kernel.
func (r *Registry) Logger() Logger { return r.logger }

// Bus implements Kernel.
func (r *Registry) Bus() *event.Bus { return r.bus }

// Lookup implements Kernel: only enabled, initialized, not-yet-closed
// components are visible (SPEC §3.1 definition 2).
func (r *Registry) Lookup(kind string, instance ...string) (Component, bool) {
	inst := DefaultInstance
	if len(instance) > 0 && instance[0] != "" {
		inst = instance[0]
	}
	v := r.view.Load()
	e, ok := v.entries[Key{Kind: kind, Instance: inst}]
	if !ok || !e.enabled || e.state != StateReady {
		return nil, false
	}
	return e.comp, true
}

// Components lists every assembled component's observable status,
// disabled entries included, in assembly order.
func (r *Registry) Components() []ComponentStatus {
	v := r.view.Load()
	out := make([]ComponentStatus, 0, len(v.order))
	for _, k := range v.order {
		e := v.entries[k]
		st := ComponentStatus{Key: k, Descriptor: e.desc, ConfigKey: e.cfgKey, State: e.state, Err: e.err}
		out = append(out, st)
	}
	return out
}

// Health probes every Healther in parallel with a fan-in budget and
// classifies the aggregate (SPEC §0.3 rules preserved).
func (r *Registry) Health(ctx context.Context) HealthReport {
	v := r.view.Load()
	entries := make([]HealthEntry, len(v.order))

	var wg sync.WaitGroup
	for i, k := range v.order {
		e := v.entries[k]
		switch {
		case !e.enabled:
			entries[i] = HealthEntry{Key: k, Status: HealthDisabled}
			continue
		case e.state == StateDegraded:
			entries[i] = HealthEntry{Key: k, Status: HealthDegraded, Err: e.err}
			continue
		case e.state != StateReady:
			entries[i] = HealthEntry{Key: k, Status: HealthDown, Err: e.err}
			continue
		}
		h, ok := e.comp.(Healther)
		if !ok {
			entries[i] = HealthEntry{Key: k, Status: HealthUp}
			continue
		}
		wg.Add(1)
		go func(i int, k Key, opt bool) {
			defer wg.Done()
			start := time.Now()
			err := probe(ctx, h)
			d := time.Since(start)
			switch {
			case err == nil:
				entries[i] = HealthEntry{Key: k, Status: HealthUp, Duration: d}
			case opt:
				entries[i] = HealthEntry{Key: k, Status: HealthDegraded, Err: err.Error(), Duration: d}
			default:
				entries[i] = HealthEntry{Key: k, Status: HealthDown, Err: err.Error(), Duration: d}
			}
		}(i, k, e.desc.Optional)
	}
	wg.Wait()

	rep := HealthReport{Status: HealthUp, Entries: entries}
	for i, e := range entries {
		switch e.Status {
		case HealthDown:
			if !v.entries[entries[i].Key].desc.Optional {
				rep.Status = HealthDown
			} else if rep.Status != HealthDown {
				rep.Status = HealthDegraded
			}
		case HealthDegraded:
			if rep.Status != HealthDown {
				rep.Status = HealthDegraded
			}
		}
	}
	return rep
}

// probe runs one Health check with panic isolation.
func probe(ctx context.Context, h Healther) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("health probe panicked: %v", p)
		}
	}()
	return h.Health(ctx)
}

// Ready aggregates readiness: draining fails immediately (503 before
// traffic drop), then every enabled required component must be Ready
// and every Readier must pass.
func (r *Registry) Ready(ctx context.Context) error {
	v := r.view.Load()
	if v.draining {
		return ErrDraining
	}
	if !r.started.Load() || r.stopped.Load() {
		return ErrNotStarted
	}
	var errs []error
	for _, k := range v.order {
		e := v.entries[k]
		if !e.enabled || e.desc.Optional {
			continue
		}
		if e.state != StateReady {
			errs = append(errs, fmt.Errorf("component %s is %s", k, e.state))
			continue
		}
		if rd, ok := e.comp.(Readier); ok {
			if err := rd.ReadyCheck(ctx); err != nil {
				errs = append(errs, fmt.Errorf("component %s not ready: %w", k, err))
			}
		}
	}
	return errors.Join(errs...)
}

// --- view publication (actor-side) --------------------------------------

func (r *Registry) publishView(draining bool) {
	v := &view{entries: make(map[Key]*viewEntry, len(r.slots)), draining: draining}
	for _, s := range r.slots {
		v.order = append(v.order, s.key)
		v.entries[s.key] = &viewEntry{
			comp: s.comp, desc: s.desc, cfgKey: s.configKey,
			state: s.state, err: s.lastErr, enabled: s.enabled,
		}
	}
	r.view.Store(v)
}

// --- startup -------------------------------------------------------------

func (r *Registry) doStart(ctx context.Context) error {
	if r.startRan {
		return ErrAlreadyStarted
	}
	r.startRan = true
	begin := time.Now()

	// Enabled determination is a one-shot startup decision (SPEC §3.1
	// definition 4): flips during the process lifetime warn-only.
	snap := r.store.Snapshot()
	for _, s := range r.slots {
		s.enabled = snap.EnabledFor(s.configKey)
		if !s.enabled {
			s.state = StateDisabled
		}
	}
	r.publishView(false)

	if err := r.validateDeps(); err != nil {
		return err
	}
	levels, err := r.topoLevels()
	if err != nil {
		return err
	}
	r.levels = levels

	r.logger.Info("kernel: starting components",
		"count", len(r.enabledSlots()), "levels", len(levels), "disabled", len(r.slots)-len(r.enabledSlots()))

	// Init parallel per level; Migrate serial after each level.
	for _, level := range levels {
		if err := r.initLevel(ctx, level); err != nil {
			r.rollback(ctx)
			return err
		}
		if err := r.migrateLevel(ctx, level); err != nil {
			r.rollback(ctx)
			return err
		}
		r.publishView(false)
	}

	if err := r.mount(); err != nil {
		r.rollback(ctx)
		return err
	}

	if err := r.serve(ctx); err != nil {
		r.rollback(ctx)
		return err
	}

	r.started.Store(true)
	r.publishView(false)
	d := time.Since(begin)
	r.logger.Info("kernel: started", "duration", d)
	event.Publish(ctx, r.bus, AppStarted{Duration: d})
	return nil
}

func (r *Registry) enabledSlots() []*slot {
	out := make([]*slot, 0, len(r.slots))
	for _, s := range r.slots {
		if s.enabled {
			out = append(out, s)
		}
	}
	return out
}

// validateDeps checks the full graph before any Init runs: hard deps
// must exist and be enabled (SPEC §3.1 definition 1).
func (r *Registry) validateDeps() error {
	var errs []error
	for _, s := range r.slots {
		if !s.enabled {
			continue
		}
		for _, dep := range s.desc.Needs {
			dk := Key{Kind: dep.Kind, Instance: dep.Instance}
			if dk.Instance == "" {
				dk.Instance = DefaultInstance
			}
			target, exists := r.byKey[dk]
			switch {
			case !exists:
				if !dep.Optional {
					errs = append(errs, fmt.Errorf("kernel: %s requires %s which is not assembled", s.key, dk))
				}
			case !target.enabled:
				if !dep.Optional {
					errs = append(errs, fmt.Errorf("kernel: %s requires %s which is disabled by config", s.key, dk))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// topoLevels layers enabled components with Kahn's algorithm.
// Deterministic: within a level, assembly order is preserved.
func (r *Registry) topoLevels() ([][]*slot, error) {
	enabled := r.enabledSlots()
	indeg := make(map[*slot]int, len(enabled))
	dependents := make(map[*slot][]*slot, len(enabled))
	for _, s := range enabled {
		indeg[s] += 0
		for _, dep := range s.desc.Needs {
			dk := Key{Kind: dep.Kind, Instance: dep.Instance}
			if dk.Instance == "" {
				dk.Instance = DefaultInstance
			}
			t, ok := r.byKey[dk]
			if !ok || !t.enabled {
				continue // optional-absent (hard-absent already failed validateDeps)
			}
			indeg[s]++
			dependents[t] = append(dependents[t], s)
		}
	}

	var levels [][]*slot
	placed := 0
	frontier := make([]*slot, 0, len(enabled))
	for _, s := range enabled { // assembly order
		if indeg[s] == 0 {
			frontier = append(frontier, s)
		}
	}
	for len(frontier) > 0 {
		levels = append(levels, frontier)
		placed += len(frontier)
		var next []*slot
		for _, s := range frontier {
			for _, d := range dependents[s] {
				indeg[d]--
				if indeg[d] == 0 {
					next = append(next, d)
				}
			}
		}
		frontier = next
	}
	if placed != len(enabled) {
		var cyc []string
		for _, s := range enabled {
			if indeg[s] > 0 {
				cyc = append(cyc, s.key.String())
			}
		}
		sort.Strings(cyc)
		return nil, fmt.Errorf("kernel: dependency cycle involving %v", cyc)
	}
	return levels, nil
}

func (r *Registry) initLevel(ctx context.Context, level []*slot) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, s := range level {
		wg.Add(1)
		go func(s *slot) {
			defer wg.Done()
			start := time.Now()
			err := runBounded(ctx, r.timeoutFor(s, "init"), fmt.Sprintf("init %s", s.key), func(c context.Context) error {
				return s.comp.Init(c, r)
			})
			d := time.Since(start)
			if err == nil {
				s.state = StateReady
				r.logger.Info("kernel: component initialized", "component", s.key.String(), "duration", d)
				event.Publish(ctx, r.bus, ComponentInitialized{Key: s.key, Duration: d})
				return
			}
			if s.desc.Optional {
				s.state = StateDegraded
				s.lastErr = err.Error()
				r.logger.Warn("kernel: optional component failed, continuing degraded",
					"component", s.key.String(), "error", err)
				event.Publish(ctx, r.bus, ComponentDegraded{Key: s.key, Err: err.Error()})
				return
			}
			s.state = StateFailed
			s.lastErr = err.Error()
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (r *Registry) migrateLevel(ctx context.Context, level []*slot) error {
	for _, s := range level {
		if s.state != StateReady {
			continue
		}
		m, ok := s.comp.(Migrator)
		if !ok {
			continue
		}
		err := runBounded(ctx, r.timeoutFor(s, "init"), fmt.Sprintf("migrate %s", s.key), m.Migrate)
		if err != nil {
			s.state = StateFailed
			s.lastErr = err.Error()
			return err
		}
	}
	return nil
}

// mount runs the fixed mount phase: MountOrder ≤ 0 mounters (stable by
// topological start order) → user Routes callback → MountOrder > 0
// ascending. The Router comes from the single RouterProvider among the
// enabled components; assembling mounters or Routes without one is a
// startup error, and so is assembling two providers (the kernel knows
// the ROLE, never a battery name).
func (r *Registry) mount() error {
	var provider RouterProvider
	var providerKey Key
	for _, level := range r.levels {
		for _, s := range level {
			if s.state != StateReady {
				continue
			}
			p, ok := s.comp.(RouterProvider)
			if !ok {
				continue
			}
			if provider != nil {
				return fmt.Errorf("kernel: multiple RouterProviders assembled (%s and %s)", providerKey, s.key)
			}
			provider = p
			providerKey = s.key
		}
	}

	type mountable struct {
		s *slot
		m Mounter
	}
	var pre, post []mountable
	for _, level := range r.levels {
		for _, s := range level {
			if s.state != StateReady {
				continue
			}
			m, ok := s.comp.(Mounter)
			if !ok {
				continue
			}
			if s.desc.MountOrder > 0 {
				post = append(post, mountable{s, m})
			} else {
				pre = append(pre, mountable{s, m})
			}
		}
	}
	sort.SliceStable(post, func(i, j int) bool {
		return post[i].s.desc.MountOrder < post[j].s.desc.MountOrder
	})

	if provider == nil {
		if len(pre)+len(post) == 0 && r.routes == nil {
			return nil // nothing to mount, no router needed
		}
		return fmt.Errorf("kernel: %d mounter(s) and Routes callback present but no RouterProvider assembled", len(pre)+len(post))
	}
	router := provider.ProvideRouter()
	if router == nil {
		return fmt.Errorf("kernel: RouterProvider %s returned a nil Router", providerKey)
	}

	for _, mb := range pre {
		if err := mb.m.Mount(router); err != nil {
			return fmt.Errorf("kernel: mount %s: %w", mb.s.key, err)
		}
	}
	if r.routes != nil {
		if err := r.routes(router); err != nil {
			return fmt.Errorf("kernel: user routes: %w", err)
		}
	}
	for _, mb := range post {
		if err := mb.m.Mount(router); err != nil {
			return fmt.Errorf("kernel: mount %s: %w", mb.s.key, err)
		}
	}
	return nil
}

// serve starts every Server in parallel and waits for the aggregate
// ready. A Serve returning before Stop — nil or not — is a failure
// signal (v1 contract: unexpected server exit shuts the app down).
func (r *Registry) serve(ctx context.Context) error {
	var servers []*slot
	for _, level := range r.levels {
		for _, s := range level {
			if s.state != StateReady {
				continue
			}
			if _, ok := s.comp.(Server); ok {
				servers = append(servers, s)
			}
		}
	}
	if len(servers) == 0 {
		return nil
	}

	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.serveCancel = cancel

	readyCh := make(chan Key, len(servers))
	exitCh := make(chan serveExit, len(servers))

	for _, s := range servers {
		srv := s.comp.(Server)
		key := s.key
		var once sync.Once
		ready := func() { once.Do(func() { readyCh <- key }) }
		r.serveWG.Add(1)
		go func() {
			defer r.serveWG.Done()
			err := serveGuarded(serveCtx, srv, ready)
			exitCh <- serveExit{key: key, err: err}
		}()
	}

	// Monitor: forwards post-startup exits into failedCh.
	monitorStop := make(chan struct{})
	pending := len(servers)
	go func() {
		for pending > 0 {
			select {
			case <-monitorStop:
				return
			case ex := <-exitCh:
				pending--
				select {
				case <-serveCtx.Done(): // stopping — exits are expected
					if ex.err != nil {
						r.logger.Warn("kernel: server exited with error during shutdown",
							"component", ex.key.String(), "error", ex.err)
					}
				default:
					err := ex.err
					if err == nil {
						err = fmt.Errorf("kernel: server %s exited unexpectedly", ex.key)
					} else {
						err = fmt.Errorf("kernel: server %s failed: %w", ex.key, err)
					}
					select {
					case r.failedCh <- err:
					default:
					}
				}
			}
		}
	}()
	_ = monitorStop

	// Aggregate readiness, bounded by the start ctx; a pre-ready exit
	// aborts startup.
	got := 0
	for got < len(servers) {
		select {
		case <-readyCh:
			got++
		case err := <-r.failedCh:
			return err
		case <-ctx.Done():
			return classifyCtxErr(ctx, "waiting for servers ready")
		}
	}
	r.logger.Info("kernel: all servers ready", "count", len(servers))
	return nil
}

// serveGuarded isolates Serve panics into errors.
func serveGuarded(ctx context.Context, srv Server, ready func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("serve panicked: %v", p)
		}
	}()
	return srv.Serve(ctx, ready)
}

// rollback closes everything already initialized, in reverse
// topological order, after a startup failure.
func (r *Registry) rollback(ctx context.Context) {
	r.logger.Warn("kernel: startup failed, rolling back")
	if r.serveCancel != nil {
		r.serveCancel()
		r.serveWG.Wait()
	}
	r.closeAll(context.WithoutCancel(ctx))
	r.stopped.Store(true)
	r.publishView(false)
}

// --- shutdown -------------------------------------------------------------

func (r *Registry) doStop(ctx context.Context) error {
	if r.stopped.Load() {
		return nil
	}
	if !r.startRan {
		r.stopped.Store(true)
		return nil
	}
	begin := time.Now()

	// Phase: draining. Broadcast Drain to every implementer (parallel,
	// short budget), then honour the configured drain delay, then
	// cancel the Serve contexts and wait for every server to return —
	// in-flight work finishes while all dependencies are still alive.
	r.publishView(true)
	r.drainAll(ctx)

	if r.drainDelay > 0 {
		r.logger.Info("kernel: drain delay", "delay", r.drainDelay)
		select {
		case <-time.After(r.drainDelay):
		case <-ctx.Done():
			r.logger.Warn("kernel: drain delay cut short", "reason", ctx.Err())
		}
	}

	if r.serveCancel != nil {
		r.serveCancel()
		r.serveWG.Wait()
	}

	closeErr := r.closeAll(context.WithoutCancel(ctx))

	d := time.Since(begin)
	event.Publish(context.WithoutCancel(ctx), r.bus, AppStopped{Duration: d})

	busCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), busCloseBudget)
	defer cancel()
	r.bus.Close(busCtx)

	r.stopped.Store(true)
	r.logger.Info("kernel: stopped", "duration", d)
	return closeErr
}

func (r *Registry) drainAll(ctx context.Context) {
	var drainers []*slot
	for _, s := range r.slots {
		if s.state != StateReady {
			continue
		}
		if _, ok := s.comp.(Drainer); ok {
			drainers = append(drainers, s)
		}
	}
	if len(drainers) == 0 {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainBroadcastTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range drainers {
		wg.Add(1)
		go func(s *slot) {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					r.logger.Error("kernel: drain panicked", "component", s.key.String(), "panic", p)
				}
			}()
			s.comp.(Drainer).Drain(dctx)
		}(s)
	}
	wg.Wait()
}

// closeAll closes initialized components in reverse topological order,
// levels sequential, components within a level parallel. The view
// shrinks after each level — Lookup during shutdown only sees what is
// still alive (v1's "available lazy scrub" as structure).
func (r *Registry) closeAll(ctx context.Context) error {
	var errs []error
	for i := len(r.levels) - 1; i >= 0; i-- {
		level := r.levels[i]
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, s := range level {
			if s.state != StateReady && s.state != StateDegraded {
				continue
			}
			if s.state == StateDegraded && s.lastErr != "" && s.desc.Optional {
				// Degraded = Init failed: nothing to close.
				s.state = StateClosed
				continue
			}
			wg.Add(1)
			go func(s *slot) {
				defer wg.Done()
				start := time.Now()
				err := runBounded(ctx, r.timeoutFor(s, "close"), fmt.Sprintf("close %s", s.key), s.comp.Close)
				d := time.Since(start)
				s.state = StateClosed
				msg := ""
				if err != nil {
					msg = err.Error()
					s.lastErr = msg
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					r.logger.Warn("kernel: component close failed", "component", s.key.String(), "error", err)
				} else {
					r.logger.Info("kernel: component closed", "component", s.key.String(), "duration", d)
				}
				event.Publish(ctx, r.bus, ComponentClosed{Key: s.key, Duration: d, Err: msg})
			}(s)
		}
		wg.Wait()
		r.publishView(true)
	}
	return errors.Join(errs...)
}

// --- reload ----------------------------------------------------------------

// doReload is the three-stage pipeline (SPEC §3.5 layer one): config
// swap → component dispatch → user post-reload callback; each failure
// short-circuits the rest.
func (r *Registry) doReload(ctx context.Context) error {
	begin := time.Now()

	diff, err := r.store.Reload()
	if err != nil {
		return fmt.Errorf("kernel: config reload: %w", err)
	}

	var reloaded, restartPending []string

	// Stage two: sectioned components with hot changes, topo order.
	for _, level := range r.levels {
		for _, s := range level {
			if s.state != StateReady || s.configKey == "" {
				continue
			}
			ch, ok := diff.Sections[s.configKey]
			if !ok || !ch.Changed() {
				continue
			}
			if ch.EnabledFlipped {
				r.logger.Warn("kernel: enabled flipped in config — restart-only, not applied at runtime",
					"component", s.key.String())
			}
			for _, f := range ch.Restart {
				if ch.EnabledFlipped && f == s.configKey+".enabled" {
					continue // already warned above with clearer wording
				}
				r.logger.Warn("kernel: restart-only config field changed — not applied until restart",
					"component", s.key.String(), "field", f)
				restartPending = append(restartPending, f)
			}
			if len(ch.Hot) == 0 {
				continue
			}
			rl, ok := s.comp.(Reloader)
			if !ok {
				r.logger.Warn("kernel: hot config fields changed but component has no Reload — restart required",
					"component", s.key.String(), "fields", ch.Hot)
				restartPending = append(restartPending, ch.Hot...)
				continue
			}
			if err := runBounded(ctx, r.timeoutFor(s, "reload"), fmt.Sprintf("reload %s", s.key), rl.Reload); err != nil {
				return fmt.Errorf("kernel: reload %s: %w", s.key, err)
			}
			reloaded = append(reloaded, s.key.String())
		}
	}

	// No-section Reloaders: every reload, after all sectioned ones
	// (SPEC §3.4 — no dispatch dead corners).
	for _, level := range r.levels {
		for _, s := range level {
			if s.state != StateReady || s.configKey != "" {
				continue
			}
			rl, ok := s.comp.(Reloader)
			if !ok {
				continue
			}
			if err := runBounded(ctx, r.timeoutFor(s, "reload"), fmt.Sprintf("reload %s", s.key), rl.Reload); err != nil {
				return fmt.Errorf("kernel: reload %s: %w", s.key, err)
			}
			reloaded = append(reloaded, s.key.String())
		}
	}

	// Stage three: user callback — only after everything else worked;
	// its error fails the whole reload (v1 gating contract).
	if r.postReload != nil {
		if err := r.postReload(ctx); err != nil {
			return fmt.Errorf("kernel: post-reload callback: %w", err)
		}
	}

	d := time.Since(begin)
	r.logger.Info("kernel: reload applied", "duration", d, "reloaded", reloaded)
	event.Publish(ctx, r.bus, ReloadApplied{Duration: d, Reloaded: reloaded, RestartPending: restartPending})
	return nil
}

// --- shared helpers ---------------------------------------------------------

func (r *Registry) timeoutFor(s *slot, phase string) time.Duration {
	pick := func(specific, global, fallback time.Duration) time.Duration {
		if specific > 0 {
			return specific
		}
		if global > 0 {
			return global
		}
		return fallback
	}
	switch phase {
	case "init":
		return pick(s.desc.Timeouts.Init, r.defaults.Init, DefaultInitTimeout)
	case "close":
		return pick(s.desc.Timeouts.Close, r.defaults.Close, DefaultCloseTimeout)
	default:
		return pick(s.desc.Timeouts.Reload, r.defaults.Reload, DefaultReloadTimeout)
	}
}

// runBounded executes op under a timeout with panic isolation, and —
// the M0 lesson — reports cancellation and deadline expiry as
// distinguishable errors instead of blaming everything on "timeout".
func runBounded(parent context.Context, d time.Duration, what string, op func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()

	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("%s panicked: %v", what, p)
			}
		}()
		return op(ctx)
	}()
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
		return fmt.Errorf("%s: timeout exceeded after %s", what, d)
	}
	if errors.Is(err, context.Canceled) || parent.Err() != nil {
		return fmt.Errorf("%s: cancelled: %w", what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func classifyCtxErr(ctx context.Context, what string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("kernel: timeout %s", what)
	}
	return fmt.Errorf("kernel: cancelled %s", what)
}
