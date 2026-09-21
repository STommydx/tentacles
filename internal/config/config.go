// Package config loads, validates, and exposes the tentacles YAML
// configuration. Keep fields and defaults documented in README.md and
// configs/config.example.yaml when changing the YAML layout.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0xinterface/tentacles/internal/env"
)

// HardCapMaxRunners is the compiled ceiling for capacity.max_runners.
// It exists so a typo cannot ask the host for a thousand runner
// processes; raise it only with the host's resources in mind.
const HardCapMaxRunners = 32

// AllowUnverifiedPayloadEnv, when set to "1", skips the runner.sha256
// requirement so an unverified payload can be fetched. It matches
// internal/payload.AllowUnverifiedEnv; keep the values in lockstep.
const AllowUnverifiedPayloadEnv = "TENTACLES_ALLOW_UNVERIFIED_PAYLOAD"

// Default directories and values applied by Load when the YAML omits them.
const (
	DefaultGitHubURL          = "https://github.com"
	DefaultRunnerGroup        = "Default"
	DefaultWorkDir            = "_work"
	DefaultRunnerUser         = "gha-runner"
	DefaultStateDir           = "/var/lib/tentacles"
	DefaultCacheDir           = "/var/cache/tentacles"
	DefaultLogDir             = "/var/log/tentacles"
	DefaultJitDir             = "/run/tentacles"
	DefaultBackend            = "systemd"
	DefaultListen             = "127.0.0.1:9090"
	DefaultLogLevel           = "info"
	DefaultDiagMaxAge         = 7 * 24 * time.Hour
	DefaultDiagMaxBytes int64 = 1 << 30
)

// Backend names for runtime.backend.
const (
	BackendSystemd = "systemd"
	BackendProcess = "process"
)

// Default slot lifecycle timeouts.
const (
	DefaultSlotStartTimeout = 90 * time.Second
	DefaultSlotStopTimeout  = 30 * time.Second
	DefaultCleanupTimeout   = 60 * time.Second
	DefaultAcquireGrace     = 3 * time.Minute
	DefaultIdleGrace        = 30 * time.Second
)

// DefaultSharedCachePaths are the HOME-relative directories every job may
// write despite ProtectHome=read-only. The XDG cache tree holds pip and
// Go build caches plus RUNNER_TOOL_CACHE (see the shipped runner.env
// example); the other two keep mise toolchains and Go modules warm across
// jobs.
var DefaultSharedCachePaths = []string{".cache", ".local/share/mise", "go/pkg/mod"}

// MaxCPUQuotaPercent is the upper bound for capacity.job_cpu_quota_percent
// (systemd CPUQuota accepts a ceiling of 100 * NumCPU; 100*1024 covers any
// plausible host).
const MaxCPUQuotaPercent = 100 * 1024

// Config is the root of the tentacles YAML document.
type Config struct {
	Pools         []Pool        `yaml:"pools"`
	Capacity      Capacity      `yaml:"capacity"`
	Runner        Runner        `yaml:"runner"`
	Paths         Paths         `yaml:"paths"`
	Runtime       Runtime       `yaml:"runtime"`
	Scaling       Scaling       `yaml:"scaling"`
	Observability Observability `yaml:"observability"`
}

// Admission-gate defaults.
const (
	DefaultCPUTargetPercent    = 90
	DefaultMemoryMarginPercent = 20
	DefaultSampleInterval      = 30 * time.Second
)

// Pool identifies one independently authenticated GitHub runner scale set.
// All pools share the host payload, runner identity, and capacity scheduler.
type Pool struct {
	ID       string       `yaml:"id"`
	GitHub   GitHub       `yaml:"github"`
	ScaleSet ScaleSet     `yaml:"scale_set"`
	Capacity PoolCapacity `yaml:"capacity"`
}

// GitHub holds the GitHub App credentials and target scope for one pool.
type GitHub struct {
	URL   string `yaml:"url"`
	App   App    `yaml:"app"`
	Scope Scope  `yaml:"scope"`
}

