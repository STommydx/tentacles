package slot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/runner"
)

// fakeBackend is a runner.Backend for tests. Start registers a per-unit
// exit channel; Stop (or exit) closes it so a blocked Wait returns and
// the table's watcher runs cleanup.
type fakeBackend struct {
	mu       sync.Mutex
	started  []runner.Spec
	stopped  []string
	startErr error
	stopErr  error
	exitChs  map[string]chan struct{}
	active   []string
}

func (f *fakeBackend) Start(_ context.Context, spec runner.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		if f.stopErr == nil {
			return fmt.Errorf("%w: %w", runner.ErrNotStarted, f.startErr)
		}
		return f.startErr
	}
	f.started = append(f.started, spec)
	if f.exitChs == nil {
		f.exitChs = map[string]chan struct{}{}
	}
	f.exitChs[spec.UnitName] = make(chan struct{})
	return nil
}

func (f *fakeBackend) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, unit)
	if f.stopErr != nil {
		return f.stopErr
	}
	f.signalExitLocked(unit)
	return nil
}

func (f *fakeBackend) Wait(ctx context.Context, unit string) error {
	f.mu.Lock()
	if f.exitChs == nil {
		f.exitChs = map[string]chan struct{}{}
	}
	ch, ok := f.exitChs[unit]
	if !ok {
		// Unit adopted from a previous boot: no Start recorded, wait for
		// an external exit signal.
		ch = make(chan struct{})
		f.exitChs[unit] = ch
	}
	f.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeBackend) Active(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.active...), nil
}

// exit simulates the runner process exiting on its own.
func (f *fakeBackend) exit(unit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signalExitLocked(unit)
}

func (f *fakeBackend) signalExitLocked(unit string) {
	if ch, ok := f.exitChs[unit]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

// materializer wraps the injected materialize function so tests can
// toggle failures and count calls.
type materializer struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (m *materializer) fn(dst string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return m.err
	}
	return markerMaterialize(dst)
}

func (m *materializer) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *materializer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// markerMaterialize fakes the payload-template copy: it creates the slot
// directory with a run.sh marker.
func markerMaterialize(dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
}

// fakeJIT mints a deterministic JIT using the runner-name convention
// "debian-host-<id>-<rand>".
func fakeJIT(_ context.Context, id ID) (runner.JIT, error) {
	return runner.JIT{
		Encoded:    "jit:" + string(id),
		RunnerName: "debian-host-" + string(id) + "-ab12",
	}, nil
}

type eventRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *eventRecorder) hook(event Event, _ Slot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *eventRecorder) has(event Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == event {
			return true
		}
	}
	return false
}

