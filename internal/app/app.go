// Package app wires pool listeners and slot tables behind one host scheduler,
// shared payload, admission gate, metrics registry, and run loop.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/0xinterface/tentacles/internal/config"
	"github.com/0xinterface/tentacles/internal/history"
	"github.com/0xinterface/tentacles/internal/metrics"
	"github.com/0xinterface/tentacles/internal/payload"
	"github.com/0xinterface/tentacles/internal/process"
	"github.com/0xinterface/tentacles/internal/runner"
	"github.com/0xinterface/tentacles/internal/scaleset"
	"github.com/0xinterface/tentacles/internal/slot"
	"github.com/0xinterface/tentacles/internal/systemd"
	"github.com/0xinterface/tentacles/internal/version"
)

type Options struct {
	ConfigPath string
	DryRun     bool
	ScaleSets  map[string]ScaleSet

	// releasesURL overrides the releases API endpoint used to resolve
	// an unset runner.version; tests inject a fake server. Empty means
	// payload.ReleasesAPIURL.
	releasesURL string
	// numCPU and memAvailable are the host budget probes behind the
	// admission gate; tests inject fixed values.
	numCPU       func() int
	memAvailable func() (uint64, error)
	// backends lets tests inject failures at the provisioning seam.
	backends map[string]runner.Backend
}

// ScaleSet is the narrow surface app consumes from the scale-set adapter.
// *scaleset.Adapter satisfies it; tests substitute a fake.
type ScaleSet interface {
	EnsureScaleSet(ctx context.Context) error
	Run(ctx context.Context) error
	GenerateJIT(ctx context.Context, runnerName, workFolder string) (string, error)
	ScaleSetID() int
}

// Run loads the config, validates it, and — unless DryRun — runs the
// daemon until ctx is canceled. DryRun stops after validation.
func Run(ctx context.Context, opts Options) error {
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if opts.DryRun {
		fmt.Printf("config %s valid (dry run)\n", opts.ConfigPath)
		return nil
	}
	if err := cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("create state dirs: %w", err)
	}
	log := newLogger(cfg)
	log.Info(
		"starting tentacles",
		"version", version.String(),
		"pools", len(cfg.Pools),
		"backend", cfg.Runtime.Backend,
	)
	d, err := newDaemonContext(ctx, cfg, log, opts)
	if err != nil {
		return err
	}
	defer d.close()
	return d.run(ctx)
}