// App is the GitHub App installation used for authentication.
type App struct {
	ClientID       string `yaml:"client_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

// Scope selects the organization or repository that owns the scale set.
type Scope struct {
	Kind       string `yaml:"kind"` // organization | repository
	Owner      string `yaml:"owner"`
	Repository string `yaml:"repository"`
}

// ScaleSet describes one GitHub Actions runner scale set.
type ScaleSet struct {
	Name        string   `yaml:"name"`
	RunnerGroup string   `yaml:"runner_group"`
	ExtraLabels []string `yaml:"extra_labels"`
}

// PoolCapacity bounds one pool's requested runners. The host-wide ceiling
// remains Capacity.MaxRunners.
type PoolCapacity struct {
	MinRunners int `yaml:"min_runners"`
	MaxRunners int `yaml:"max_runners"`
}

// Capacity bounds total runner processes on the host and their per-slot
// resource limits.
type Capacity struct {
	MaxRunners         int    `yaml:"max_runners"`
	JobCPUQuotaPercent int    `yaml:"job_cpu_quota_percent"`
	JobMemoryMax       string `yaml:"job_memory_max"`
}

// Runner pins the official actions/runner payload and how slots run it.
type Runner struct {
	Version         string `yaml:"version"`
	DownloadURL     string `yaml:"download_url"`
	SHA256          string `yaml:"sha256"`
	WorkDirectory   string `yaml:"work_directory"`
	DisableUpdate   bool   `yaml:"disable_update"`
	User            string `yaml:"user"`
	Group           string `yaml:"group"` // slot unit group; "" means same as user
	EnvironmentFile string `yaml:"environment_file"`
	// SharedCachePaths lists HOME-relative directories shared and writable
	// by every job on this host despite ProtectHome=read-only. Omitted or
	// empty selects DefaultSharedCachePaths. Entries must be relative and
	// stay under the runner HOME; paths outside ~/.cache typically need
	// matching ReadWritePaths on the supervisor's own unit.
	SharedCachePaths []string `yaml:"shared_cache_paths"`
	// ExtraAddressFamilies lists socket address families added to every slot
	// unit's RestrictAddressFamilies allowlist on top of the always-present
	// AF_UNIX, AF_INET, and AF_INET6. Each entry is an AF_ token (for example
	// AF_NETLINK, which a userspace Tailscale netmon socket opens). Omitted or
	// empty keeps only the base three. Widen this only when a job genuinely
	// needs the family: every added family is extra kernel attack surface.
	ExtraAddressFamilies []string `yaml:"extra_address_families"`
}

// Paths are the daemon's on-disk homes.
type Paths struct {
	StateDir string `yaml:"state_dir"`
	CacheDir string `yaml:"cache_dir"`
	LogDir   string `yaml:"log_dir"`
}

// Runtime tunes slot lifecycle behavior.
type Runtime struct {
	Backend          string        `yaml:"backend"`
	SlotStartTimeout time.Duration `yaml:"slot_start_timeout"`
	SlotStopTimeout  time.Duration `yaml:"slot_stop_timeout"`
	CleanupTimeout   time.Duration `yaml:"cleanup_timeout"`
	AcquireGrace     time.Duration `yaml:"acquire_grace"`
	IdleGrace        time.Duration `yaml:"idle_grace"`
	JitDir           string        `yaml:"jit_dir"`
}

// Scaling tunes the history-based admission gate. When
// admission_control is on, a new slot is held back whenever the
// predicted resource usage of all live slots plus the incoming job
// would exceed the host budget derived from these targets.
type Scaling struct {
	AdmissionControl    bool          `yaml:"admission_control"`
	CPUTargetPercent    int           `yaml:"cpu_target_percent"`
	MemoryMarginPercent int           `yaml:"memory_margin_percent"`
	SampleInterval      time.Duration `yaml:"sample_interval"`
}

// Observability configures the metrics endpoint and logging.
type Observability struct {
	Listen       string        `yaml:"listen"`
	LogLevel     string        `yaml:"log_level"`
	ShipDiag     bool          `yaml:"ship_diag"`
	DiagMaxAge   time.Duration `yaml:"diag_max_age"`
	DiagMaxBytes int64         `yaml:"diag_max_bytes"`
}

var (
	// labelRe matches a GitHub Actions runner label: letters, digits,
	// underscore first, then dots, dashes and underscores.
	labelRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	// poolIDRe keeps pool IDs safe in paths, systemd unit names, and labels.
	poolIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// versionRe matches a bare X.Y.Z release version.
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	// sha256Re matches a 64-character lowercase or uppercase hex digest.
	sha256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	// addressFamilyRe matches a systemd RestrictAddressFamilies token: the
	// AF_ prefix followed by uppercase letters and digits (AF_UNIX, AF_INET6,
	// AF_NETLINK).
	addressFamilyRe = regexp.MustCompile(`^AF_[A-Z0-9]+$`)
)

// systemdRuntimeDir is probed to decide whether the systemd backend is
// available on this host. Overridden in tests.
var systemdRuntimeDir = "/run/systemd/system"

// Validate checks configuration constraints and returns a joined error
// listing all detected failures. Runtime preflight happens during startup.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// Host capacity.
	if c.Capacity.MaxRunners < 1 {
		fail("capacity.max_runners must be >= 1 (got %d)", c.Capacity.MaxRunners)
	}
	if c.Capacity.MaxRunners > HardCapMaxRunners {
		fail("capacity.max_runners (%d) exceeds the hard cap of %d", c.Capacity.MaxRunners, HardCapMaxRunners)
	}
	if c.Capacity.JobCPUQuotaPercent <= 0 || c.Capacity.JobCPUQuotaPercent > MaxCPUQuotaPercent {
		fail("capacity.job_cpu_quota_percent must be in (0, %d] (got %d)", MaxCPUQuotaPercent, c.Capacity.JobCPUQuotaPercent)
	}
	if c.Capacity.JobMemoryMax == "" {
		fail("capacity.job_memory_max must not be empty")
	}

	if len(c.Pools) == 0 {
		fail("pools must contain at least one runner pool")
	}
	ids := make(map[string]int, len(c.Pools))
	targets := make(map[string]int, len(c.Pools))
	totalMin := 0
	for i, pool := range c.Pools {
		errs = append(errs, validatePool(i, pool, c.Capacity.MaxRunners)...)
		totalMin += pool.Capacity.MinRunners
		if previous, ok := ids[pool.ID]; ok {
			fail("pools[%d].id %q duplicates pools[%d].id", i, pool.ID, previous)
		} else {
			ids[pool.ID] = i
		}
		target := strings.Join([]string{
			pool.GitHub.URL,
			pool.GitHub.Scope.Kind,
			pool.GitHub.Scope.Owner,
			pool.GitHub.Scope.Repository,
			pool.ScaleSet.RunnerGroup,
			pool.ScaleSet.Name,
		}, "\x00")
		if previous, ok := targets[target]; ok {
			fail("pools[%d] duplicates the GitHub scale-set target of pools[%d]", i, previous)
		} else {
			targets[target] = i
		}
	}
	if totalMin > c.Capacity.MaxRunners {
		fail(
			"sum of pools[].capacity.min_runners (%d) exceeds capacity.max_runners (%d)",
			totalMin,
			c.Capacity.MaxRunners,
		)
	}

	// Runner payload. An unset version means "track the latest
	// release": the daemon resolves the version and its asset digest
	// from the GitHub releases API at startup. A pinned
	// sha256 requires a pinned version — a digest cannot constrain a
	// version that moves with every release.
	dynamicVersion := c.Runner.Version == ""
	if !dynamicVersion && !versionRe.MatchString(c.Runner.Version) {
		fail("runner.version %q must look like X.Y.Z (e.g. 2.328.0), or be unset to track the latest release", c.Runner.Version)
	}
	if dynamicVersion && c.Runner.SHA256 != "" {
		fail("runner.sha256 cannot be pinned while runner.version tracks the latest release; set runner.version too")
	}
	if !dynamicVersion && os.Getenv(AllowUnverifiedPayloadEnv) != "1" {
		if !sha256Re.MatchString(c.Runner.SHA256) {
			fail("runner.sha256 must be a 64-character hex digest (got %d characters); set %s=1 to allow an unverified payload", len(c.Runner.SHA256), AllowUnverifiedPayloadEnv)
		}
	}
	if c.Runner.EnvironmentFile == "" {
		fail("runner.environment_file must not be empty")
	} else if _, err := os.Stat(c.Runner.EnvironmentFile); err != nil {
		fail("runner.environment_file %q does not exist: %v", c.Runner.EnvironmentFile, err)
	} else if vars, err := env.ParseFile(c.Runner.EnvironmentFile); err != nil {
		fail("runner.environment_file %q is not a valid environment file: %v", c.Runner.EnvironmentFile, err)
	} else if err := env.Validate(vars); err != nil {
		fail("runner.environment_file %q: %v", c.Runner.EnvironmentFile, err)
	}

	// Shared cache paths are HOME-relative: the unit resolves them against
	// the runner environment's HOME at start time.
	seenCache := make(map[string]bool, len(c.Runner.SharedCachePaths))
	for i, p := range c.Runner.SharedCachePaths {
		switch {
		case p == "" || p == ".":
			fail("runner.shared_cache_paths[%d] must name a directory under the runner HOME (got %q)", i, p)
		case filepath.IsAbs(p):
			fail("runner.shared_cache_paths[%d] %q must be relative to the runner HOME", i, p)
		}
		for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
			if seg == ".." {
				fail("runner.shared_cache_paths[%d] %q must stay under the runner HOME", i, p)
			}
		}
		if seenCache[p] {
			fail("runner.shared_cache_paths[%d] %q duplicates an earlier entry", i, p)
		}
		seenCache[p] = true
	}

	// Extra address families extend the slot unit's RestrictAddressFamilies
	// allowlist; each must be a well-formed AF_ token.
	seenFamily := make(map[string]bool, len(c.Runner.ExtraAddressFamilies))
	for i, f := range c.Runner.ExtraAddressFamilies {
		if !addressFamilyRe.MatchString(f) {
			fail("runner.extra_address_families[%d] %q must be an AF_ token like AF_NETLINK", i, f)
		}
		if seenFamily[f] {
			fail("runner.extra_address_families[%d] %q duplicates an earlier entry", i, f)
		}
		seenFamily[f] = true
	}

	// Runtime backend.
	switch c.Runtime.Backend {
	case "systemd":
		if _, err := os.Stat(systemdRuntimeDir); err != nil {
			fail("runtime.backend %q requires %s (systemd), which is not present: %v", c.Runtime.Backend, systemdRuntimeDir, err)
		}
	case "process":
	default:
		fail("runtime.backend must be %q or %q (got %q)", "systemd", "process", c.Runtime.Backend)
	}

	// Paths.
	if c.Paths.StateDir == "" {
		fail("paths.state_dir must not be empty")
	}
	if c.Paths.CacheDir == "" {
		fail("paths.cache_dir must not be empty")
	}
	if c.Paths.LogDir == "" {
		fail("paths.log_dir must not be empty")
	}

	// Timeouts.
	if c.Runtime.SlotStartTimeout <= 0 {
		fail("runtime.slot_start_timeout must be > 0 (got %s)", c.Runtime.SlotStartTimeout)
	}
	if c.Runtime.SlotStopTimeout <= 0 {
		fail("runtime.slot_stop_timeout must be > 0 (got %s)", c.Runtime.SlotStopTimeout)
	}
	if c.Runtime.CleanupTimeout <= 0 {
		fail("runtime.cleanup_timeout must be > 0 (got %s)", c.Runtime.CleanupTimeout)
	}
	if c.Runtime.AcquireGrace <= 0 {
		fail("runtime.acquire_grace must be > 0 (got %s)", c.Runtime.AcquireGrace)
	}
	if c.Runtime.IdleGrace <= 0 {
		fail("runtime.idle_grace must be > 0 (got %s)", c.Runtime.IdleGrace)
	}

	// Scaling (admission gate). Checked only when the gate is enabled:
	// a zero Scaling struct means the caller built the config directly
	// (tests) and Load's defaults never ran.
	if c.Scaling.AdmissionControl {
		if c.Scaling.CPUTargetPercent <= 0 || c.Scaling.CPUTargetPercent > 100 {
			fail("scaling.cpu_target_percent must be in (0, 100] (got %d)", c.Scaling.CPUTargetPercent)
		}
		if c.Scaling.MemoryMarginPercent < 0 || c.Scaling.MemoryMarginPercent > 90 {
			fail("scaling.memory_margin_percent must be in [0, 90] (got %d)", c.Scaling.MemoryMarginPercent)
		}
		if c.Scaling.SampleInterval <= 0 {
			fail("scaling.sample_interval must be > 0 (got %s)", c.Scaling.SampleInterval)
		}
	}

	// Observability.
	if c.Observability.DiagMaxAge < 0 {
		fail("observability.diag_max_age must be positive")
	}
	if c.Observability.DiagMaxBytes < 0 {
		fail("observability.diag_max_bytes must be positive")
	}
	if _, _, err := net.SplitHostPort(c.Observability.Listen); err != nil {
		fail("observability.listen %q is not a valid host:port: %v", c.Observability.Listen, err)
	}
	switch c.Observability.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("observability.log_level must be one of debug, info, warn, error (got %q)", c.Observability.LogLevel)
	}

	return errors.Join(errs...)
}

func validatePool(index int, pool Pool, hostMax int) []error {
	errs := []error{}
	path := fmt.Sprintf("pools[%d]", index)
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(path+"."+format, args...))
	}

	if !poolIDRe.MatchString(pool.ID) {
		fail("id %q must match %s", pool.ID, poolIDRe.String())
	}
	if pool.Capacity.MinRunners < 0 {
		fail("capacity.min_runners must be >= 0 (got %d)", pool.Capacity.MinRunners)
	}
	if pool.Capacity.MaxRunners < 1 {
		fail("capacity.max_runners must be >= 1 (got %d)", pool.Capacity.MaxRunners)
	}
	if pool.Capacity.MinRunners > pool.Capacity.MaxRunners {
		fail(
			"capacity.min_runners (%d) must be <= capacity.max_runners (%d)",
			pool.Capacity.MinRunners,
			pool.Capacity.MaxRunners,
		)
	}
	if hostMax > 0 && pool.Capacity.MaxRunners > hostMax {
		fail(
			"capacity.max_runners (%d) must be <= host capacity.max_runners (%d)",
			pool.Capacity.MaxRunners,
			hostMax,
		)
	}
	if pool.GitHub.URL == "" {
		fail("github.url must not be empty")
	}
	if pool.GitHub.App.InstallationID <= 0 {
		fail("github.app.installation_id must be > 0 (got %d)", pool.GitHub.App.InstallationID)
	}
	if pool.GitHub.App.ClientID == "" {
		fail("github.app.client_id must not be empty")
	}
	if pool.GitHub.App.PrivateKeyPath == "" {
		fail("github.app.private_key_path must not be empty")
	} else if _, err := os.Stat(pool.GitHub.App.PrivateKeyPath); err != nil {
		fail("github.app.private_key_path %q is not readable: %v", pool.GitHub.App.PrivateKeyPath, err)
	}

	switch pool.GitHub.Scope.Kind {
	case "organization":
	case "repository":
		if pool.GitHub.Scope.Repository == "" {
			fail(
				"github.scope.repository must be set when scope.kind is %q",
				pool.GitHub.Scope.Kind,
			)
		}
	default:
		fail(
			"github.scope.kind must be %q or %q (got %q)",
			"organization",
			"repository",
			pool.GitHub.Scope.Kind,
		)
	}
	if pool.GitHub.Scope.Owner == "" {
		fail("github.scope.owner must not be empty")
	}
	if !labelRe.MatchString(pool.ScaleSet.Name) {
		fail(
			"scale_set.name %q is not a valid Actions label (must match %s)",
			pool.ScaleSet.Name,
			labelRe.String(),
		)
	}
	return errs
}

// PoolStateDir is the private state root for one configured pool.
func (c *Config) PoolStateDir(id string) string {
	return filepath.Join(c.Paths.StateDir, "pools", id)
}

// PoolSlotsDir is the slot-table root for one configured pool.
func (c *Config) PoolSlotsDir(id string) string {
	return filepath.Join(c.PoolStateDir(id), "slots")
}

// PoolJITDir is the root-only JIT source directory for one configured pool.
func (c *Config) PoolJITDir(id string) string {
	return filepath.Join(c.Runtime.JitDir, id)
}

// PoolLogDir is the diagnostic archive directory for one configured pool.
func (c *Config) PoolLogDir(id string) string {
	return filepath.Join(c.Paths.LogDir, "pools", id)
}

// EnsureDirs creates the shared daemon directories and isolated state, JIT,
// and diagnostic directories for every configured pool.
func (c *Config) EnsureDirs() error {
	type dirSpec struct {
		path string
		mode os.FileMode
	}
	dirs := []dirSpec{
		{path: c.Paths.StateDir, mode: 0o755},
		{path: c.Paths.CacheDir, mode: 0o755},
		{path: c.Paths.LogDir, mode: 0o755},
		{path: filepath.Join(c.Paths.StateDir, "template"), mode: 0o755},
		{path: c.Runtime.JitDir, mode: 0o700},
	}
	for _, pool := range c.Pools {
		dirs = append(
			dirs,
			dirSpec{path: c.PoolSlotsDir(pool.ID), mode: 0o755},
			dirSpec{path: c.PoolJITDir(pool.ID), mode: 0o700},
			dirSpec{path: c.PoolLogDir(pool.ID), mode: 0o755},
		)
	}
	for _, dir := range dirs {
		if dir.path == "" {
			return fmt.Errorf("cannot create directory: empty path")
		}
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			return fmt.Errorf("create directory %s: %w", dir.path, err)
		}
		if dir.mode == 0o700 {
			info, err := os.Lstat(dir.path)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("JIT directory must be a real directory: %s", dir.path)
			}
			if err := os.Chmod(dir.path, dir.mode); err != nil {
				return err
			}
		}
		if err := probeWritable(dir.path); err != nil {
			return fmt.Errorf("directory %s is not writable: %w", dir.path, err)
		}
	}
	return nil
}

// probeWritable verifies the daemon can create a file in dir.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
