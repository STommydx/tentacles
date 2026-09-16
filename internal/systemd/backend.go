// Package systemd implements runner.Backend with transient systemd units
// created via systemd-run(1), plus sd_notify(3) readiness signalling for
// the daemon's own unit.
//
// Every knob (user, quotas, hardening properties) is passed to systemd-run
// as -p properties, so values come from configuration rather than a
// static unit file. The systemd-run and systemctl binaries are exec'd
// (overridable for tests); there is no D-Bus dependency.
package systemd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xinterface/tentacles/internal/env"
	"github.com/0xinterface/tentacles/internal/runner"
	"github.com/0xinterface/tentacles/internal/slot"
)

// Options configures the systemd backend.
type Options struct {
	// SystemdRunBin is the systemd-run binary; empty defaults to
	// "systemd-run". Tests inject a fake via PATH.
	SystemdRunBin string
	// SystemctlBin is the systemctl binary; empty defaults to
	// "systemctl". Tests inject a fake via PATH.
	SystemctlBin string
	// Log receives structured backend diagnostics; nil defaults to
	// slog.Default().
	Log *slog.Logger
	// StopTimeout is the TimeoutStopSec value set on each slot unit.
	StopTimeout time.Duration
	// Namespace is the configured pool ID embedded in every transient unit.
	Namespace string
	// CacheSubdirs are the HOME-relative shared cache directories added to
	// each unit's ReadWritePaths and pre-created as the runner user before
	// start. Empty selects the runnerCacheSubdirs default.
	CacheSubdirs []string
}

// runnerCacheSubdirs are the default HOME-relative shared cache locations
// jobs may write despite ProtectHome=read-only. These exceptions let the
// runner use the host's toolchains and shared caches. Operators extend or
// replace the list with runner.shared_cache_paths.
var runnerCacheSubdirs = []string{".cache", ".local/share/mise", "go/pkg/mod"}

// Backend starts, stops, and waits on transient
// tentacle-<pool>-<id>.service units. All methods are safe for concurrent
// use.
type Backend struct {
	systemdRunBin string
	systemctlBin  string
	log           *slog.Logger
	namespace     string
	stopTimeout   time.Duration
	cacheSubdirs  []string

	mu      sync.Mutex
	started map[string]struct{} // units this backend has started
}

// New returns a Backend with the given options and binary defaults.
func New(opts Options) *Backend {
	runBin := opts.SystemdRunBin
	if runBin == "" {
		runBin = "systemd-run"
	}
	ctlBin := opts.SystemctlBin
	if ctlBin == "" {
		ctlBin = "systemctl"
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = 30 * time.Second
	}
	cacheSubdirs := opts.CacheSubdirs
	if len(cacheSubdirs) == 0 {
		cacheSubdirs = runnerCacheSubdirs
	}
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "default"
	}
	return &Backend{
		systemdRunBin: runBin,
		systemctlBin:  ctlBin,
		namespace:     namespace,
		log:           log,
		stopTimeout:   opts.StopTimeout,
		cacheSubdirs:  cacheSubdirs,
		started:       make(map[string]struct{}),
	}
}

// Start launches run.sh in the slot described by spec as a transient unit.
// With Type=exec, systemd-run blocks until the service has exec'd, so a
// nil return means the runner process is running (or at least has forked).
func (b *Backend) Start(ctx context.Context, spec runner.Spec) (retErr error) {
	attempted := false
	defer func() {
		if retErr != nil && !attempted {
			retErr = fmt.Errorf("%w: %w", runner.ErrNotStarted, retErr)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if spec.User == "" {
		return fmt.Errorf("systemd: runner user is required")
	}
	identity, err := runner.ResolveIdentity(spec)
	if err != nil {
		return err
	}
	if identity.UID == 0 {
		return fmt.Errorf("systemd: runner user must be unprivileged")
	}
	if !validUnit(b.namespace, spec.UnitName) {
		return fmt.Errorf("systemd: invalid slot unit %q for pool %q", spec.UnitName, b.namespace)
	}
	if !filepath.IsAbs(spec.SlotDir) || !filepath.IsAbs(spec.JITPath) {
		return fmt.Errorf("systemd: slot and JIT paths must be absolute")
	}
	vars := map[string]string{}
	if spec.EnvFile != "" {
		if !filepath.IsAbs(spec.EnvFile) {
			return fmt.Errorf("systemd: environment file must be absolute")
		}
		vars, err = env.ParseFile(spec.EnvFile)
		if err != nil {
			return fmt.Errorf("systemd: parse environment file: %w", err)
		}
	}
	if _, err := runner.ReadJIT(spec.JITPath); err != nil {
		return fmt.Errorf("systemd: JIT source: %w", err)
	}
	home := identity.Home
	if value, ok := vars["HOME"]; ok {
		home = value
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("systemd: runner HOME must be absolute")
	}
	caches := b.cachePaths(home)
	// Create caches as the job identity: no privileged traversal or chown of
	// directories a previous job can replace with symlinks.
	mkdirArgs := append([]string{"-p", "--"}, caches...)
	mkdir := runner.CommandContext(ctx, "/bin/mkdir", mkdirArgs...)
	mkdir.SysProcAttr.Credential = identity.Credential
	if out, err := mkdir.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd: prepare runner caches: %w: %s", err, tail(out))
	}
	if err := runner.PrepareSlot(ctx, spec, identity); err != nil {
		return err
	}
	args := b.startArgs(spec, caches...)
	cmd := runner.CommandContext(ctx, b.systemdRunBin, args...)
	attempted = true
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("systemd: start %s: %w: %s", spec.UnitName, err, tail(out))
	}
	b.mu.Lock()
	b.started[spec.UnitName] = struct{}{}
	b.mu.Unlock()
	b.log.Debug("slot unit started", "unit", spec.UnitName)
	return nil
}

