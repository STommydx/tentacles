package slot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/0xinterface/tentacles/internal/cleanup"
	"github.com/0xinterface/tentacles/internal/runner"
)

// Default table tunables. The reconciler and app layers can override them
// with the With* options; production values come from config.
const (
	defaultAcquireGrace   = 3 * time.Minute
	defaultIdleGrace      = 30 * time.Second
	defaultStopTimeout    = 30 * time.Second
	defaultCleanupTimeout = 60 * time.Second
	defaultStartTimeout   = 90 * time.Second
	defaultWorkDir        = "_work"
	defaultNamespace      = "default"

	maxSlotID = 9999
)

// tableOptions holds the tunables for a Table.
type tableOptions struct {
	namespace          string
	startAllowed       func() bool
	reservation        func() (float64, uint64)
	acquireGrace       time.Duration
	idleGrace          time.Duration
	stopTimeout        time.Duration
	cleanupTimeout     time.Duration
	startTimeout       time.Duration
	startObserver      func(time.Duration)
	diagHook           func(Slot) error
	eventHook          func(Event, Slot)
	envFile            string
	user               string
	group              string
	cpuQuota           string
	memoryMax          string
	workDir            string
	usageSampler       func(unit string) (Usage, error)
	sampleInterval     time.Duration
	gate               func(live []Slot) bool
	completionHook     func(Completion)
	materializeContext func(context.Context, string) error
	diagContext        func(context.Context, Slot) error
}

// WithNamespace assigns the pool ID used in every backend unit name.
func WithNamespace(namespace string) TableOption {
	return func(o *tableOptions) { o.namespace = namespace }
}

// WithStartAllowed checks an acquisition pause before every new launch.
func WithStartAllowed(fn func() bool) TableOption {
	return func(o *tableOptions) { o.startAllowed = fn }
}

// WithReservation captures resource estimates before an unclaimed runner starts.
func WithReservation(fn func() (float64, uint64)) TableOption {
	return func(o *tableOptions) { o.reservation = fn }
}

// WithMaterializer supplies a cancellable copy into a directory reserved by the table.
func WithMaterializer(fn func(context.Context, string) error) TableOption {
	return func(o *tableOptions) { o.materializeContext = fn }
}

// WithDiagContext supplies a cancellable diagnostic hook for the complete cleanup deadline.
func WithDiagContext(fn func(context.Context, Slot) error) TableOption {
	return func(o *tableOptions) { o.diagContext = fn }
}

// defaultSampleInterval is how often live slot units are polled for
// resource accounting when a usage sampler is configured.
const defaultSampleInterval = 30 * time.Second

// TableOption configures a Table. Options are applied in order; the last
// one wins for each field.
type TableOption func(*tableOptions)

// WithAcquireGrace sets the window a freshly started runner has to claim
// its first job. A process exit before the window closes without a job
// is classified as an acquire failure, and a slot still in
// "starting" past the window may be stopped as surplus. Non-positive
// values disable both uses. Default 3m.
func WithAcquireGrace(d time.Duration) TableOption {
	return func(o *tableOptions) { o.acquireGrace = d }
}

// WithIdleGrace sets the window after a slot becomes idle during which
// surplus scale-down will not stop it. GitHub assigns jobs to idle
// runners out of band, and the JobStarted message that marks the slot
// busy arrives only after the assignment; stopping a freshly idle slot
// in that gap kills a job that never had a chance to run (issue #4).
// The window only delays surplus stops: daemon shutdown bypasses it via
// Shutdown, and busy slots are never stopped either way. Non-positive
// values disable the protection. Default 30s.
func WithIdleGrace(d time.Duration) TableOption {
	return func(o *tableOptions) { o.idleGrace = d }
}

// WithStopTimeout bounds each backend.Stop call issued by the Table.
// Default 30s.
func WithStopTimeout(d time.Duration) TableOption {
	return func(o *tableOptions) { o.stopTimeout = d }
}

// WithCleanupTimeout bounds the slot-directory wipe after a runner exits.
// Default 60s.
func WithCleanupTimeout(d time.Duration) TableOption {
	return func(o *tableOptions) { o.cleanupTimeout = d }
}

// WithStartTimeout bounds each backend.Start call. Default 90s.
func WithStartTimeout(d time.Duration) TableOption {
	return func(o *tableOptions) { o.startTimeout = d }
}