// newTestTable builds a Table on temp dirs with a recording event hook.
// The returned jitDir is created and populated with the slot JIT files.
func newTestTable(t *testing.T, backend *fakeBackend, opts ...TableOption) (*Table, *eventRecorder, string) {
	t.Helper()
	root := t.TempDir()
	jitDir := filepath.Join(t.TempDir(), "jit")
	if err := os.MkdirAll(jitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := &eventRecorder{}
	all := append([]TableOption{WithEventHook(rec.hook)}, opts...)
	tab := NewTable(root, backend, markerMaterialize, fakeJIT, jitDir, nil, all...)
	t.Cleanup(tab.Close)
	return tab, rec, jitDir
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func slotIDs(slots []Slot) []ID {
	ids := make([]ID, len(slots))
	for i, s := range slots {
		ids[i] = s.ID
	}
	return ids
}

func stateOf(tab *Table, id ID) (State, bool) {
	for _, s := range tab.Snapshot() {
		if s.ID == id {
			return s.State, true
		}
	}
	return "", false
}

func TestTableStartN(t *testing.T) {
	backend := &fakeBackend{}
	tab, rec, jitDir := newTestTable(t, backend)

	if err := tab.Ensure(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	got := slotIDs(tab.Active())
	want := []ID{"0001", "0002", "0003"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("active IDs = %v, want %v", got, want)
	}
	backend.mu.Lock()
	nStarted := len(backend.started)
	specs := append([]runner.Spec(nil), backend.started...)
	backend.mu.Unlock()
	if nStarted != 3 {
		t.Fatalf("started %d slots, want 3", nStarted)
	}
	for i, spec := range specs {
		id := ID(fmt.Sprintf("%04d", i+1))
		if spec.UnitName != "tentacle-default-"+string(id)+".service" {
			t.Errorf("spec %d unit = %q", i, spec.UnitName)
		}
		if spec.SlotDir != filepath.Join(tab.root, string(id)) {
			t.Errorf("spec %d slot dir = %q", i, spec.SlotDir)
		}
		if wd := tab.WorkDir(id); wd != filepath.Join(spec.SlotDir, "_work") {
			t.Errorf("spec %d work dir = %q, want %q", i, wd, filepath.Join(spec.SlotDir, "_work"))
		}
		if spec.JITPath != filepath.Join(jitDir, string(id)+".jit") {
			t.Errorf("spec %d jit path = %q", i, spec.JITPath)
		}
		// JIT file exists, 0600.
		fi, err := os.Stat(spec.JITPath)
		if err != nil {
			t.Fatalf("stat jit file %s: %v", spec.JITPath, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("jit file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	if !rec.has("started") {
		t.Errorf("expected a started event, got %v", rec.events)
	}
}

func TestTableIDAllocationReuseAfterWipe(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend)
	ctx := context.Background()

	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0001"}) {
		t.Fatalf("active = %v, want [0001]", got)
	}

	backend.exit("tentacle-default-0001.service")
	// The ID is freed only after the wipe completes, so wait for the dir
	// to be gone before allocating again.
	eventually(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(tab.root, "0001"))
		return os.IsNotExist(err)
	})

	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0001"}) {
		t.Fatalf("active after reuse = %v, want [0001]", got)
	}
}

func TestTableEnsureIdempotent(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend)
	ctx := context.Background()

	if err := tab.Ensure(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := tab.Ensure(ctx, 2); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	nStarted := len(backend.started)
	backend.mu.Unlock()
	if nStarted != 2 {
		t.Fatalf("started %d slots after two Ensure(2) calls, want 2", nStarted)
	}
	if got := slotIDs(tab.Active()); len(got) != 2 {
		t.Fatalf("active = %v, want 2 slots", got)
	}
}

func TestTableStopOnlyIdleNeverBusy(t *testing.T) {
	backend := &fakeBackend{}
	tab, rec, jitDir := newTestTable(t, backend, WithIdleGrace(0))
	ctx := context.Background()

	if err := tab.Ensure(ctx, 2); err != nil {
		t.Fatal(err)
	}
	tab.MarkBusy("0001")

	if err := tab.Ensure(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// Active() excludes the stopping slot, so wait for the wipe itself.
	eventually(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(tab.root, "0002"))
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(filepath.Join(jitDir, "0002.jit")); !os.IsNotExist(err) {
		t.Errorf("stopped slot jit still present: %v", err)
	}
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0001"}) {
		t.Fatalf("active = %v, want [0001]", got)
	}

	backend.mu.Lock()
	stopped := append([]string(nil), backend.stopped...)
	backend.mu.Unlock()
	if fmt.Sprint(stopped) != fmt.Sprint([]string{"tentacle-default-0002.service"}) {
		t.Fatalf("stopped = %v, want only [tentacle-0002.service]", stopped)
	}
	if !rec.has("stopped") {
		t.Errorf("expected a stopped event, got %v", rec.events)
	}

	// Busy slot untouched: its dir and JIT survive.
	if _, err := os.Stat(filepath.Join(tab.root, "0001")); err != nil {
		t.Errorf("busy slot dir removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jitDir, "0001.jit")); err != nil {
		t.Errorf("busy slot jit removed: %v", err)
	}
	if st, ok := stateOf(tab, "0001"); !ok || st != StateBusy {
		t.Errorf("slot 0001 state = %q, ok=%v; want busy", st, ok)
	}
}

func TestTableStopOldestIdlePreferred(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend, WithIdleGrace(0))
	ctx := context.Background()

	if err := tab.Ensure(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool { return len(tab.Active()) == 1 })

	backend.mu.Lock()
	stopped := append([]string(nil), backend.stopped...)
	backend.mu.Unlock()
	if fmt.Sprint(stopped) != fmt.Sprint([]string{"tentacle-default-0001.service"}) {
		t.Fatalf("stopped = %v, want oldest idle [tentacle-0001.service]", stopped)
	}
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0002"}) {
		t.Fatalf("remaining active = %v, want [0002]", got)
	}
}

