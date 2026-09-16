package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/config"
	"github.com/0xinterface/tentacles/internal/metrics"
	"github.com/0xinterface/tentacles/internal/runner"
	"github.com/0xinterface/tentacles/internal/slot"
)

type heldBackend struct {
	mu    sync.Mutex
	units map[string]chan struct{}
}

func newHeldBackend() *heldBackend {
	return &heldBackend{units: make(map[string]chan struct{})}
}

func (b *heldBackend) Start(_ context.Context, spec runner.Spec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.units[spec.UnitName] = make(chan struct{})
	return nil
}

func (b *heldBackend) Stop(_ context.Context, unit string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if done := b.units[unit]; done != nil {
		select {
		case <-done:
		default:
			close(done)
		}
	}
	return nil
}

func (b *heldBackend) Wait(ctx context.Context, unit string) error {
	b.mu.Lock()
	done := b.units[unit]
	b.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *heldBackend) Active(context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	units := make([]string, 0, len(b.units))
	for name, done := range b.units {
		select {
		case <-done:
		default:
			units = append(units, name)
		}
	}
	return units, nil
}

func scheduledPool(
	t *testing.T,
	host *daemon,
	id string,
	minted *int,
	options ...slot.TableOption,
) *poolRuntime {
	t.Helper()
	pool := newPoolRuntime(host, config.Pool{
		ID: id,
		Capacity: config.PoolCapacity{
			MaxRunners: 1,
		},
	})
	pool.desired.Store(1)
	pool.backend = newHeldBackend()
	root := filepath.Join(t.TempDir(), "slots")
	jitDir := filepath.Join(t.TempDir(), "jit")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tableOptions := []slot.TableOption{
		slot.WithNamespace(id),
		slot.WithEventHook(pool.onSlotEvent),
	}
	tableOptions = append(tableOptions, options...)
	pool.table = slot.NewTable(
		root,
		pool.backend,
		func(string) error { return nil },
		func(_ context.Context, slotID slot.ID) (runner.JIT, error) {
			*minted++
			return runner.JIT{Encoded: "jit", RunnerName: id + "-" + string(slotID)}, nil
		},
		jitDir,
		pool.log,
		tableOptions...,
	)
	return pool
}

func TestSchedulerSharesOneHostSlotAcrossPools(t *testing.T) {
	host := &daemon{
		cfg: &config.Config{
			Capacity: config.Capacity{MaxRunners: 1},
			Runtime: config.Runtime{
				SlotStopTimeout: time.Second,
			},
		},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		met:     metrics.NewRegistry(),
		pools:   []*poolRuntime{},
		reconCh: make(chan struct{}, 1),
	}
	mintedA := 0
	mintedB := 0
	poolA := scheduledPool(t, host, "org-a", &mintedA, slot.WithIdleGrace(0))
	poolB := scheduledPool(t, host, "org-b", &mintedB, slot.WithIdleGrace(0))
	host.pools = append(host.pools, poolA, poolB)
	defer host.close()
	defer host.shutdownSlots()

	if err := host.reconcilePools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.allActive()) != 1 || mintedA != 1 || mintedB != 0 {
		t.Fatalf("first allocation: active=%d minted-a=%d minted-b=%d", len(host.allActive()), mintedA, mintedB)
	}
	first := poolA.table.Active()[0]
	if first.Pool != "org-a" || first.Unit != "tentacle-org-a-0001.service" {
		t.Fatalf("first slot not namespaced to org-a: %+v", first)
	}

	poolA.desired.Store(0)
	if err := host.reconcilePools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.allActive()) != 1 || mintedA != 1 || mintedB != 1 {
		t.Fatalf("reallocation: active=%d minted-a=%d minted-b=%d", len(host.allActive()), mintedA, mintedB)
	}
	second := poolB.table.Active()[0]
	if second.Pool != "org-b" || second.Unit != "tentacle-org-b-0001.service" {
		t.Fatalf("second slot not namespaced to org-b: %+v", second)
	}
}

func TestSchedulerRetriesCleanupWithoutDemand(t *testing.T) {
	host := &daemon{
		cfg: &config.Config{
			Capacity: config.Capacity{MaxRunners: 1},
			Runtime:  config.Runtime{SlotStopTimeout: time.Second},
		},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		met:     metrics.NewRegistry(),
		pools:   []*poolRuntime{},
		reconCh: make(chan struct{}, 1),
	}
	var diagCalls atomic.Int32
	firstDone := make(chan struct{})
	diag := func(ctx context.Context, _ slot.Slot) error {
		if diagCalls.Add(1) != 1 {
			return nil
		}
		<-ctx.Done()
		close(firstDone)
		return ctx.Err()
	}
	minted := 0
	pool := scheduledPool(
		t,
		host,
		"org-a",
		&minted,
		slot.WithCleanupTimeout(20*time.Millisecond),
		slot.WithIdleGrace(0),
		slot.WithDiagContext(diag),
	)
	host.pools = append(host.pools, pool)
	defer host.close()

	if err := host.reconcilePools(context.Background()); err != nil {
		t.Fatal(err)
	}
	slotDir := pool.table.Active()[0].Dir
	pool.desired.Store(0)
	if err := host.reconcilePools(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first cleanup did not reach its deadline")
	}

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(slotDir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup never converged with zero demand; calls=%d", diagCalls.Load())
		}
		if err := host.reconcilePools(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if calls := diagCalls.Load(); calls != 2 {
		t.Fatalf("cleanup calls = %d, want initial failure plus one retry", calls)
	}
}

func TestReadinessWaitsForEveryPool(t *testing.T) {
	host := &daemon{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pools: []*poolRuntime{
			{cfg: config.Pool{ID: "org-a"}, sessionUp: make(chan struct{})},
			{cfg: config.Pool{ID: "org-b"}, sessionUp: make(chan struct{})},
		},
	}
	notified := make(chan string, 1)
	original := sdNotify
	sdNotify = func(state string) error {
		notified <- state
		return nil
	}
	defer func() { sdNotify = original }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.notifyReady(ctx)
	close(host.pools[0].sessionUp)
	select {
	case state := <-notified:
		t.Fatalf("readiness fired before org-b connected: %s", state)
	case <-time.After(50 * time.Millisecond):
	}
	close(host.pools[1].sessionUp)
	select {
	case state := <-notified:
		if state != "READY=1" {
			t.Fatalf("notification = %q", state)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness did not fire after every pool connected")
	}
}