func newLogger(cfg *config.Config) *slog.Logger {
	lvl := slog.LevelInfo
	switch cfg.Observability.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// desiredCell is the latest desired runner count pushed by the listener.
type desiredCell struct {
	mu sync.Mutex
	n  int
}

func (c *desiredCell) Store(n int) { c.mu.Lock(); c.n = n; c.mu.Unlock() }
func (c *desiredCell) Load() int   { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// defaultReconcileInterval is the safety-net reconcile tick. Events nudge
// reconcile immediately; the tick retries failed starts and replenishes
// the warm pool even if an event is lost.
const defaultReconcileInterval = 30 * time.Second

// sd_notify seam; swapped in tests to observe readiness signalling.
var sdNotify = systemd.SdNotify

type daemon struct {
	diagSem           chan struct{}
	cfg               *config.Config
	log               *slog.Logger
	met               *metrics.Registry
	pools             []*poolRuntime
	httpSrv           *http.Server
	reconCh           chan struct{}
	reconcileInterval time.Duration
	privateKeys       map[string]string
	scheduleCursor    int

	hist         *history.Store
	numCPU       func() int
	memAvailable func() (uint64, error)
}

func newDaemon(cfg *config.Config, log *slog.Logger, opts Options) (*daemon, error) {
	return newDaemonContext(context.Background(), cfg, log, opts)
}

func newDaemonContext(
	ctx context.Context,
	cfg *config.Config,
	log *slog.Logger,
	opts Options,
) (*daemon, error) {
	identity, err := runner.ResolveIdentity(runner.Spec{
		User:  cfg.Runner.User,
		Group: cfg.Runner.Group,
	})
	if err != nil {
		return nil, fmt.Errorf("runner identity: %w", err)
	}
	if cfg.Runtime.Backend == config.BackendSystemd && identity.UID == 0 {
		return nil, errors.New("systemd jobs require a non-root runner user")
	}
	if cfg.Runtime.Backend == config.BackendSystemd && os.Geteuid() != 0 {
		return nil, errors.New("systemd backend requires the privileged supervisor service")
	}
	if cfg.Runtime.Backend == config.BackendSystemd {
		if err := systemd.CheckSupport(ctx); err != nil {
			return nil, err
		}
	}

	d := &daemon{
		cfg:               cfg,
		diagSem:           make(chan struct{}, 1),
		log:               log,
		met:               metrics.NewRegistry(),
		pools:             make([]*poolRuntime, 0, len(cfg.Pools)),
		privateKeys:       make(map[string]string),
		reconCh:           make(chan struct{}, 1),
		reconcileInterval: defaultReconcileInterval,
		numCPU:            runtime.NumCPU,
		memAvailable:      procMemAvailable,
	}
	d.met.SetHostMaxRunners(cfg.Capacity.MaxRunners)
	if opts.numCPU != nil {
		d.numCPU = opts.numCPU
	}
	if opts.memAvailable != nil {
		d.memAvailable = opts.memAvailable
	}

	payloadContext, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	runnerVersion := cfg.Runner.Version
	sha := cfg.Runner.SHA256
	if runnerVersion == "" {
		api := opts.releasesURL
		if api == "" {
			api = payload.ReleasesAPIURL
		}
		resolved, digest, err := payload.ResolveLatest(payloadContext, api)
		if err != nil {
			return nil, fmt.Errorf("resolve latest runner version: %w", err)
		}
		runnerVersion = resolved
		sha = digest
		log.Info("runner version resolved from latest release", "runner_version", runnerVersion)
	}
	payloadManager := payload.New(
		cfg.Paths.CacheDir,
		filepath.Join(cfg.Paths.StateDir, "template"),
		log,
	)
	downloadURL := cfg.Runner.DownloadURL
	if downloadURL == "" {
		downloadURL = payload.DownloadURL(runnerVersion)
	}
	if err := payloadManager.Ensure(payloadContext, runnerVersion, sha, downloadURL); err != nil {
		return nil, fmt.Errorf("ensure runner payload: %w", err)
	}

	hist, err := history.Open(filepath.Join(cfg.Paths.StateDir, "history.jsonl"))
	if err != nil {
		log.Warn("usage history unavailable; admission control stays inert", "err", err)
	} else {
		d.hist = hist
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("hostname: %w", err)
	}
	for _, poolConfig := range cfg.Pools {
		d.met.RegisterPool(poolConfig.ID)
		pool := newPoolRuntime(d, poolConfig)
		pool.desired.Store(poolConfig.Capacity.MinRunners)
		events := pool.buildEvents()
		pool.ss, err = d.scaleSetFor(poolConfig, events, hostname, opts)
		if err != nil {
			d.close()
			return nil, err
		}
		pool.backend = d.backendFor(poolConfig.ID, opts)

		tableOptions := []slot.TableOption{
			slot.WithNamespace(poolConfig.ID),
			slot.WithAcquireGrace(cfg.Runtime.AcquireGrace),
			slot.WithIdleGrace(cfg.Runtime.IdleGrace),
			slot.WithStartAllowed(func() bool { return pool.acquireRetryWait() == 0 }),
			slot.WithStopTimeout(cfg.Runtime.SlotStopTimeout),
			slot.WithCleanupTimeout(cfg.Runtime.CleanupTimeout),
			slot.WithStartTimeout(cfg.Runtime.SlotStartTimeout),
			slot.WithWorkDir(cfg.Runner.WorkDirectory),
			slot.WithStartObserver(func(duration time.Duration) {
				d.met.ObserveSlotStart(poolConfig.ID, duration.Seconds())
			}),
			slot.WithLimits(
				fmt.Sprintf("%d%%", cfg.Capacity.JobCPUQuotaPercent),
				cfg.Capacity.JobMemoryMax,
			),
			slot.WithDiagContext(pool.shipDiag),
			slot.WithRunUser(cfg.Runner.User, cfg.Runner.Group),
			slot.WithEnvFile(cfg.Runner.EnvironmentFile),
			slot.WithMaterializer(pool.materializeSlotContext(payloadManager)),
			slot.WithEventHook(pool.onSlotEvent),
			slot.WithSampleInterval(cfg.Scaling.SampleInterval),
			slot.WithCompletionHook(pool.onCompletion),
		}
		if usage, ok := pool.backend.(interface {
			Usage(string) (slot.Usage, error)
		}); ok {
			tableOptions = append(tableOptions, slot.WithUsageSampler(usage.Usage))
		}
		if cfg.Scaling.AdmissionControl && d.hist != nil {
			tableOptions = append(
				tableOptions,
				slot.WithGate(func([]slot.Slot) bool {
					return d.admissionGate(pool, d.allActive())
				}),
				slot.WithReservation(pool.candidateEstimate),
			)
		}
		pool.table = slot.NewTable(
			cfg.PoolSlotsDir(poolConfig.ID),
			pool.backend,
			nil,
			pool.mintJIT,
			cfg.PoolJITDir(poolConfig.ID),
			pool.log.WithGroup("slot"),
			tableOptions...,
		)
		d.pools = append(d.pools, pool)
	}
	return d, nil
}

func (d *daemon) scaleSetFor(
	pool config.Pool,
	events scaleset.Events,
	hostname string,
	opts Options,
) (ScaleSet, error) {
	if injected := opts.ScaleSets[pool.ID]; injected != nil {
		if sink, ok := injected.(interface{ SetEvents(scaleset.Events) }); ok {
			sink.SetEvents(events)
		}
		return injected, nil
	}

	privateKey, err := d.privateKey(pool.GitHub.App.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("pool %q: read app private key: %w", pool.ID, err)
	}
	githubURL := strings.TrimSuffix(pool.GitHub.URL, "/") + "/" + pool.GitHub.Scope.Owner
	if pool.GitHub.Scope.Kind == "repository" {
		githubURL += "/" + pool.GitHub.Scope.Repository
	}
	adapter, err := scaleset.New(scaleset.Config{
		GitHubURL:      githubURL,
		ClientID:       pool.GitHub.App.ClientID,
		InstallationID: pool.GitHub.App.InstallationID,
		PrivateKeyPEM:  privateKey,
		OwnerName:      pool.GitHub.Scope.Owner,
		Repository:     pool.GitHub.Scope.Repository,
		ScaleSetName:   pool.ScaleSet.Name,
		RunnerGroup:    pool.ScaleSet.RunnerGroup,
		ExtraLabels:    pool.ScaleSet.ExtraLabels,
		MinRunners:     pool.Capacity.MinRunners,
		MaxRunners:     pool.Capacity.MaxRunners,
		DisableUpdate:  d.cfg.Runner.DisableUpdate,
		SystemVersion:  version.String(),
		SessionOwner:   hostname + "-" + pool.ID,
	}, events, d.log.With("pool", pool.ID).WithGroup("scaleset"))
	if err != nil {
		return nil, fmt.Errorf("pool %q: create scale-set client: %w", pool.ID, err)
	}
	return adapter, nil
}

func (d *daemon) privateKey(path string) (string, error) {
	if key, ok := d.privateKeys[path]; ok {
		return key, nil
	}
	if d.privateKeys == nil {
		d.privateKeys = make(map[string]string)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key := string(contents)
	d.privateKeys[path] = key
	return key, nil
}

func (d *daemon) backendFor(poolID string, opts Options) runner.Backend {
	if backend := opts.backends[poolID]; backend != nil {
		return backend
	}
	if d.cfg.Runtime.Backend == config.BackendSystemd {
		return systemd.New(systemd.Options{
			Log:          d.log.With("pool", poolID).WithGroup("systemd"),
			StopTimeout:  d.cfg.Runtime.SlotStopTimeout,
			Namespace:    poolID,
			CacheSubdirs: d.cfg.Runner.SharedCachePaths,
		})
	}
	return process.New(process.Options{
		EnvFile: d.cfg.Runner.EnvironmentFile,
		Log:     d.log.With("pool", poolID).WithGroup("process"),
	})
}

// procMemAvailable reads MemAvailable from /proc/meminfo. Unsupported
// platforms return an error; the gate then skips the memory check.
func procMemAvailable() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, errors.New("meminfo: MemAvailable not found")
}

func (d *daemon) run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", d.met.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	d.httpSrv = &http.Server{
		Addr:              d.cfg.Observability.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	listener, err := net.Listen("tcp", d.httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("listen metrics: %w", err)
	}
	go func() {
		if err := d.httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("metrics server failed", "err", err)
		}
	}()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := d.httpSrv.Shutdown(shutdownContext); err != nil {
			_ = d.httpSrv.Close()
		}
	}()

	for _, pool := range d.pools {
		if err := pool.ss.EnsureScaleSet(ctx); err != nil {
			return fmt.Errorf("pool %q: ensure scale set: %w", pool.cfg.ID, err)
		}
		pool.log.Info(
			"scale set ready",
			"scale_set", pool.cfg.ScaleSet.Name,
			"scale_set_id", pool.ss.ScaleSetID(),
		)
	}
	adoptContext, cancelAdoption := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelAdoption()
	for _, pool := range d.pools {
		units, err := pool.backend.Active(adoptContext)
		if err != nil {
			return fmt.Errorf("pool %q: boot adoption: list active units: %w", pool.cfg.ID, err)
		}
		if err := pool.table.Adopt(adoptContext, units); err != nil {
			return fmt.Errorf("pool %q: boot adoption: %w", pool.cfg.ID, err)
		}
	}

	listenerDone := make(chan struct{}, len(d.pools))
	for _, pool := range d.pools {
		go func() {
			defer func() { listenerDone <- struct{}{} }()
			d.runPoolListener(ctx, pool)
		}()
	}
	go d.notifyReady(ctx)

	metricsDone := make(chan struct{})
	go func() {
		defer close(metricsDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.syncMetrics()
			}
		}
	}()

	d.requestReconcile()
	tick := time.NewTicker(d.reconcileInterval)
	defer tick.Stop()
	var retryTimer *time.Timer
	var retry <-chan time.Time
	resetRetry := func(delay time.Duration) {
		if retryTimer != nil {
			if !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
		}
		if delay <= 0 {
			retry = nil
			return
		}
		if retryTimer == nil {
			retryTimer = time.NewTimer(delay)
		} else {
			retryTimer.Reset(delay)
		}
		retry = retryTimer.C
	}
	defer func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			d.log.Info("shutting down")
			d.shutdownSlots()
			for range d.pools {
				<-listenerDone
			}
			<-metricsDone
			return nil
		case <-d.reconCh:
		case <-tick.C:
		case <-retry:
		}
		reconcileContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if err := d.reconcilePools(reconcileContext); err != nil && ctx.Err() == nil {
			d.log.Error("reconcile failed", "err", err)
		}
		cancel()
		resetRetry(d.nextRetryWait())
	}
}