// startArgs builds the exact systemd-run argument vector for a slot,
// ending in a static shell that reads systemd's private credential copy.
func (b *Backend) startArgs(spec runner.Spec, caches ...string) []string {
	args := []string{
		"--collect",
		"--expand-environment=no",
		"--unit", spec.UnitName,
		"--description", "GitHub Actions runner slot",
		"-p", "Type=exec",
	}
	addProp := func(key, value string) {
		if value != "" {
			args = append(args, "-p", key+"="+value)
		}
	}
	addProp("User", spec.User)
	addProp("Group", spec.Group)
	addProp("WorkingDirectory", spec.SlotDir)
	addProp("EnvironmentFile", spec.EnvFile)
	addProp("LoadCredential", "jit:"+spec.JITPath)
	addProp("CPUQuota", spec.CPUQuota)
	addProp("MemoryMax", spec.MemoryMax)
	paths := append([]string{spec.SlotDir, "/tmp"}, caches...)
	for i, path := range paths {
		paths[i] = quotePath(path)
	}
	args = append(args,
		"-p", "Nice=5",
		"-p", "KillMode=mixed",
		"-p", "TimeoutStopSec="+strconv.FormatFloat(b.stopTimeout.Seconds(), 'f', -1, 64),
		"-p", "TasksMax=4096",
		"-p", "PrivateTmp=yes",
		"-p", "NoNewPrivileges=yes",
		"-p", "CPUAccounting=yes",
		"-p", "MemoryAccounting=yes",
		"-p", "ProtectSystem=strict",
		"-p", "ProtectHome=read-only",
		"-p", "ReadWritePaths="+strings.Join(paths, " "),
		"-p", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"-p", "LockPersonality=yes",
		"/bin/sh", "-c", runner.CredentialScript(),
	)
	return args
}

// systemd-run passes literal path values over D-Bus; systemd itself escapes
// percent specifiers when persisting the unit. Doubling them here would change
// the actual pathname. The list parser unquotes but does not decode C escapes.
func quotePath(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func (b *Backend) cachePaths(home string) []string {
	paths := make([]string, 0, len(b.cacheSubdirs))
	for _, sub := range b.cacheSubdirs {
		paths = append(paths, filepath.Join(home, sub))
	}
	return paths
}

func validUnit(namespace, unit string) bool {
	unitNamespace, ok := namespaceFromUnit(unit)
	return ok && unitNamespace == namespace
}

// namespaceFromUnit parses from the final numeric slot suffix. A systemctl
// glob for "org-a" also returns units from "org-a-b"; parsing the whole name
// lets each adapter ignore valid units owned by another pool.
func namespaceFromUnit(unit string) (string, bool) {
	const prefix = "tentacle-"
	const suffix = ".service"
	if !strings.HasPrefix(unit, prefix) || !strings.HasSuffix(unit, suffix) {
		return "", false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(unit, prefix), suffix)
	separator := strings.LastIndexByte(body, '-')
	if separator <= 0 || separator == len(body)-1 {
		return "", false
	}
	namespace := body[:separator]
	if !validNamespace(namespace) {
		return "", false
	}
	for _, r := range body[separator+1:] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return namespace, true
}

func validNamespace(namespace string) bool {
	if len(namespace) == 0 || len(namespace) > 32 {
		return false
	}
	for i, r := range namespace {
		isDigit := r >= '0' && r <= '9'
		isLower := r >= 'a' && r <= 'z'
		if !isDigit && !isLower && (i == 0 || r != '-') {
			return false
		}
	}
	return true
}

// Stop terminates the unit and returns once it is gone. The started set
// is left untouched here; it is reconciled by the next Active scan.
func (b *Backend) Stop(ctx context.Context, unit string) error {
	ctx, cancel := context.WithTimeout(ctx, b.stopTimeout+10*time.Second)
	defer cancel()
	if !validUnit(b.namespace, unit) {
		return fmt.Errorf("systemd: invalid slot unit %q for pool %q", unit, b.namespace)
	}
	cmd := runner.CommandContext(ctx, b.systemctlBin, "stop", unit)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("systemd: stop %s: %w: %s", unit, err, tail(out))
	}
	b.log.Debug("slot unit stopped", "unit", unit)
	return nil
}