// WithStartObserver registers a callback invoked once per successful
// slot start with the total provision time (materialize + JIT mint +
// backend start). Used for the slot_start histogram.
func WithStartObserver(fn func(time.Duration)) TableOption {
	return func(o *tableOptions) { o.startObserver = fn }
}

// WithDiagHook registers a best-effort diagnostic callback invoked with
// the exiting slot before its directory is wiped. Errors are logged, not
// fatal. Used to ship _diag before cleanup.
func WithDiagHook(hook func(Slot) error) TableOption {
	return func(o *tableOptions) { o.diagHook = hook }
}

// WithEventHook registers a lifecycle event sink. See the Event constants
// for the vocabulary.
func WithEventHook(hook func(Event, Slot)) TableOption {
	return func(o *tableOptions) { o.eventHook = hook }
}

// WithEnvFile sets the environment file (systemd EnvironmentFile syntax)
// passed to the backend for every slot.
func WithEnvFile(path string) TableOption {
	return func(o *tableOptions) { o.envFile = path }
}

// WithRunUser sets the unix user (and optional group) the runner process
// runs as.
func WithRunUser(user, group string) TableOption {
	return func(o *tableOptions) { o.user, o.group = user, group }
}

// WithLimits sets the CPU quota and memory ceiling passed to the backend
// (systemd CPUQuota / MemoryMax syntax, e.g. "400%" and "8G").
func WithLimits(cpuQuota, memoryMax string) TableOption {
	return func(o *tableOptions) { o.cpuQuota, o.memoryMax = cpuQuota, memoryMax }
}

// WithWorkDir sets the work-directory name inside each slot. Default
// "_work".
func WithWorkDir(rel string) TableOption {
	return func(o *tableOptions) { o.workDir = rel }
}

// WithUsageSampler registers a callback that reads cumulative resource
// accounting for a unit (systemd CPUUsageNSec/MemoryPeak in the
// production backend). Non-nil enables per-job usage recording.
func WithUsageSampler(fn func(unit string) (Usage, error)) TableOption {
	return func(o *tableOptions) { o.usageSampler = fn }
}

// WithSampleInterval overrides the usage sampling period. Non-positive
// values fall back to the default 30s.
func WithSampleInterval(d time.Duration) TableOption {
	return func(o *tableOptions) { o.sampleInterval = d }
}

// WithGate registers the admission gate. Before each slot start it
// receives the live slots and reports whether the host budget can take
// another job. A refusal is backpressure: the start is retried on a
// later tick, and reported as EventAdmissionHold, not a failure. Nil
// admits everything.
func WithGate(fn func(live []Slot) bool) TableOption {
	return func(o *tableOptions) { o.gate = fn }
}

// WithCompletionHook registers a sink for per-job usage records, fired
// once per claimed slot exit. Unclaimed exits (acquire failures) are
// not completions.
func WithCompletionHook(fn func(Completion)) TableOption {
	return func(o *tableOptions) { o.completionHook = fn }
}

// slotRec is the internal bookkeeping for one slot. The embedded Slot is
// the public view; the remaining fields drive lifecycle decisions and
// usage attribution.
type slotRec struct {
	Slot
	busySince      time.Time // set when the runner first claims a job; zero = never busy
	provStart      time.Time // when provisioning began; drives starting-past-grace stops
	claimed        bool      // a JobStarted was seen for this slot
	result         string    // GitHub-reported job result (JobCompleted message), empty until it arrives
	queueWait      float64   // GitHub-reported queue wait at claim
	claimAt        time.Time // local time of the claim
	usageAtClaim   Usage     // sampler reading at claim time
	lastUsage      Usage     // most recent sampler reading
	sampledOK      bool      // at least one sampler reading succeeded
	cleaning       bool
	cleanupPending bool
	exitEvent      Event
	completionSent bool
	cleanDone      chan struct{}
}

// Table is the concrete Manager: it allocates slot IDs, materializes and
// starts runner slots through the injected backend, and wipes them on
// exit. It is safe for concurrent use; the reconciler is the only
// writer, but metrics/listers may read at any time.
type Table struct {
	root        string
	backend     runner.Backend
	materialize func(dst string) error
	jit         func(ctx context.Context, id ID) (runner.JIT, error)
	jitDir      string
	log         *slog.Logger
	opts        tableOptions

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	slots   map[ID]*slotRec
	desired int
	closed  bool
	ops     chan struct{}
}