func TestTableStopSurplusBusyOnlyDoesNothing(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend)
	ctx := context.Background()

	if err := tab.Ensure(ctx, 2); err != nil {
		t.Fatal(err)
	}
	tab.MarkBusy("0001")
	tab.MarkBusy("0002")

	if err := tab.Ensure(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// Give any (wrong) stop a chance to land.
	time.Sleep(50 * time.Millisecond)
	backend.mu.Lock()
	nStopped := len(backend.stopped)
	backend.mu.Unlock()
	if nStopped != 0 {
		t.Fatalf("stopped %d busy slots, want 0", nStopped)
	}
	if len(tab.Active()) != 2 {
		t.Fatalf("active = %v, want both busy slots", slotIDs(tab.Active()))
	}
}

// TestTableFreshIdleSlotSafeFromSurplusStop reproduces issue #4: GitHub
// assigns a job to an idle runner out of band, and the JobStarted message
// that would mark the slot busy arrives only later. A slot that became
// idle within the idle grace must not be picked as surplus, so the late
// claim still lands on a live runner. Once the grace passes, an
// unclaimed idle slot is stopped as surplus again.
func TestTableFreshIdleSlotSafeFromSurplusStop(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend, WithIdleGrace(150*time.Millisecond))
	ctx := context.Background()

	if err := tab.Ensure(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// Surplus decisions within the grace spare both freshly idle slots.
	if err := tab.Ensure(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// The late JobStarted from GitHub's out-of-band assignment.
	if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12", WorkflowRef: "o/r/w.yml@main"}) {
		t.Fatal("Claim failed for freshly idle slot")
	}
	if err := tab.Ensure(ctx, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let any wrong stop land
	backend.mu.Lock()
	stopped := append([]string(nil), backend.stopped...)
	backend.mu.Unlock()
	if len(stopped) != 0 {
		t.Fatalf("surplus decision stopped a slot within the idle grace: %v", stopped)
	}
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0001", "0002"}) {
		t.Fatalf("active = %v, want both slots after in-grace surplus decisions", got)
	}
	if st, ok := stateOf(tab, "0001"); !ok || st != StateBusy {
		t.Fatalf("claimed slot state = %q, ok=%v; want busy", st, ok)
	}

	// Past the grace the unclaimed idle slot is stopped; the busy one is not.
	time.Sleep(150 * time.Millisecond)
	if err := tab.Ensure(ctx, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(tab.root, "0002"))
		return os.IsNotExist(err)
	})
	backend.mu.Lock()
	stopped = append([]string(nil), backend.stopped...)
	backend.mu.Unlock()
	if fmt.Sprint(stopped) != fmt.Sprint([]string{"tentacle-default-0002.service"}) {
		t.Fatalf("stopped = %v, want only the grace-expired idle slot", stopped)
	}
	if st, ok := stateOf(tab, "0001"); !ok || st != StateBusy {
		t.Fatalf("claimed slot state = %q, ok=%v; want busy after scale-down", st, ok)
	}
}

// TestTableShutdownStopsFreshIdleSlot: daemon shutdown bypasses the idle
// grace — no further assignment can be honored, so a freshly idle slot
// is stopped immediately.
func TestTableShutdownStopsFreshIdleSlot(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend, WithIdleGrace(time.Hour))
	ctx := context.Background()

	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := tab.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(tab.root, "0001"))
		return os.IsNotExist(err)
	})
	backend.mu.Lock()
	stopped := append([]string(nil), backend.stopped...)
	backend.mu.Unlock()
	if fmt.Sprint(stopped) != fmt.Sprint([]string{"tentacle-default-0001.service"}) {
		t.Fatalf("stopped = %v, want shutdown to bypass the idle grace", stopped)
	}
}

func TestTableObserveExitWipesEvenWhenDiagHookFails(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, jitDir := newTestTable(t, backend, WithDiagHook(func(Slot) error {
		return errors.New("diag boom")
	}))
	ctx := context.Background()

	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tab.root, "0001")
	jitPath := filepath.Join(jitDir, "0001.jit")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("slot dir missing before exit: %v", err)
	}
	if _, err := os.Stat(jitPath); err != nil {
		t.Fatalf("jit file missing before exit: %v", err)
	}

	backend.exit("tentacle-default-0001.service")
	// Wait for the wipe itself, not just the state flip.
	eventually(t, 3*time.Second, func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("slot dir not wiped after exit: %v", err)
	}
	if _, err := os.Stat(jitPath); !os.IsNotExist(err) {
		t.Errorf("jit file not wiped after exit: %v", err)
	}
}