// Wait confirms exit only after a successful, well-formed state query.
// Communication errors and malformed output never establish that a job ended.
func (b *Backend) Wait(ctx context.Context, unit string) error {
	if !validUnit(b.namespace, unit) {
		return fmt.Errorf("systemd: invalid slot unit %q for pool %q", unit, b.namespace)
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := b.isActive(ctx, unit)
		if err != nil {
			return err
		}
		if state == "inactive" || state == "failed" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (b *Backend) isActive(ctx context.Context, unit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := runner.CommandContext(ctx, b.systemctlBin, "show", unit, "--property=LoadState", "--property=ActiveState")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("systemd: observe %s: %w: %s", unit, err, tail(stderr.Bytes()))
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		_, duplicate := values[key]
		if !ok || (key != "LoadState" && key != "ActiveState") || duplicate {
			return "", fmt.Errorf("systemd: malformed state observation for %s", unit)
		}
		values[key] = value
	}
	switch values["LoadState"] {
	case "loaded", "not-found", "error", "masked", "bad-setting", "merged", "stub":
	default:
		return "", fmt.Errorf("systemd: unknown load state for %s", unit)
	}
	state := values["ActiveState"]
	switch state {
	case "active", "reloading", "inactive", "failed", "activating", "deactivating", "maintenance", "refreshing":
		return state, nil
	default:
		return "", fmt.Errorf("systemd: unknown active state for %s", unit)
	}
}

// Active lists this pool's running transient units on the host for boot
// adoption. Command errors are returned honestly so the caller can decide
// how to treat them. The scan also prunes the started set.
func (b *Backend) Active(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	pattern := "tentacle-" + b.namespace + "-*.service"
	cmd := runner.CommandContext(ctx, b.systemctlBin,
		"list-units", pattern, "--no-legend", "--plain", "--no-pager", "--full", "--all")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("systemd: list-units: %w: %s", err, tail(stderr.Bytes()))
	}
	units := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("systemd: malformed list-units output")
		}
		namespace, ok := namespaceFromUnit(fields[0])
		if !ok {
			return nil, fmt.Errorf("systemd: malformed list-units output")
		}
		if namespace != b.namespace {
			continue
		}
		switch fields[2] {
		case "active", "activating", "deactivating", "reloading", "maintenance", "refreshing", "inactive", "failed":
			// Include inactive/failed units as well: adoption must clean their slot
			// directories through the same confirmed-exit path as running units.
			units = append(units, fields[0])
		default:
			return nil, fmt.Errorf("systemd: unknown list-units state %q", fields[2])
		}
	}
	b.mu.Lock()
	for unit := range b.started {
		if !slices.Contains(units, unit) {
			delete(b.started, unit)
		}
	}
	b.mu.Unlock()
	return units, nil
}

// Usage reads cumulative CPU time and peak memory for a unit from
// systemd's accounting (CPUAccounting=yes and MemoryAccounting=yes are
// set on every slot unit). The Table samples this while a job runs so
// usage can be attributed at exit, before systemd garbage-collects the
// unit.
func (b *Backend) Usage(unit string) (slot.Usage, error) {
	if !validUnit(b.namespace, unit) {
		return slot.Usage{}, fmt.Errorf("systemd: invalid slot unit %q for pool %q", unit, b.namespace)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := runner.CommandContext(ctx, b.systemctlBin, "show", unit,
		"--property=CPUUsageNSec", "--property=MemoryPeak", "--property=MemoryCurrent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return slot.Usage{}, fmt.Errorf("systemd: show %s: %w: %s", unit, err, tail(out))
	}
	var u slot.Usage
	for _, line := range strings.Split(string(out), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch name {
		case "CPUUsageNSec":
			ns, err := strconv.ParseInt(value, 10, 64)
			if err == nil && ns > 0 {
				u.CPUSeconds = float64(ns) / 1e9
			}
		case "MemoryCurrent":
			if bytes, err := strconv.ParseUint(value, 10, 64); err == nil {
				u.CurrentMemBytes = bytes
			}
		case "MemoryPeak":
			if bytes, err := strconv.ParseUint(value, 10, 64); err == nil {
				u.PeakMemBytes = bytes
			}
		}
	}
	return u, nil
}

// tail returns the last ~2KB of combined command output for error
// messages, trimmed of surrounding whitespace.
func tail(out []byte) string {
	const maxTail = 2 * 1024
	if len(out) > maxTail {
		out = out[len(out)-maxTail:]
	}
	return strings.TrimSpace(string(out))
}