// NewTable builds a slot table rooted at root (the directory that holds
// the per-slot subdirectories "0001" … "9999"). materialize populates a
// fresh slot directory (e.g. copying the runner payload template); jit
// mints a one-job JIT config for a slot (returning the GitHub runner
// name); jitDir holds the 0600 JIT files, one per slot ID.
func NewTable(root string, backend runner.Backend, materialize func(dst string) error, jit func(ctx context.Context, id ID) (runner.JIT, error), jitDir string, log *slog.Logger, opts ...TableOption) *Table {
	if log == nil {
		log = slog.Default()
	}
	o := tableOptions{
		namespace:      defaultNamespace,
		acquireGrace:   defaultAcquireGrace,
		stopTimeout:    defaultStopTimeout,
		idleGrace:      defaultIdleGrace,
		cleanupTimeout: defaultCleanupTimeout,
		startTimeout:   defaultStartTimeout,
		workDir:        defaultWorkDir,
		sampleInterval: defaultSampleInterval,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.sampleInterval <= 0 {
		o.sampleInterval = defaultSampleInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &Table{
		root:        root,
		backend:     backend,
		materialize: materialize,
		jit:         jit,
		jitDir:      jitDir,
		log:         log,
		opts:        o,
		ctx:         ctx,
		cancel:      cancel,
		slots:       make(map[ID]*slotRec),
		ops:         make(chan struct{}, 1),
	}
	if o.usageSampler != nil {
		go t.sampleLoop()
	}
	return t
}

// sampleLoop polls the usage sampler for every live slot so a job's
// resource consumption is known even though systemd garbage-collects
// the unit right after exit. CPU seconds are cumulative; peak memory is
// maintained by the kernel, so the latest reading is the peak so far.
func (t *Table) sampleLoop() {
	ticker := time.NewTicker(t.opts.sampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
		}
		type target struct {
			rec  *slotRec
			unit string
		}
		t.mu.Lock()
		var targets []target
		for _, rec := range t.slots {
			if rec.State == StateBusy || rec.State == StateIdle {
				targets = append(targets, target{rec: rec, unit: rec.Unit})
			}
		}
		t.mu.Unlock()
		for _, tg := range targets {
			u, err := t.opts.usageSampler(tg.unit)
			if err != nil {
				continue // unit may have just exited; the last sample stands
			}
			t.mu.Lock()
			if cur := t.slots[tg.rec.ID]; cur == tg.rec && !cur.cleaning && !cur.cleanupPending {
				cur.lastUsage = u
				cur.sampledOK = true
				cur.CurrentMemBytes = u.CurrentMemBytes
			}
			t.mu.Unlock()
		}
	}
}

// Desired returns the last desired runner count pushed by the listener.
func (t *Table) Desired() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.desired
}

// SetDesired records the desired runner count. The reconciler calls this
// before Ensure so the value is observable even while convergence is in
// flight.
func (t *Table) SetDesired(n int) {
	t.mu.Lock()
	t.desired = n
	t.mu.Unlock()
}

// WorkDir returns the absolute work-directory path for a slot ID (the
// JIT work folder). It is the single source of truth for the
// configured work-directory name.
func (t *Table) WorkDir(id ID) string {
	return filepath.Join(t.root, string(id), t.opts.workDir)
}

// Active returns the slots that count toward the actual runner count
// (starting, idle, busy), oldest ID first.
func (t *Table) Active() []Slot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(func(st State) bool { return st.Live() })
}

// Snapshot returns a copy of every tracked slot (including stopping),
// oldest ID first.
func (t *Table) Snapshot() []Slot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(func(State) bool { return true })
}