// TestTableWarmIdleSlotSurvivesAcquireGrace: an idle slot is the warm
// pool when no jobs are running.
// Idleness is never an acquire failure; only a start failure or a quick
// never-busy exit is.
func TestTableWarmIdleSlotSurvivesAcquireGrace(t *testing.T) {
	backend := &fakeBackend{}
	tab, rec, _ := newTestTable(t, backend, WithAcquireGrace(30*time.Millisecond))

	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0001"}) {
		t.Fatalf("warm idle slot torn down after grace: active=%v events=%v", got, rec.events)
	}
	backend.mu.Lock()
	nStopped := len(backend.stopped)
	backend.mu.Unlock()
	if nStopped != 0 {
		t.Fatalf("idle warm slot stopped: %v", backend.stopped)
	}
	if rec.has(EventAcquireFailure) {
		t.Fatalf("idle warm slot classified acquire failure: %v", rec.events)
	}
}

func TestTableGraceDoesNotKillBusySlot(t *testing.T) {
	backend := &fakeBackend{}
	tab, rec, _ := newTestTable(t, backend, WithAcquireGrace(30*time.Millisecond))
	ctx := context.Background()

	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	tab.MarkBusy("0001")
	time.Sleep(100 * time.Millisecond)
	if rec.has("acquire_failure") {
		t.Fatalf("busy slot reported acquire failure: %v", rec.events)
	}
	if len(tab.Active()) != 1 {
		t.Fatalf("busy slot was torn down: %v", slotIDs(tab.Active()))
	}
}

// TestTableClaimStoresRef: a claimed slot (JobStarted) records the
// workflow identity so usage at exit can be attributed, and reports
// false for runners we do not track.
func TestTableClaimStoresRef(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend)
	ctx := context.Background()

	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if !tab.Claim(ClaimJob{
		RunnerName:       "debian-host-0001-ab12",
		WorkflowRef:      "o/r/.github/workflows/ci.yml@main",
		RunID:            42,
		QueueWaitSeconds: 3.5,
	}) {
		t.Fatal("Claim did not find the slot")
	}
	if tab.Claim(ClaimJob{RunnerName: "debian-host-9999-zz99"}) {
		t.Fatal("Claim matched a nonexistent runner")
	}
	snap := tab.Snapshot()
	if len(snap) != 1 || snap[0].WorkflowRef != "o/r/.github/workflows/ci.yml@main" || snap[0].RunID != 42 {
		t.Fatalf("slot after claim = %+v", snap)
	}
	if st, ok := stateOf(tab, "0001"); !ok || st != StateBusy {
		t.Fatalf("slot 0001 state = %q ok=%v; want busy", st, ok)
	}
}

// TestTableCompletionRecord: at exit, a claimed slot reports a
// Completion with the workflow identity, wall time, and queue wait.
func TestTableCompletionRecord(t *testing.T) {
	backend := &fakeBackend{}
	var mu sync.Mutex
	var done []Completion
	tab, _, _ := newTestTable(t, backend, WithCompletionHook(func(c Completion) {
		mu.Lock()
		done = append(done, c)
		mu.Unlock()
	}), WithSampleInterval(5*time.Millisecond))
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if !tab.Claim(ClaimJob{
		RunnerName:       "debian-host-0001-ab12",
		WorkflowRef:      "o/r/w.yml@main",
		RunID:            7,
		QueueWaitSeconds: 2,
	}) {
		t.Fatal("Claim failed")
	}
	time.Sleep(30 * time.Millisecond)
	backend.exit("tentacle-default-0001.service")
	eventually(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(done) == 1
	})
	mu.Lock()
	c := done[0]
	if c.ID != "0001" || c.WorkflowRef != "o/r/w.yml@main" || c.RunID != 7 {
		t.Fatalf("completion = %+v", c)
	}
	if c.WallSeconds <= 0 || c.QueueWaitSeconds != 2 {
		t.Fatalf("completion timings = %+v", c)
	}
	mu.Unlock()

	// An unclaimed slot produces no completion record: acquire-failure
	// accounting already covers it. The retried start reuses ID 0001.
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := len(done)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("completions = %d, want still 1", n)
	}
	backend.exit("tentacle-default-0001.service")
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	n = len(done)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("unclaimed exit produced a completion record (%d)", n)
	}
}

