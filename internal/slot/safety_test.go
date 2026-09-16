package slot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/runner"
)

type interleavedBackend struct {
	fakeBackend
	onStart   func(runner.Spec)
	onStop    func(context.Context, string) error
	waitCalls chan struct{}
}

func (b *interleavedBackend) Start(ctx context.Context, s runner.Spec) error {
	if err := b.fakeBackend.Start(ctx, s); err != nil {
		return err
	}
	if b.onStart != nil {
		b.onStart(s)
	}
	return nil
}
func (b *interleavedBackend) Stop(ctx context.Context, u string) error {
	if b.onStop != nil {
		return b.onStop(ctx, u)
	}
	return b.fakeBackend.Stop(ctx, u)
}
func (b *interleavedBackend) Wait(ctx context.Context, u string) error {
	if b.waitCalls != nil {
		select {
		case b.waitCalls <- struct{}{}:
		default:
		}
		return errors.New("temporary bus failure")
	}
	return b.fakeBackend.Wait(ctx, u)
}

func TestClaimDuringStartSurvives(t *testing.T) {
	b := &interleavedBackend{}
	tab := NewTable(t.TempDir(), b, markerMaterialize, fakeJIT, t.TempDir(), nil)
	defer tab.Close()
	b.onStart = func(runner.Spec) {
		if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12", WorkflowRef: "ci@main"}) {
			t.Fatal("claim failed")
		}
	}
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := tab.Active()[0].State; got != StateBusy {
		t.Errorf("claimed runner became %s", got)
	}
	if err := tab.Ensure(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.stopped) != 0 {
		t.Errorf("stopped claimed runner: %v", b.stopped)
	}
}

func TestObservationErrorPreservesSlotAndRetries(t *testing.T) {
	b := &interleavedBackend{waitCalls: make(chan struct{}, 10)}
	root := t.TempDir()
	tab := NewTable(root, b, markerMaterialize, fakeJIT, t.TempDir(), nil)
	defer tab.Close()
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-b.waitCalls:
		case <-time.After(2 * time.Second):
			t.Fatal("observation was not retried")
		}
	}
	if _, err := os.Stat(filepath.Join(root, "0001", "run.sh")); err != nil {
		t.Fatalf("live slot removed: %v", err)
	}
	if len(tab.Active()) != 1 {
		t.Fatal("uncertain unit no longer counts toward capacity")
	}
}

func TestAllocationPreservesPreexistingDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "0001")
	if err := markerMaterialize(dir); err != nil {
		t.Fatal(err)
	}
	materialize := func(dst string) error {
		if _, err := os.Stat(dst); err == nil {
			return errors.New("already exists")
		}
		return markerMaterialize(dst)
	}
	tab := NewTable(root, &fakeBackend{}, materialize, fakeJIT, t.TempDir(), nil)
	defer tab.Close()
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil {
		t.Fatalf("existing slot destroyed: %v", err)
	}
}

func TestConcurrentSnapshotsDuringProvision(t *testing.T) {
	tab, _, _ := newTestTable(t, &fakeBackend{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				tab.Snapshot()
			}
		}
	})
	defer func() { close(done); wg.Wait() }()
	if err := tab.Ensure(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupTimeoutRetainsIDAndRunnerIdentity(t *testing.T) {
	entered := make(chan Slot, 1)
	release := make(chan struct{})
	defer close(release)
	tab, _, _ := newTestTable(t, &fakeBackend{}, WithCleanupTimeout(20*time.Millisecond), WithDiagHook(func(s Slot) error { entered <- s; <-release; return nil }))
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { tab.ObserveExit("0001", nil); close(done) }()
	s := <-entered
	if s.RunnerName != "debian-host-0001-ab12" {
		t.Errorf("diagnostic identity lost: %+v", s)
	}
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Error("cleanup exceeded its complete-operation deadline")
	}
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := slotIDs(tab.Active()); len(got) != 1 || got[0] != "0002" {
		t.Errorf("cleanup reused reserved ID: %v", got)
	}
}

func TestUncertainFailedStartRetainsFiles(t *testing.T) {
	b := &fakeBackend{startErr: errors.New("start reply lost"), stopErr: errors.New("stop unavailable")}
	tab, _, _ := newTestTable(t, b)
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tab.root, "0001", "run.sh")); err != nil {
		t.Errorf("uncertain runner files removed: %v", err)
	}
	if len(tab.Active()) != 1 {
		t.Error("uncertain start does not count toward capacity")
	}
}

func TestScaleDownHonorsCallerDeadline(t *testing.T) {
	b := &interleavedBackend{onStop: func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }}
	tab := NewTable(t.TempDir(), b, markerMaterialize, fakeJIT, t.TempDir(), nil, WithStopTimeout(time.Second), WithIdleGrace(0))
	defer tab.Close()
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = tab.Ensure(ctx, 0)
	if time.Since(start) > 200*time.Millisecond {
		t.Error("scale-down ignored caller deadline")
	}
}

func TestAcquireFailureReportedBeforeCleanup(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	events := make(chan Event, 4)
	tab, _, _ := newTestTable(t, &fakeBackend{startErr: errors.New("cannot launch")}, WithCleanupTimeout(20*time.Millisecond), WithDiagHook(func(Slot) error { entered <- struct{}{}; <-release; return nil }), WithEventHook(func(e Event, _ Slot) { events <- e }))
	done := make(chan struct{})
	go func() { _ = tab.Ensure(context.Background(), 1); close(done) }()
	<-entered
	select {
	case e := <-events:
		if e != EventAcquireFailure {
			t.Fatalf("got %s", e)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("acquire failure hidden by unfinished cleanup")
	}
	<-done
}

func TestAcquisitionPauseStopsOngoingEnsure(t *testing.T) {
	b := &interleavedBackend{}
	var pause atomic.Bool
	failed := make(chan struct{})
	starts := 0
	b.onStart = func(runner.Spec) {
		starts++
		if starts == 2 {
			b.exit("tentacle-default-0001.service")
			<-failed
		}
	}
	tab := NewTable(t.TempDir(), b, markerMaterialize, fakeJIT, t.TempDir(), nil, WithStartAllowed(func() bool { return !pause.Load() }), WithEventHook(func(e Event, _ Slot) {
		if e == EventAcquireFailure {
			pause.Store(true)
			close(failed)
		}
	}))
	defer tab.Close()
	if err := tab.Ensure(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if starts != 2 {
		t.Fatalf("kept starting after acquire backoff: %d launches", starts)
	}
}
