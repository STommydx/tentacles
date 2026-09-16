package slot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/process"
)

type cleanupGapHandler struct {
	slog.Handler
	entered chan struct{}
	release chan struct{}
}

func (h *cleanupGapHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "slot exited" {
		close(h.entered)
		<-h.release
	}
	return nil
}

func TestStopWaitsUntilCleanupWorkerExists(t *testing.T) {
	h := &cleanupGapHandler{Handler: slog.NewTextHandler(os.Stderr, nil), entered: make(chan struct{}), release: make(chan struct{})}
	b := &interleavedBackend{}
	tab := NewTable(t.TempDir(), b, markerMaterialize, fakeJIT, t.TempDir(), slog.New(h), WithIdleGrace(0))
	defer tab.Close()
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	b.onStop = func(ctx context.Context, u string) error {
		if err := b.fakeBackend.Stop(ctx, u); err != nil {
			return err
		}
		<-h.entered
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- tab.Ensure(context.Background(), 0) }()
	returned := false
	select {
	case <-done:
		returned = true
	case <-time.After(50 * time.Millisecond):
	}
	close(h.release)
	if !returned {
		<-done
	}
	if returned {
		t.Fatal("Ensure(0) returned before confirmed-stop cleanup worker started")
	}
}

func TestProcessFailedStartDoesNotRemainBusyForever(t *testing.T) {
	b := process.New(process.Options{})
	tab := NewTable(t.TempDir(), b, markerMaterialize, fakeJIT, t.TempDir(), nil, WithRunUser("reviewer-user-that-does-not-exist", ""))
	defer tab.Close()
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if len(tab.Active()) > 0 {
		t.Fatalf("failed process launch remains busy with no process: %+v", tab.Active())
	}
}

func TestFailedCleanupDoesNotRestoreIdle(t *testing.T) {
	b := &interleavedBackend{}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	tab := NewTable(t.TempDir(), b, markerMaterialize, fakeJIT, t.TempDir(), nil, WithDiagHook(func(Slot) error { entered <- struct{}{}; <-release; return nil }), WithIdleGrace(0))
	defer tab.Close()
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	b.onStop = func(ctx context.Context, u string) error {
		if err := b.fakeBackend.Stop(ctx, u); err != nil {
			return err
		}
		<-entered
		return os.ErrDeadlineExceeded
	}
	if err := tab.Ensure(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	state := tab.Snapshot()[0].State
	close(release)
	if state != StateStopping {
		t.Fatalf("confirmed exited slot in active cleanup restored as %s (dir %s)", state, filepath.Join(tab.root, "0001"))
	}
}