// TestTableUsageDelta: the CPU seconds on a completion record are the
// delta between exit and claim (the agent's registration CPU is not the
// job's), sampled by the injected sampler.
func TestTableUsageDelta(t *testing.T) {
	backend := &fakeBackend{}
	cpu := 10.0 // the agent already burned 10s registering (fake)
	var mu sync.Mutex
	sampler := func(unit string) (Usage, error) {
		mu.Lock()
		defer mu.Unlock()
		return Usage{CPUSeconds: cpu, PeakMemBytes: 1 << 28}, nil
	}
	var done []Completion
	var dmu sync.Mutex
	tab, _, _ := newTestTable(t, backend,
		WithUsageSampler(sampler),
		WithSampleInterval(5*time.Millisecond),
		WithCompletionHook(func(c Completion) {
			dmu.Lock()
			done = append(done, c)
			dmu.Unlock()
		}))
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// Let the sampler establish the pre-claim reading (the fake agent's
	// 10s of registration CPU).
	time.Sleep(20 * time.Millisecond)
	if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12", WorkflowRef: "o/r/w.yml@main"}) {
		t.Fatal("Claim failed")
	}
	mu.Lock()
	cpu += 25 // the job burns 25s
	mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	backend.exit("tentacle-default-0001.service")
	eventually(t, 3*time.Second, func() bool {
		dmu.Lock()
		defer dmu.Unlock()
		return len(done) == 1
	})
	dmu.Lock()
	c := done[0]
	dmu.Unlock()
	if !c.Sampled {
		t.Fatalf("completion not sampled: %+v", c)
	}
	if c.CPUSeconds < 20 || c.CPUSeconds > 26 {
		t.Fatalf("cpu seconds = %v, want ~25 (exit minus claim)", c.CPUSeconds)
	}
	if c.PeakMemBytes != 1<<28 {
		t.Fatalf("peak mem = %v", c.PeakMemBytes)
	}
}

// TestTableGateHolds: when the gate refuses, no slot is started, the
// refusal is not an acquire failure, and a later reconcile retries.
func TestTableGateHolds(t *testing.T) {
	backend := &fakeBackend{}
	admit := false
	rec := &eventRecorder{}
	tab, _, _ := newTestTable(t, backend, WithGate(func([]Slot) bool {
		return admit
	}), WithEventHook(rec.hook))
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(tab.Active()) != 0 {
		t.Fatalf("gate refused but a slot started: %v", slotIDs(tab.Active()))
	}
	if rec.has(EventAcquireFailure) {
		t.Fatalf("admission hold reported as acquire failure: %v", rec.events)
	}
	if !rec.has(EventAdmissionHold) {
		t.Fatalf("expected admission_hold event, got %v", rec.events)
	}

	// Budget frees up: the next reconcile starts the slot.
	admit = true
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0001"}) {
		t.Fatalf("active after gate opens = %v, want [0001]", got)
	}
}

func TestTableStartMaterializeError(t *testing.T) {
	backend := &fakeBackend{}
	mat := &materializer{}
	root := t.TempDir()
	jitDir := filepath.Join(t.TempDir(), "jit")
	if err := os.MkdirAll(jitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := &eventRecorder{}
	tab := NewTable(root, backend, mat.fn, fakeJIT, jitDir, nil, WithEventHook(rec.hook))
	t.Cleanup(tab.Close)
	ctx := context.Background()

	mat.setErr(errors.New("no disk"))
	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(tab.Active()) != 0 {
		t.Fatalf("active = %v, want none after materialize failure", slotIDs(tab.Active()))
	}
	if !rec.has("acquire_failure") {
		t.Fatalf("expected acquire_failure event, got %v", rec.events)
	}

	// ID is freed: a successful retry reuses 0001.
	mat.setErr(nil)
	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0001"}) {
		t.Fatalf("active after retry = %v, want [0001]", got)
	}
}

func TestTableBackendStartError(t *testing.T) {
	backend := &fakeBackend{startErr: errors.New("systemd-run boom")}
	tab, rec, _ := newTestTable(t, backend)
	ctx := context.Background()

	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(tab.Active()) != 0 {
		t.Fatalf("active = %v, want none after backend start failure", slotIDs(tab.Active()))
	}
	if !rec.has("acquire_failure") {
		t.Fatalf("expected acquire_failure event, got %v", rec.events)
	}
}