func (t *Table) snapshotLocked(keep func(State) bool) []Slot {
	out := make([]Slot, 0, len(t.slots))
	for _, rec := range t.slots {
		if keep(rec.State) {
			out = append(out, rec.Slot)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Ensure converges the live slot count toward desired. Missing slots are
// started sequentially; surplus idle/starting slots are stopped (oldest
// first), an idle slot only once the idle grace has passed. It is
// idempotent: redelivery of the same desired count is a no-op. Busy
// slots are never stopped. A failed start leaves desired unsatisfied and
// is retried on the next tick.
func (t *Table) Ensure(ctx context.Context, desired int) error {
	select {
	case t.ops <- struct{}{}:
		defer func() { <-t.ops }()
	case <-ctx.Done():
		return ctx.Err()
	}
	t.retryCleanup(ctx)
	for {
		t.mu.Lock()
		live := t.countLiveLocked()
		closed := t.closed
		t.mu.Unlock()
		if closed || live >= desired {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.startOne(ctx) == "" {
			// Start failed (logged and emitted); do not spin.
			break
		}
	}
	t.stopSurplusContext(ctx, desired, false)
	return ctx.Err()
}

// Shutdown stops every eligible slot immediately, bypassing the idle
// grace: the daemon is going away, so an in-flight assignment can no
// longer be honored through this table and a freshly idle slot loses its
// protection. Busy slots are still never stopped — they finish naturally
// and are adopted from the pool namespace at next boot.
func (t *Table) Shutdown(ctx context.Context) error {
	select {
	case t.ops <- struct{}{}:
		defer func() { <-t.ops }()
	case <-ctx.Done():
		return ctx.Err()
	}
	t.retryCleanup(ctx)
	t.stopSurplusContext(ctx, 0, true)
	return ctx.Err()
}

func (t *Table) countLiveLocked() int {
	n := 0
	for _, rec := range t.slots {
		if rec.State.Live() {
			n++
		}
	}
	return n
}

// startOne provisions and starts a single slot. It returns the new slot
// ID, or "" if the start failed (already logged and emitted as an
// "acquire_failure" event).
func (t *Table) startOne(ctx context.Context) ID {
	if t.opts.startAllowed != nil && !t.opts.startAllowed() {
		return ""
	}
	// Gate before reserving the candidate: existing starting and idle slots
	// are reservations too, but the candidate must only be charged once.
	if t.opts.gate != nil && !t.opts.gate(t.Active()) {
		t.emit(EventAdmissionHold, Slot{State: StateStarting})
		return ""
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ""
	}
	id, ok := t.nextIDLocked()
	if !ok {
		t.mu.Unlock()
		t.log.Error("no free slot ids", "max", maxSlotID)
		return ""
	}
	t0 := time.Now()
	rec := &slotRec{Slot: Slot{Pool: t.opts.namespace, ID: id, Dir: filepath.Join(t.root, string(id)), Unit: unitName(t.opts.namespace, id), State: StateStarting}, provStart: t0}
	if t.opts.reservation != nil {
		rec.ReservedCores, rec.ReservedMemBytes = t.opts.reservation()
	}
	t.slots[id] = rec
	t.mu.Unlock()

	// Only a successful exclusive mkdir grants this attempt ownership of a
	// directory. In particular, a surviving job's directory is never erased.
	if err := os.Mkdir(rec.Dir, 0o755); err != nil {
		t.mu.Lock()
		delete(t.slots, id)
		snap := rec.Slot
		t.mu.Unlock()
		t.log.Error("reserve slot directory", "slot", id, "err", err)
		t.emit(EventAcquireFailure, snap)
		return ""
	}
	fail := func(err error) {
		t.log.Warn("slot start failed", "slot", id, "err", err)
		t.mu.Lock()
		rec.State = StateStopping
		snap := rec.Slot
		t.mu.Unlock()
		t.emit(EventAcquireFailure, snap)
		t.clean(ctx, rec)
	}
	startCtx, cancel := context.WithTimeout(ctx, t.opts.startTimeout)
	defer cancel()
	var err error
	if t.opts.materializeContext != nil {
		err = t.opts.materializeContext(startCtx, rec.Dir)
	} else {
		err = t.materialize(rec.Dir)
	}
	if err != nil {
		fail(fmt.Errorf("materialize slot: %w", err))
		return ""
	}
	jit, err := t.jit(startCtx, id)
	if err != nil {
		fail(fmt.Errorf("mint JIT: %w", err))
		return ""
	}
	t.mu.Lock()
	rec.RunnerName = jit.RunnerName
	rec.JITPath = filepath.Join(t.jitDir, string(id)+".jit")
	spec := runner.Spec{SlotDir: rec.Dir, JITPath: rec.JITPath, EnvFile: t.opts.envFile, User: t.opts.user, Group: t.opts.group, CPUQuota: t.opts.cpuQuota, MemoryMax: t.opts.memoryMax, UnitName: rec.Unit}
	t.mu.Unlock()
	if err := runner.WriteJIT(spec.JITPath, jit.Encoded); err != nil {
		fail(fmt.Errorf("write JIT: %w", err))
		return ""
	}
	if err := t.backend.Start(startCtx, spec); err != nil {
		if errors.Is(err, runner.ErrNotStarted) {
			fail(err)
			return ""
		}
		// A lost launch reply can race with a claim arriving at any time.
		// Never compensate by stopping a runner whose execution is uncertain.
		t.mu.Lock()
		rec.State = StateBusy
		snap := rec.Slot
		t.mu.Unlock()
		t.log.Warn("launch uncertain; retaining slot until confirmed exit", "slot", id, "err", err)
		t.emit(EventAcquireFailure, snap)
		go t.watch(rec)
		return ""
	}

	t.mu.Lock()
	rec.StartedAt = time.Now()
	if rec.State == StateStarting {
		rec.State = StateIdle
	}
	snap := rec.Slot
	t.mu.Unlock()
	t.log.Info("slot started", "slot", id, "unit", spec.UnitName, "runner_name", snap.RunnerName)
	t.emit(EventStarted, snap)
	if t.opts.startObserver != nil {
		t.opts.startObserver(time.Since(t0))
	}
	go t.watch(rec)
	return id
}

// nextIDLocked returns the lowest unused zero-padded slot ID.
func (t *Table) nextIDLocked() (ID, bool) {
	for n := 1; n <= maxSlotID; n++ {
		id := ID(fmt.Sprintf("%04d", n))
		if _, ok := t.slots[id]; !ok {
			return id, true
		}
	}
	return "", false
}

// stopSurplus stops the oldest surplus slots, never busy ones. If the
// only live slots are busy, nothing is stopped. Unless ignoreIdleGrace
// is set (daemon shutdown), a slot that became idle within the idle
// grace is spared: GitHub may have already assigned it a job whose
// JobStarted message has not been delivered yet (issue #4).
func (t *Table) stopSurplus(desired int) {
	t.stopSurplusContext(t.ctx, desired, false)
}

func (t *Table) stopSurplusContext(ctx context.Context, desired int, ignoreIdleGrace bool) {
	type candidate struct {
		rec  *slotRec
		snap Slot
	}
	var candidates []candidate

	t.mu.Lock()
	excess := t.countLiveLocked() - desired
	if excess > 0 {
		for _, rec := range t.slots {
			switch rec.State {
			case StateIdle:
				// Eligible only once the idle grace has passed: the
				// JobStarted message for an assignment GitHub already
				// made can still be in flight.
				if !ignoreIdleGrace && t.opts.idleGrace > 0 &&
					!rec.StartedAt.IsZero() && time.Since(rec.StartedAt) <= t.opts.idleGrace {
					continue
				}
			case StateStarting:
				// Starting slots become eligible only after acquire grace.
				if t.opts.acquireGrace > 0 && time.Since(rec.provStart) <= t.opts.acquireGrace {
					continue
				}
			default:
				continue
			}
			candidates = append(candidates, candidate{rec: rec, snap: rec.Slot})
		}
		// Oldest first; StartedAt ties broken by ID for determinism.
		sort.Slice(candidates, func(i, j int) bool {
			if !candidates[i].rec.StartedAt.Equal(candidates[j].rec.StartedAt) {
				return candidates[i].rec.StartedAt.Before(candidates[j].rec.StartedAt)
			}
			return candidates[i].rec.ID < candidates[j].rec.ID
		})
		if len(candidates) > excess {
			candidates = candidates[:excess]
		}
		for _, c := range candidates {
			c.rec.State = StateStopping
		}
	}
	t.mu.Unlock()

	for _, c := range candidates {
		t.log.Info("stopping surplus slot", "slot", c.rec.ID)
		stopCtx, cancel := context.WithTimeout(ctx, t.opts.stopTimeout)
		err := t.backend.Stop(stopCtx, c.rec.Unit)
		cancel()
		if err != nil {
			t.log.Warn("stop surplus slot failed", "slot", c.rec.ID, "err", err)
			t.mu.Lock()
			if cur, ok := t.slots[c.rec.ID]; ok && cur == c.rec && c.rec.State == StateStopping && !cur.cleanupPending && !cur.cleaning {
				c.rec.State = c.snap.State
			}
			t.mu.Unlock()
			continue
		}
		t.emit(EventStopped, c.snap)
		t.observeExitContext(ctx, c.rec.ID, c.rec, nil)
		t.waitCleanup(ctx, c.rec)
	}
}

// watch blocks on the backend until the unit exits, then hands the slot
// to ObserveExit for cleanup. On Table.Close the backend wait is
// cancelled and the slot is left in place for boot adoption.
func (t *Table) watch(rec *slotRec) {
	delay := 100 * time.Millisecond
	for {
		err := t.backend.Wait(t.ctx, rec.Unit)
		if t.ctx.Err() != nil {
			return
		}
		if err == nil {
			t.observeExit(rec.ID, rec, nil)
			return
		}
		t.log.Warn("runner exit observation failed; retaining slot", "slot", rec.ID, "err", err)
		timer := time.NewTimer(delay)
		select {
		case <-t.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 5*time.Second {
			delay = min(delay*2, 5*time.Second)
		}
	}
}

// MarkBusy flags the slot as busy so scale-down will not stop it. Only
// starting and idle slots transition; stopping/empty slots are left
// alone.
func (t *Table) MarkBusy(id ID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec, ok := t.slots[id]; ok {
		if rec.State == StateStarting || rec.State == StateIdle {
			rec.State = StateBusy
			rec.busySince = time.Now()
		}
	}
}

// Claim records that a runner claimed a job (JobStarted) and marks its
// slot busy so scale-down will not stop it. It reports whether a slot
// with that runner name was found.
func (t *Table) Claim(job ClaimJob) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rec := range t.slots {
		if rec.RunnerName != job.RunnerName {
			continue
		}
		if rec.State == StateStarting || rec.State == StateIdle || (rec.State == StateBusy && !rec.claimed) {
			rec.State = StateBusy
			rec.busySince = time.Now()
			rec.WorkflowRef = job.WorkflowRef
			rec.RunID = job.RunID
			rec.claimed = true
			rec.claimAt = time.Now()
			rec.queueWait = job.QueueWaitSeconds
			rec.usageAtClaim = rec.lastUsage
		}
		return true
	}
	return false
}

// MarkResult records the job result reported by the JobCompleted
// scale-set message so the slot's completion carries it. Results for
// unknown or already-exited runners are dropped: a completion already
// counted cannot retroactively change its label.
func (t *Table) MarkResult(runnerName, result string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if result == "" {
		return
	}
	for _, rec := range t.slots {
		if rec.RunnerName == runnerName && rec.State.Live() {
			rec.result = result
			return
		}
	}
}

// ObserveExit records a runner process exit and releases the slot: the
// diag hook runs (best-effort), the JIT file and slot directory are
// wiped, and the ID is freed for reuse.
func (t *Table) ObserveExit(id ID, err error) { t.observeExit(id, nil, err) }

func (t *Table) observeExit(id ID, expected *slotRec, err error) {
	t.observeExitContext(t.ctx, id, expected, err)
}

func (t *Table) observeExitContext(ctx context.Context, id ID, expected *slotRec, err error) {
	// This entry point is for confirmed process exits (including nonzero
	// exit status). Transport errors are retried by watch, never sent here.
	t.mu.Lock()
	rec, ok := t.slots[id]
	if !ok || (expected != nil && rec != expected) || rec.cleaning || rec.cleanupPending {
		t.mu.Unlock()
		return
	}
	prev := rec.State
	if prev == StateEmpty || prev == StateFailed {
		t.mu.Unlock()
		return
	}
	rec.State = StateStopping
	rec.cleanupPending = true
	rec.cleanDone = make(chan struct{})
	if prev != StateStopping {
		rec.exitEvent = EventExited
		if t.quickNeverBusyExit(rec, prev) {
			rec.exitEvent = EventAcquireFailure
		}
	}
	// Acquisition failures start backoff immediately, even when cleanup
	// fails or stalls. Clearing exitEvent prevents a second report on retry.
	var acquisitionFailure *Slot
	if rec.exitEvent == EventAcquireFailure {
		snap := rec.Slot
		acquisitionFailure = &snap
		rec.exitEvent = ""
	}
	var completion *Completion
	if rec.claimed && !rec.completionSent && t.opts.completionHook != nil {
		rec.completionSent = true
		c := Completion{ID: id, RunnerName: rec.RunnerName, WorkflowRef: rec.WorkflowRef, RunID: rec.RunID, Result: rec.result,
			CPUSeconds: max(0, rec.lastUsage.CPUSeconds-rec.usageAtClaim.CPUSeconds), PeakMemBytes: rec.lastUsage.PeakMemBytes,
			QueueWaitSeconds: rec.queueWait, Sampled: rec.sampledOK}
		if !rec.claimAt.IsZero() {
			c.WallSeconds = time.Since(rec.claimAt).Seconds()
		}
		completion = &c
	}
	t.mu.Unlock()
	if acquisitionFailure != nil {
		t.emit(EventAcquireFailure, *acquisitionFailure)
	}
	t.log.Info("slot exited", "slot", id, "prev_state", prev, "err", err)
	if completion != nil {
		t.opts.completionHook(*completion)
	}
	t.clean(ctx, rec)
}

// clean bounds caller latency without abandoning ownership of the path.
// A timed-out worker continues to reserve its ID until all filesystem
// operations have actually ended. Failures remain reserved for a later tick.
func (t *Table) clean(parent context.Context, rec *slotRec) {
	t.mu.Lock()
	if t.slots[rec.ID] != rec || rec.cleaning {
		t.mu.Unlock()
		return
	}
	rec.cleaning = true
	if rec.cleanDone == nil {
		rec.cleanDone = make(chan struct{})
	} else {
		select {
		case <-rec.cleanDone:
			rec.cleanDone = make(chan struct{})
		default:
		}
	}
	rec.cleanupPending = true
	snap := rec.Slot
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, t.opts.cleanupTimeout)
	done := rec.cleanDone
	go func() {
		defer func() { t.mu.Lock(); close(done); rec.cleaning = false; t.mu.Unlock() }()
		defer cancel()
		err := t.wipe(ctx, snap)
		t.mu.Lock()
		event := rec.exitEvent
		if err == nil && t.slots[rec.ID] == rec {
			delete(t.slots, rec.ID)
		}
		t.mu.Unlock()
		if err != nil {
			t.log.Warn("slot cleanup retained for retry", "slot", snap.ID, "err", err)
			return
		}
		if event != "" {
			t.emit(event, snap)
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
			return
		default:
		}
		t.log.Warn("slot cleanup deadline reached; retaining ID", "slot", snap.ID)
	}
}

func (t *Table) waitCleanup(ctx context.Context, rec *slotRec) {
	t.mu.Lock()
	done := rec.cleanDone
	t.mu.Unlock()
	if done == nil {
		return
	}
	waitCtx, cancel := context.WithTimeout(ctx, t.opts.cleanupTimeout)
	defer cancel()
	select {
	case <-done:
	case <-waitCtx.Done():
	}
}

func (t *Table) retryCleanup(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	t.mu.Lock()
	var pending []*slotRec
	for _, rec := range t.slots {
		if rec.cleanupPending && !rec.cleaning {
			pending = append(pending, rec)
		}
	}
	t.mu.Unlock()
	for _, rec := range pending {
		if ctx.Err() != nil {
			return
		}
		t.clean(ctx, rec)
	}
}

// quickNeverBusyExit reports whether an exit counts as an acquire
// failure: the process exited before any JobStarted and
// before the acquire grace elapsed. Adopted slots that were running a
// job count as busy (boot adoption is conservative).
func (t *Table) quickNeverBusyExit(rec *slotRec, prev State) bool {
	if t.opts.acquireGrace <= 0 {
		return false
	}
	if prev == StateBusy || !rec.busySince.IsZero() {
		return false
	}
	start := rec.StartedAt
	if start.IsZero() {
		start = rec.provStart
	}
	return time.Since(start) < t.opts.acquireGrace
}

// wipe runs the post-exit teardown for a slot: diag hook, JIT removal,
// and directory removal, bounded by the cleanup timeout.
func (t *Table) wipe(ctx context.Context, snap Slot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var diagErr error
	if t.opts.diagContext != nil {
		diagErr = t.opts.diagContext(ctx, snap)
	} else if t.opts.diagHook != nil {
		diagErr = t.opts.diagHook(snap)
	}
	if diagErr != nil {
		t.log.Warn("slot diag hook failed", "slot", snap.ID, "err", diagErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cleanup.ShredCredentials(snap.Dir); err != nil {
		t.log.Warn("credential unlink failed; removing complete slot tree", "slot", snap.ID, "err", err)
	}
	if t.jitDir != "" {
		if err := os.Remove(filepath.Join(t.jitDir, string(snap.ID)+".jit")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// RemoveAll does not follow symlinks. Even if a filesystem syscall
	// stalls past the deadline, clean keeps this slot reserved until return.
	return os.RemoveAll(snap.Dir)
}

// Adopt reconciles the table with pre-existing state at boot, given the
// backend's currently-active unit names. A slot directory whose unit is
// running is adopted as busy (conservative); a directory with no running
// unit is wiped; a running unit with no directory is stopped. Already
// tracked slots are left untouched.
func (t *Table) Adopt(ctx context.Context, units []string) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil
	}

	running := make(map[string]bool, len(units))
	for _, u := range units {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		running[u] = true
	}

	entries, err := os.ReadDir(t.root)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("adopt: list slot dirs: %w", err)
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !e.IsDir() {
			if validID(ID(e.Name())) {
				return fmt.Errorf("adopt: slot %s is not a directory", e.Name())
			}
			continue
		}
		id := ID(e.Name())
		if !validID(id) {
			t.log.Warn("ignoring non-slot directory", "dir", e.Name())
			continue
		}
		dir := filepath.Join(t.root, string(id))
		t.mu.Lock()
		_, tracked := t.slots[id]
		t.mu.Unlock()
		if tracked {
			continue
		}
		if running[unitName(t.opts.namespace, id)] {
			rec := &slotRec{Slot: Slot{
				Pool:       t.opts.namespace,
				ID:         id,
				Dir:        dir,
				Unit:       unitName(t.opts.namespace, id),
				RunnerName: readRunnerName(dir),
				State:      StateBusy,
				StartedAt:  time.Now(),
			}}
			t.mu.Lock()
			t.slots[id] = rec
			t.mu.Unlock()
			t.log.Info("adopted running slot", "slot", id, "unit", rec.Unit)
			go t.watch(rec)
			continue
		}
		t.log.Warn("wiping orphan slot directory", "slot", id)
		rec := &slotRec{Slot: Slot{Pool: t.opts.namespace, ID: id, Dir: dir, Unit: unitName(t.opts.namespace, id), RunnerName: readRunnerName(dir), State: StateStopping}}
		t.mu.Lock()
		t.slots[id] = rec
		t.mu.Unlock()
		t.clean(ctx, rec)
	}

	for _, u := range units {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id, ok := idFromUnit(t.opts.namespace, u)
		if !ok {
			continue
		}
		t.mu.Lock()
		_, tracked := t.slots[id]
		t.mu.Unlock()
		if tracked {
			continue
		}
		dir := filepath.Join(t.root, string(id))
		info, err := os.Lstat(dir)
		if err == nil && !info.IsDir() {
			return fmt.Errorf("adopt: unit %s has invalid slot directory", u)
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("adopt: inspect slot for %s: %w", u, err)
		}
		if os.IsNotExist(err) {
			t.log.Warn("stopping unit without slot directory", "unit", u)
			if err := t.backend.Stop(ctx, u); err != nil {
				return fmt.Errorf("adopt: stop unit %s without slot directory: %w", u, err)
			}
		}
	}
	return ctx.Err()
}

// Close cancels the watcher goroutines. Running units and slot
// directories are left in place so boot adoption can recover them on the
// next start.
func (t *Table) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.cancel()
	t.mu.Unlock()
}

func (t *Table) emit(event Event, s Slot) {
	if t.opts.eventHook != nil {
		t.opts.eventHook(event, s)
	}
}

// unitName maps a pool namespace and slot ID to a backend unit name.
func unitName(namespace string, id ID) string {
	return "tentacle-" + namespace + "-" + string(id) + ".service"
}

// idFromUnit extracts a slot ID from this table's namespaced unit name.
func idFromUnit(namespace, unit string) (ID, bool) {
	prefix := "tentacle-" + namespace + "-"
	const suffix = ".service"
	if !strings.HasPrefix(unit, prefix) || !strings.HasSuffix(unit, suffix) {
		return "", false
	}
	id := ID(strings.TrimSuffix(strings.TrimPrefix(unit, prefix), suffix))
	if !validID(id) {
		return "", false
	}
	return id, true
}

// validID reports whether s is a zero-padded slot ID in "0001" … "9999".
func validID(id ID) bool {
	s := string(id)
	if len(s) != 4 {
		return false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1 && n <= maxSlotID
}

// readRunnerName extracts the runner name the agent persisted in the
// slot's .runner file at registration, so an adopted in-flight job can
// still be correlated with JobStarted messages. Any parse failure
// yields "" (the pre-adoption behavior).
func readRunnerName(dir string) string {
	f, err := os.OpenFile(filepath.Join(dir, ".runner"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return ""
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return ""
	}
	var cfg struct {
		AgentName string `json:"agentName"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cfg.AgentName
}