func (d *daemon) runPoolListener(ctx context.Context, pool *poolRuntime) {
	backoff := []time.Duration{
		time.Second,
		2 * time.Second,
		5 * time.Second,
		15 * time.Second,
		30 * time.Second,
	}
	for attempt := 0; ; attempt++ {
		err := pool.ss.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.met.IncListenerErrors(pool.cfg.ID)
			pool.log.Error("listener run failed", "err", err)
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		pool.log.Warn("listener restarting", "backoff", wait.String())
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

func (d *daemon) notifyReady(ctx context.Context) {
	for _, pool := range d.pools {
		select {
		case <-pool.sessionUp:
		case <-ctx.Done():
			return
		}
	}
	if err := sdNotify("READY=1"); err != nil {
		d.log.Warn("sd_notify failed", "err", err)
	}
}

func (d *daemon) syncMetrics() {
	states := [...]slot.State{
		slot.StateEmpty,
		slot.StateStarting,
		slot.StateIdle,
		slot.StateBusy,
		slot.StateStopping,
		slot.StateFailed,
	}
	for _, pool := range d.pools {
		d.met.SetDesired(pool.cfg.ID, pool.table.Desired())
		var counts [len(states)]int
		for _, runnerSlot := range pool.table.Snapshot() {
			for i, state := range states {
				if runnerSlot.State == state {
					counts[i]++
					break
				}
			}
		}
		for i, state := range states {
			d.met.SetActual(pool.cfg.ID, string(state), counts[i])
		}
	}
}

func (d *daemon) requestReconcile() {
	select {
	case d.reconCh <- struct{}{}:
	default:
	}
}

// shutdownSlots stops every pool's surplus slots now, bypassing the idle
// grace: no new assignment can be honored once the daemon is going away.
// Starting slots still keep their acquire grace and busy runners finish
// naturally; both are adopted from their pool namespace after restart.
func (d *daemon) shutdownSlots() {
	for _, pool := range d.pools {
		shutdownContext, cancel := context.WithTimeout(
			context.Background(),
			d.cfg.Runtime.SlotStopTimeout,
		)
		if err := pool.table.Shutdown(shutdownContext); err != nil {
			pool.log.Error("stopping idle slots on shutdown", "err", err)
		}
		cancel()
	}
}

func (d *daemon) close() {
	for _, pool := range d.pools {
		if pool.table != nil {
			pool.table.Close()
		}
	}
}

// diskUsage reports free and total bytes on the filesystem holding p.
// It returns an error on unsupported platforms; callers treat failure as
// "cannot check" and proceed.
func diskUsage(p string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