func TestTableSetDesired(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend)
	if tab.Desired() != 0 {
		t.Fatalf("initial desired = %d, want 0", tab.Desired())
	}
	tab.SetDesired(4)
	if tab.Desired() != 4 {
		t.Fatalf("desired = %d, want 4", tab.Desired())
	}
}

func TestTableAdopt(t *testing.T) {
	backend := &fakeBackend{}
	root := t.TempDir()
	jitDir := filepath.Join(t.TempDir(), "jit")
	if err := os.MkdirAll(jitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tab := NewTable(root, backend, markerMaterialize, fakeJIT, jitDir, nil)
	t.Cleanup(tab.Close)
	ctx := context.Background()

	// 0001: running unit + matching dir → adopt busy.
	if err := markerMaterialize(filepath.Join(root, "0001")); err != nil {
		t.Fatal(err)
	}
	// 0002: dir with no running unit → wipe (leave a stale JIT file).
	if err := markerMaterialize(filepath.Join(root, "0002")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jitDir, "0002.jit"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 0003: running unit with no dir → stop.

	backend.active = []string{"tentacle-default-0001.service", "tentacle-default-0003.service"}
	if err := tab.Adopt(ctx, backend.active); err != nil {
		t.Fatal(err)
	}

	got := tab.Active()
	if len(got) != 1 || got[0].ID != "0001" || got[0].State != StateBusy {
		t.Fatalf("active after adopt = %+v, want [0001 busy]", got)
	}
	if _, err := os.Stat(filepath.Join(root, "0002")); !os.IsNotExist(err) {
		t.Errorf("orphan dir 0002 not wiped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jitDir, "0002.jit")); !os.IsNotExist(err) {
		t.Errorf("orphan jit file not wiped: %v", err)
	}
	backend.mu.Lock()
	stopped := append([]string(nil), backend.stopped...)
	backend.mu.Unlock()
	if fmt.Sprint(stopped) != fmt.Sprint([]string{"tentacle-default-0003.service"}) {
		t.Fatalf("stopped = %v, want unitless [tentacle-0003.service]", stopped)
	}
}

func TestTableAdoptIgnoresNonSlotDirs(t *testing.T) {
	backend := &fakeBackend{}
	root := t.TempDir()
	tab := NewTable(root, backend, markerMaterialize, fakeJIT, t.TempDir(), nil)
	t.Cleanup(tab.Close)

	if err := os.MkdirAll(filepath.Join(root, "template"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tab.Adopt(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "template")); err != nil {
		t.Fatalf("non-slot dir was touched: %v", err)
	}
}

func TestCloseLeavesRunningSlotsAlone(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, jitDir := newTestTable(t, backend)
	ctx := context.Background()

	if err := tab.Ensure(ctx, 1); err != nil {
		t.Fatal(err)
	}
	tab.Close()
	// After Close, watchers stop; nothing is stopped or wiped.
	time.Sleep(50 * time.Millisecond)
	backend.mu.Lock()
	nStopped := len(backend.stopped)
	backend.mu.Unlock()
	if nStopped != 0 {
		t.Fatalf("Close stopped units: %v", backend.stopped)
	}
	if _, err := os.Stat(filepath.Join(tab.root, "0001")); err != nil {
		t.Errorf("Close wiped slot dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jitDir, "0001.jit")); err != nil {
		t.Errorf("Close wiped jit file: %v", err)
	}
}

// TestTableStartFailureLeavesNoPartialDir: a start that fails after
// partially materializing must remove the slot directory, otherwise the
// ID is wedged (CopySlot refuses an existing destination) until reboot.
func TestTableStartFailureLeavesNoPartialDir(t *testing.T) {
	backend := &fakeBackend{}
	root := t.TempDir()
	jitDir := filepath.Join(t.TempDir(), "jit")
	if err := os.MkdirAll(jitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := &eventRecorder{}
	var calls int
	partial := func(dst string) error {
		calls++
		if calls == 1 {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dst, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
				return err
			}
			return errors.New("simulated partial copy")
		}
		return markerMaterialize(dst)
	}
	tab := NewTable(root, backend, partial, fakeJIT, jitDir, nil, WithEventHook(rec.hook))
	t.Cleanup(tab.Close)

	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "0001")); !os.IsNotExist(err) {
		t.Fatalf("partial slot dir left behind after failed start: %v", err)
	}
	if !rec.has(EventAcquireFailure) {
		t.Fatalf("expected acquire_failure event, got %v", rec.events)
	}

	// The ID is not wedged: a retry starts cleanly on the same ID.
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := slotIDs(tab.Active()); fmt.Sprint(got) != fmt.Sprint([]ID{"0001"}) {
		t.Fatalf("active after retry = %v, want [0001]", got)
	}
}

// TestTableExitBeforeGraceCountsAcquireFailure checks that an exit before
// JobStarted and acquire_grace schedules cleanup and counts an acquire failure.
func TestTableExitBeforeGraceCountsAcquireFailure(t *testing.T) {
	backend := &fakeBackend{}
	tab, rec, _ := newTestTable(t, backend, WithAcquireGrace(time.Hour))

	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	backend.exit("tentacle-default-0001.service")
	eventually(t, 3*time.Second, func() bool {
		return len(tab.Active()) == 0 && rec.has(EventAcquireFailure)
	})
	if rec.has(EventExited) {
		t.Errorf("never-busy quick exit reported as plain exit: %v", rec.events)
	}
}

// TestTableBusyExitIsPlainExit: a runner that exits after claiming a job
// (the normal one-job JIT lifecycle) is a plain exit, not an acquire
// failure.
func TestTableBusyExitIsPlainExit(t *testing.T) {
	backend := &fakeBackend{}
	tab, rec, _ := newTestTable(t, backend, WithAcquireGrace(time.Hour))

	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	tab.MarkBusy("0001")
	backend.exit("tentacle-default-0001.service")
	eventually(t, 10*time.Second, func() bool {
		return len(tab.Active()) == 0 && rec.has(EventExited)
	})
	if rec.has(EventAcquireFailure) {
		t.Fatalf("busy exit classified acquire failure: %v", rec.events)
	}
}

// TestTableStartObserverOncePerStart: the start observer fires exactly
// once per successful slot start, with the total provision time.
func TestTableStartObserverOncePerStart(t *testing.T) {
	backend := &fakeBackend{}
	var mu sync.Mutex
	var count int
	var total time.Duration
	tab, _, _ := newTestTable(t, backend, WithStartObserver(func(d time.Duration) {
		mu.Lock()
		count++
		total += d
		mu.Unlock()
	}))
	if err := tab.Ensure(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 2 {
		t.Fatalf("start observer called %d times for 2 slots, want 2", count)
	}
	if total <= 0 {
		t.Fatalf("start observer durations not positive: %v", total)
	}
}

// TestTableWipeRemovesCredentialFiles: wipe completes when the runner
// dropped .runner/.credentials files into the slot tree (they are
// shredded first; see cleanup.ShredCredentials).
func TestTableWipeRemovesCredentialFiles(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend)
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tab.root, "0001")
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("secret-"+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backend.exit("tentacle-default-0001.service")
	eventually(t, 3*time.Second, func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}

// TestTableWorkDirOption: WorkDir is the single source for the slot work
// directory (the JIT work folder), honoring the configured name.
func TestTableWorkDirOption(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend, WithWorkDir("w"))
	if got, want := tab.WorkDir("0002"), filepath.Join(tab.root, "0002", "w"); got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
	def, _, _ := newTestTable(t, backend)
	if got, want := def.WorkDir("0001"), filepath.Join(def.root, "0001", "_work"); got != want {
		t.Fatalf("default WorkDir = %q, want %q", got, want)
	}
}

// TestTableAdoptReadsRunnerName: an adopted running slot recovers its
// runner name from the .runner file the agent wrote at registration, so
// JobStarted correlation and diag shipping keep working across restarts.
func TestTableAdoptReadsRunnerName(t *testing.T) {
	backend := &fakeBackend{}
	root := t.TempDir()
	dir := filepath.Join(root, "0001")
	if err := markerMaterialize(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".runner"),
		[]byte(`{"agentId":42,"agentName":"debian-host-0001-ab12","poolId":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tab := NewTable(root, backend, markerMaterialize, fakeJIT, t.TempDir(), nil)
	t.Cleanup(tab.Close)

	backend.active = []string{"tentacle-default-0001.service"}
	if err := tab.Adopt(context.Background(), backend.active); err != nil {
		t.Fatal(err)
	}
	got := tab.Active()
	if len(got) != 1 || got[0].RunnerName != "debian-host-0001-ab12" {
		t.Fatalf("adopted slot = %+v, want runner name debian-host-0001-ab12", got)
	}
	if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12", WorkflowRef: "o/r/w.yml@main"}) {
		t.Fatal("adopted runner name does not correlate with JobStarted")
	}
}

// TestStopSurplusStartingPastGraceOnly checks that surplus stops hit idle
// slots and starting slots only once they are past the acquire grace.
func TestStopSurplusStartingPastGraceOnly(t *testing.T) {
	backend := &fakeBackend{}
	tab, _, _ := newTestTable(t, backend, WithAcquireGrace(time.Hour))
	fresh := &slotRec{Slot: Slot{ID: "0001", Unit: "tentacle-default-0001.service", State: StateStarting}, provStart: time.Now()}
	stuck := &slotRec{Slot: Slot{ID: "0002", Unit: "tentacle-default-0002.service", State: StateStarting}, provStart: time.Now().Add(-2 * time.Hour)}
	tab.mu.Lock()
	tab.slots["0001"] = fresh
	tab.slots["0002"] = stuck
	tab.mu.Unlock()

	tab.stopSurplus(0)
	backend.mu.Lock()
	stopped := append([]string(nil), backend.stopped...)
	backend.mu.Unlock()
	if fmt.Sprint(stopped) != fmt.Sprint([]string{"tentacle-default-0002.service"}) {
		t.Fatalf("stopped = %v, want only the past-grace starting slot", stopped)
	}
	if st, ok := stateOf(tab, "0001"); !ok || st != StateStarting {
		t.Fatalf("fresh starting slot = %q ok=%v; want untouched starting", st, ok)
	}
}

// TestTableCompletionUnsampledWhenSamplerFails: a job that exits before
// any successful sample has NO usage data. Recording it as sampled zero
// would drag the CPU/memory averages toward zero — the unsafe direction
// for the admission gate (admits under real memory pressure).
func TestTableCompletionUnsampledWhenSamplerFails(t *testing.T) {
	backend := &fakeBackend{}
	var done []Completion
	var dmu sync.Mutex
	tab, _, _ := newTestTable(t, backend,
		WithUsageSampler(func(string) (Usage, error) {
			return Usage{}, errors.New("unit already gone")
		}),
		WithSampleInterval(5*time.Millisecond),
		WithCompletionHook(func(c Completion) {
			dmu.Lock()
			done = append(done, c)
			dmu.Unlock()
		}))
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12", WorkflowRef: "o/r/w.yml@main"}) {
		t.Fatal("Claim failed")
	}
	time.Sleep(30 * time.Millisecond)
	backend.exit("tentacle-default-0001.service")
	eventually(t, 3*time.Second, func() bool {
		dmu.Lock()
		defer dmu.Unlock()
		return len(done) == 1
	})
	dmu.Lock()
	defer dmu.Unlock()
	if done[0].Sampled {
		t.Fatalf("completion marked sampled without any reading: %+v", done[0])
	}
	if done[0].CPUSeconds != 0 || done[0].PeakMemBytes != 0 {
		t.Fatalf("unsampled completion carries usage: %+v", done[0])
	}
}

// TestTableMarkResultLabelsCompletion: a result recorded from the
// JobCompleted message names the slot's completion; a missed message
// leaves the result empty (the daemon reports "unknown").
func TestTableMarkResultLabelsCompletion(t *testing.T) {
	backend := &fakeBackend{}
	var mu sync.Mutex
	var done []Completion
	tab, _, _ := newTestTable(t, backend, WithCompletionHook(func(c Completion) {
		mu.Lock()
		done = append(done, c)
		mu.Unlock()
	}))
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12", WorkflowRef: "o/r/w.yml@main"}) {
		t.Fatal("Claim failed")
	}
	// A result for a runner we do not track is ignored, not stored.
	tab.MarkResult("debian-host-9999-zz99", "failure")
	tab.MarkResult("debian-host-0001-ab12", "failure")
	backend.exit("tentacle-default-0001.service")
	eventually(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(done) == 1
	})
	mu.Lock()
	if got := done[0].Result; got != "failure" {
		t.Fatalf("completion result = %q, want failure", got)
	}
	mu.Unlock()

	// A second job whose completion message never arrived (listener
	// outage) exits with an empty result instead of a stale label.
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12", WorkflowRef: "o/r/w.yml@main"}) {
		t.Fatal("second Claim failed")
	}
	backend.exit("tentacle-default-0001.service")
	eventually(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(done) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if got := done[1].Result; got != "" {
		t.Fatalf("unlabeled completion result = %q, want empty", got)
	}
}
