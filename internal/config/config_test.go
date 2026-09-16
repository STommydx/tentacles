package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseValid returns a fully valid Config. Every path that Validate
// touches (private key, environment file) lives under t.TempDir.
func baseValid(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	pem := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(pem, []byte("-----BEGIN RSA PRIVATE KEY-----\ndummy\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "runner.env")
	if err := os.WriteFile(env, []byte("PATH=/usr/bin:/bin\nHOME=/home/gha-runner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Config{
		Pools: []Pool{{
			ID: "my-org",
			GitHub: GitHub{
				URL:   "https://github.com",
				App:   App{ClientID: "Iv1.test", InstallationID: 42, PrivateKeyPath: pem},
				Scope: Scope{Kind: "organization", Owner: "my-org"},
			},
			ScaleSet: ScaleSet{Name: "debian-host", RunnerGroup: "Default"},
			Capacity: PoolCapacity{MinRunners: 0, MaxRunners: 4},
		}},
		Capacity: Capacity{
			MaxRunners:         4,
			JobCPUQuotaPercent: 400,
			JobMemoryMax:       "8G",
		},
		Runner: Runner{
			Version:         "2.328.0",
			SHA256:          strings.Repeat("ab", 32),
			WorkDirectory:   "_work",
			DisableUpdate:   true,
			User:            "gha-runner",
			EnvironmentFile: env,
		},
		Paths: Paths{
			StateDir: filepath.Join(dir, "state"),
			CacheDir: filepath.Join(dir, "cache"),
			LogDir:   filepath.Join(dir, "log"),
		},
		Runtime: Runtime{
			Backend:          "process",
			JitDir:           filepath.Join(dir, "run"),
			SlotStartTimeout: 90 * time.Second,
			SlotStopTimeout:  30 * time.Second,
			CleanupTimeout:   60 * time.Second,
			AcquireGrace:     3 * time.Minute,
			IdleGrace:        30 * time.Second,
		},
		Observability: Observability{Listen: "127.0.0.1:9090", LogLevel: "info", ShipDiag: true},
	}
}

// writeConfig writes body to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate(t *testing.T) {
	systemdDirPresent := func(t *testing.T) {
		t.Helper()
		old := systemdRuntimeDir
		systemdRuntimeDir = t.TempDir() // exists
		t.Cleanup(func() { systemdRuntimeDir = old })
	}
	systemdDirAbsent := func(t *testing.T) {
		t.Helper()
		old := systemdRuntimeDir
		systemdRuntimeDir = filepath.Join(t.TempDir(), "does-not-exist")
		t.Cleanup(func() { systemdRuntimeDir = old })
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		setup   func(*testing.T)
		wantErr string // substring of the joined error; "" means want nil
	}{
		{"valid", func(*Config) {}, nil, ""},
		{"max_runners at hard cap", func(c *Config) {
			c.Capacity.MaxRunners = HardCapMaxRunners
			c.Pools[0].Capacity.MaxRunners = HardCapMaxRunners
		}, nil, ""},
		{"repository scope with repository", func(c *Config) {
			c.Pools[0].GitHub.Scope.Kind = "repository"
			c.Pools[0].GitHub.Scope.Repository = "my-repo"
		}, nil, ""},
		{"systemd backend with runtime dir", func(c *Config) { c.Runtime.Backend = "systemd" }, systemdDirPresent, ""},

		{"max_runners zero", func(c *Config) { c.Capacity.MaxRunners = 0 }, nil, "capacity.max_runners must be >= 1"},
		{"pool min above max", func(c *Config) { c.Pools[0].Capacity.MinRunners = 5 }, nil, "capacity.min_runners (5) must be <= capacity.max_runners (4)"},
		{"max_runners over hard cap", func(c *Config) { c.Capacity.MaxRunners = HardCapMaxRunners + 1 }, nil, "hard cap of 32"},
		{"pool max above host", func(c *Config) { c.Pools[0].Capacity.MaxRunners = 5 }, nil, "must be <= host capacity.max_runners"},
		{"cpu quota zero", func(c *Config) { c.Capacity.JobCPUQuotaPercent = 0 }, nil, "job_cpu_quota_percent"},
		{"cpu quota negative", func(c *Config) { c.Capacity.JobCPUQuotaPercent = -400 }, nil, "job_cpu_quota_percent"},
		{"cpu quota over ceiling", func(c *Config) { c.Capacity.JobCPUQuotaPercent = MaxCPUQuotaPercent + 1 }, nil, "job_cpu_quota_percent"},
		{"memory max empty", func(c *Config) { c.Capacity.JobMemoryMax = "" }, nil, "job_memory_max"},
		{"installation id zero", func(c *Config) { c.Pools[0].GitHub.App.InstallationID = 0 }, nil, "installation_id must be > 0"},
		{"installation id negative", func(c *Config) { c.Pools[0].GitHub.App.InstallationID = -1 }, nil, "installation_id must be > 0"},
		{"client id empty", func(c *Config) { c.Pools[0].GitHub.App.ClientID = "" }, nil, "client_id must not be empty"},
		{"private key path empty", func(c *Config) { c.Pools[0].GitHub.App.PrivateKeyPath = "" }, nil, "private_key_path must not be empty"},
		{"private key missing", func(c *Config) { c.Pools[0].GitHub.App.PrivateKeyPath = filepath.Join(c.Paths.StateDir, "nope.pem") }, nil, "private_key_path"},
		{"scope kind invalid", func(c *Config) { c.Pools[0].GitHub.Scope.Kind = "enterprise" }, nil, `scope.kind must be "organization" or "repository"`},
		{"scope kind empty", func(c *Config) { c.Pools[0].GitHub.Scope.Kind = "" }, nil, "scope.kind"},
		{"repository kind without repository", func(c *Config) { c.Pools[0].GitHub.Scope.Kind = "repository" }, nil, "repository must be set"},
		{"owner empty", func(c *Config) { c.Pools[0].GitHub.Scope.Owner = "" }, nil, "owner must not be empty"},
		{"label with space", func(c *Config) { c.Pools[0].ScaleSet.Name = "bad name" }, nil, "scale_set.name"},
		{"label leading dash", func(c *Config) { c.Pools[0].ScaleSet.Name = "-bad" }, nil, "scale_set.name"},
		{"label with slash", func(c *Config) { c.Pools[0].ScaleSet.Name = "bad/name" }, nil, "scale_set.name"},
		{"version empty with sha pinned", func(c *Config) { c.Runner.Version = "" }, nil, "runner.sha256 cannot be pinned"},
		{"version two parts", func(c *Config) { c.Runner.Version = "2.328" }, nil, "runner.version"},
		{"version prefixed", func(c *Config) { c.Runner.Version = "v2.328.0" }, nil, "runner.version"},
		{"version non-numeric", func(c *Config) { c.Runner.Version = "latest" }, nil, "runner.version"},
		{"sha256 empty", func(c *Config) { c.Runner.SHA256 = "" }, nil, "runner.sha256"},
		{"sha256 too short", func(c *Config) { c.Runner.SHA256 = strings.Repeat("ab", 20) }, nil, "runner.sha256"},
		{"sha256 not hex", func(c *Config) { c.Runner.SHA256 = strings.Repeat("zz", 32) }, nil, "runner.sha256"},
		{"sha256 uppercase hex", func(c *Config) { c.Runner.SHA256 = strings.Repeat("AB", 32) }, nil, ""},
		{"environment file empty", func(c *Config) { c.Runner.EnvironmentFile = "" }, nil, "environment_file must not be empty"},
		{"environment file missing", func(c *Config) { c.Runner.EnvironmentFile = filepath.Join(c.Paths.StateDir, "nope.env") }, nil, "environment_file"},
		{"backend invalid", func(c *Config) { c.Runtime.Backend = "docker" }, nil, `runtime.backend must be "systemd" or "process"`},
		{"backend empty", func(c *Config) { c.Runtime.Backend = "" }, nil, "runtime.backend"},
		{"systemd backend without runtime dir", func(c *Config) { c.Runtime.Backend = "systemd" }, systemdDirAbsent, "runtime.backend"},
		{"state dir empty", func(c *Config) { c.Paths.StateDir = "" }, nil, "state_dir must not be empty"},
		{"cache dir empty", func(c *Config) { c.Paths.CacheDir = "" }, nil, "cache_dir must not be empty"},
		{"log dir empty", func(c *Config) { c.Paths.LogDir = "" }, nil, "log_dir must not be empty"},
		{"start timeout zero", func(c *Config) { c.Runtime.SlotStartTimeout = 0 }, nil, "slot_start_timeout"},
		{"stop timeout zero", func(c *Config) { c.Runtime.SlotStopTimeout = 0 }, nil, "slot_stop_timeout"},
		{"cleanup timeout zero", func(c *Config) { c.Runtime.CleanupTimeout = 0 }, nil, "cleanup_timeout"},
		{"acquire grace zero", func(c *Config) { c.Runtime.AcquireGrace = 0 }, nil, "acquire_grace"},
		{"idle grace zero", func(c *Config) { c.Runtime.IdleGrace = 0 }, nil, "idle_grace"},
		{"listen not host:port", func(c *Config) { c.Observability.Listen = "localhost" }, nil, "observability.listen"},
		{"listen empty", func(c *Config) { c.Observability.Listen = "" }, nil, "observability.listen"},
		{"log level invalid", func(c *Config) { c.Observability.LogLevel = "verbose" }, nil, "log_level"},
		{"log level empty", func(c *Config) { c.Observability.LogLevel = "" }, nil, "log_level"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			c := baseValid(t)
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidateJoinsAllErrors proves Validate reports every failure at
// once (errors.Join), not just the first one.
func TestValidateJoinsAllErrors(t *testing.T) {
	c := &Config{} // nothing set, nothing valid
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want errors for a zero Config")
	}
	for _, want := range []string{
		"max_runners", "pools", "environment_file", "backend",
		"state_dir", "cache_dir", "log_dir", "job_cpu_quota_percent",
		"job_memory_max", "slot_start_timeout", "slot_stop_timeout",
		"cleanup_timeout", "acquire_grace", "idle_grace", "observability.listen", "log_level",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q:\n%v", want, err)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	body := `pools:
  - id: my-org
    github:
      app:
        installation_id: 7
      scope:
        kind: organization
        owner: my-org
    scale_set:
      name: debian-host
runner:
  version: 2.328.0
`
	c, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	pool := c.Pools[0]
	if pool.GitHub.URL != "https://github.com" {
		t.Errorf("Pool.GitHub.URL = %q, want default https://github.com", pool.GitHub.URL)
	}
	if pool.ScaleSet.RunnerGroup != "Default" {
		t.Errorf("Pool.ScaleSet.RunnerGroup = %q, want Default", pool.ScaleSet.RunnerGroup)
	}
	if pool.Capacity.MinRunners != 0 || pool.Capacity.MaxRunners != 0 {
		t.Errorf("Pool.Capacity = %+v, want zeros", pool.Capacity)
	}
	if c.Runner.WorkDirectory != "_work" {
		t.Errorf("Runner.WorkDirectory = %q, want _work", c.Runner.WorkDirectory)
	}
	if c.Runner.User != "gha-runner" {
		t.Errorf("Runner.User = %q, want gha-runner", c.Runner.User)
	}
	if !c.Runner.DisableUpdate {
		t.Error("Runner.DisableUpdate = false, want default true")
	}
	if c.Paths.StateDir != "/var/lib/tentacles" || c.Paths.CacheDir != "/var/cache/tentacles" || c.Paths.LogDir != "/var/log/tentacles" {
		t.Errorf("Paths = %+v, want production directory defaults", c.Paths)
	}
	if c.Runtime.Backend != "systemd" {
		t.Errorf("Runtime.Backend = %q, want default systemd", c.Runtime.Backend)
	}
	if c.Runtime.JitDir != "/run/tentacles" {
		t.Errorf("Runtime.JitDir = %q, want /run/tentacles", c.Runtime.JitDir)
	}
	if c.Runtime.SlotStartTimeout != 90*time.Second || c.Runtime.SlotStopTimeout != 30*time.Second ||
		c.Runtime.CleanupTimeout != 60*time.Second || c.Runtime.AcquireGrace != 3*time.Minute ||
		c.Runtime.IdleGrace != 30*time.Second {
		t.Errorf("Runtime timeouts = %+v, want 90s/30s/60s/3m/30s", c.Runtime)
	}
	if c.Observability.Listen != "127.0.0.1:9090" || c.Observability.LogLevel != "info" || !c.Observability.ShipDiag {
		t.Errorf("Observability = %+v, want 127.0.0.1:9090/info/true", c.Observability)
	}
}

func TestLoadOverridesAndNormalization(t *testing.T) {
	body := `pools:
  - id: my-org
    github:
      url: https://github.com/
      app:
        client_id: Iv1.abc
        installation_id: 42
        private_key_path: /tmp/app.pem
      scope:
        kind: repository
        owner: my-org
        repository: my-repo
    scale_set:
      name: debian-host
      runner_group: Custom
      extra_labels: [foo, bar]
    capacity:
      min_runners: 1
      max_runners: 8
capacity:
  max_runners: 8
  job_cpu_quota_percent: 200
  job_memory_max: 16G
runner:
  version: 2.328.0
  sha256: ` + strings.Repeat("ab", 32) + `
  work_directory: work
  disable_update: false
  user: some-user
  group: some-group
  environment_file: /tmp/runner.env
paths:
  state_dir: /tmp/state
  cache_dir: /tmp/cache
  log_dir: /tmp/log
runtime:
  backend: process
  slot_start_timeout: 90s
  slot_stop_timeout: 30s
  cleanup_timeout: 1m
  acquire_grace: 3m
  idle_grace: 45s
  jit_dir: /tmp/jit
observability:
  listen: 0.0.0.0:9091
  log_level: debug
  ship_diag: false
`
	c, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	pool := c.Pools[0]
	if pool.GitHub.URL != "https://github.com" {
		t.Errorf("Pool.GitHub.URL = %q, want trailing slash trimmed", pool.GitHub.URL)
	}
	if pool.GitHub.App.ClientID != "Iv1.abc" || pool.GitHub.App.InstallationID != 42 || pool.GitHub.App.PrivateKeyPath != "/tmp/app.pem" {
		t.Errorf("Pool.GitHub.App = %+v", pool.GitHub.App)
	}
	if pool.GitHub.Scope.Kind != "repository" || pool.GitHub.Scope.Owner != "my-org" || pool.GitHub.Scope.Repository != "my-repo" {
		t.Errorf("Pool.GitHub.Scope = %+v", pool.GitHub.Scope)
	}
	if pool.ScaleSet.RunnerGroup != "Custom" || len(pool.ScaleSet.ExtraLabels) != 2 {
		t.Errorf("Pool.ScaleSet = %+v", pool.ScaleSet)
	}
	if pool.Capacity.MinRunners != 1 || pool.Capacity.MaxRunners != 8 || c.Capacity.MaxRunners != 8 || c.Capacity.JobCPUQuotaPercent != 200 || c.Capacity.JobMemoryMax != "16G" {
		t.Errorf("Pool.Capacity = %+v; host Capacity = %+v", pool.Capacity, c.Capacity)
	}
	if c.Runner.WorkDirectory != "work" || c.Runner.DisableUpdate || c.Runner.User != "some-user" || c.Runner.Group != "some-group" {
		t.Errorf("Runner = %+v", c.Runner)
	}
	if c.Paths.StateDir != "/tmp/state" || c.Paths.CacheDir != "/tmp/cache" || c.Paths.LogDir != "/tmp/log" {
		t.Errorf("Paths = %+v", c.Paths)
	}
	if c.Runtime.Backend != "process" || c.Runtime.JitDir != "/tmp/jit" {
		t.Errorf("Runtime = %+v", c.Runtime)
	}
	if c.Runtime.SlotStartTimeout != 90*time.Second || c.Runtime.SlotStopTimeout != 30*time.Second ||
		c.Runtime.CleanupTimeout != time.Minute || c.Runtime.AcquireGrace != 3*time.Minute ||
		c.Runtime.IdleGrace != 45*time.Second {
		t.Errorf("Runtime timeouts = %+v, want 90s/30s/1m/3m/45s", c.Runtime)
	}
	if c.Observability.Listen != "0.0.0.0:9091" || c.Observability.LogLevel != "debug" || c.Observability.ShipDiag {
		t.Errorf("Observability = %+v, want explicit false preserved", c.Observability)
	}
}

func TestLoadStrictDecode(t *testing.T) {
	base := `pools:
  - id: my-org
    github:
      app:
        installation_id: 7
      scope:
        kind: organization
        owner: my-org
    scale_set:
      name: debian-host
runner:
  version: 2.328.0
`
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"unknown top-level field", base + "wat: 1\n", "field wat not found"},
		{"unknown field under pool", "pools:\n  - id: x\n    wat: 1\n", "field wat not found"},
		{"unknown field under github", "pools:\n  - id: x\n    github:\n      wat: 1\n", "field wat not found"},
		{"unknown field under capacity", "capacity:\n  wat: 1\n", "field wat not found"},
		{"unknown field under scale_set", "pools:\n  - id: x\n    scale_set:\n      wat: 1\n", "field wat not found"},
		{"unknown field under runner", "runner:\n  wat: 1\n", "field wat not found"},
		{"multiple documents", "pools: []\n---\npools: []\n", "multiple YAML documents"},
		{"empty file", "", "no YAML documents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("Load() = nil, want error for missing file")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Fatalf("Load() error = %q, want read failure context", err)
	}
}

func TestValidateSha256EnvEscape(t *testing.T) {
	t.Run("allow unverified payload", func(t *testing.T) {
		t.Setenv(AllowUnverifiedPayloadEnv, "1")
		c := baseValid(t)
		c.Runner.SHA256 = ""
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() with %s=1 and empty sha256 = %v, want nil", AllowUnverifiedPayloadEnv, err)
		}
	})
	t.Run("non-1 value does not escape", func(t *testing.T) {
		t.Setenv(AllowUnverifiedPayloadEnv, "0")
		c := baseValid(t)
		c.Runner.SHA256 = ""
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("Validate() with %s=0 = %v, want sha256 error", AllowUnverifiedPayloadEnv, err)
		}
	})
}

func TestEnsureDirs(t *testing.T) {
	dir := t.TempDir()
	c := &Config{
		Pools: []Pool{{ID: "org-a"}},
		Paths: Paths{
			StateDir: filepath.Join(dir, "state"),
			CacheDir: filepath.Join(dir, "cache"),
			LogDir:   filepath.Join(dir, "log"),
		},
		Runtime: Runtime{JitDir: filepath.Join(dir, "run")},
	}
	if err := c.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() = %v", err)
	}
	want := []string{
		filepath.Join(dir, "state"),
		filepath.Join(dir, "cache"),
		filepath.Join(dir, "log"),
		filepath.Join(dir, "state", "template"),
		filepath.Join(dir, "state", "pools", "org-a", "slots"),
		filepath.Join(dir, "run"),
		filepath.Join(dir, "run", "org-a"),
		filepath.Join(dir, "log", "pools", "org-a"),
	}
	for _, d := range want {
		fi, err := os.Stat(d)
		if err != nil {
			t.Errorf("EnsureDirs() did not create %s: %v", d, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s exists but is not a directory", d)
		}
	}
}

func TestEnsureDirsEmptyPath(t *testing.T) {
	if err := (&Config{}).EnsureDirs(); err == nil {
		t.Fatal("EnsureDirs() on zero Config = nil, want error for empty path")
	}
}

// TestExampleConfigRoundTrips guarantees the shipped example config
// Loads and passes Validate from the repo root — the exact path --dry-run
// walks. The example pins the runner version and its verified digest.
func TestExampleConfigRoundTrips(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root) // the example's paths are repo-relative
	t.Setenv(AllowUnverifiedPayloadEnv, "1")

	c, err := Load("configs/config.example.yaml")
	if err != nil {
		t.Fatalf("Load(example) = %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(example) = %v, want nil (--dry-run must pass)", err)
	}
	if len(c.Pools) != 2 {
		t.Fatalf("example pools = %d, want two organizations", len(c.Pools))
	}
	if c.Runtime.Backend != "process" {
		t.Errorf("example backend = %q, want process for local dry-run", c.Runtime.Backend)
	}
	if c.Capacity.MaxRunners != 4 {
		t.Errorf("example max_runners = %d, want 4", c.Capacity.MaxRunners)
	}
	// The referenced files must exist next to the example.
	if _, err := os.Stat(c.Pools[0].GitHub.App.PrivateKeyPath); err != nil {
		t.Errorf("example private_key_path %q missing: %v", c.Pools[0].GitHub.App.PrivateKeyPath, err)
	}
	if _, err := os.Stat(c.Runner.EnvironmentFile); err != nil {
		t.Errorf("example environment_file %q missing: %v", c.Runner.EnvironmentFile, err)
	}
}

func TestHardCap(t *testing.T) {
	if HardCapMaxRunners != 32 {
		t.Fatalf("HardCapMaxRunners = %d, want 32", HardCapMaxRunners)
	}
}

// TestValidateDynamicVersion: an unset runner.version means "track the
// latest release"; the daemon resolves version and digest at startup.
func TestValidateDynamicVersion(t *testing.T) {
	c := baseValid(t)
	c.Runner.Version = ""
	c.Runner.SHA256 = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with unset version/sha = %v, want ok", err)
	}
}

// TestValidateShaWithoutVersionRejected: a digest cannot pin a version
// that moves with every release.
func TestValidateShaWithoutVersionRejected(t *testing.T) {
	c := baseValid(t)
	c.Runner.Version = ""
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "runner.sha256") {
		t.Fatalf("Validate = %v, want sha256-without-version error", err)
	}
}

// TestEnsureDirsRejectsUnwritableDir checks that an existing read-only
// directory fails the writability probe even though MkdirAll succeeds.
func TestEnsureDirsRejectsUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure does not apply to root")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0o755) // let t.TempDir clean up
	c := &Config{
		Paths:   Paths{StateDir: blocked, CacheDir: filepath.Join(dir, "cache"), LogDir: filepath.Join(dir, "log")},
		Runtime: Runtime{JitDir: filepath.Join(dir, "run")},
	}
	err := c.EnsureDirs()
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("EnsureDirs() = %v, want not-writable error", err)
	}
}

// TestValidateRejectsEnvFileMissingRequiredVars checks that the environment
// file carries PATH and HOME so runners can find the host toolchain.
func TestValidateRejectsEnvFileMissingRequiredVars(t *testing.T) {
	c := baseValid(t)
	envFile := filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(envFile, []byte("LANG=C.UTF-8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Runner.EnvironmentFile = envFile
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate: expected error for env file without PATH/HOME")
	}
	for _, want := range []string{"PATH", "HOME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate error %q missing %q", err, want)
		}
	}
}

// TestValidateRejectsMalformedEnvFile: a broken KEY=VALUE line fails at
// validation time, not at the first slot start.
func TestValidateRejectsMalformedEnvFile(t *testing.T) {
	c := baseValid(t)
	envFile := filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(envFile, []byte("this line has no equals\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Runner.EnvironmentFile = envFile
	if err := c.Validate(); err == nil {
		t.Fatal("Validate: expected error for malformed env file")
	}
}

// TestLoadScalingDefaults: the scaling section is optional; omitted
// fields fall back to the admission-gate defaults and an explicit
// admission_control: false wins over the default true.
func TestLoadScalingDefaults(t *testing.T) {
	t.Setenv("TENTACLES_ALLOW_UNVERIFIED_PAYLOAD", "1")
	base := writeConfig(t, `
pools:
  - id: o
    github:
      app:
        client_id: Iv1.test
        installation_id: 1
        private_key_path: /nope.pem
      scope:
        kind: organization
        owner: o
    scale_set:
      name: label
    capacity:
      max_runners: 1
capacity:
  max_runners: 1
  job_cpu_quota_percent: 100
  job_memory_max: 1G
runner:
  version: ""
  environment_file: /nope.env
`)
	c, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Scaling.AdmissionControl {
		t.Error("admission_control default = false, want true")
	}
	if c.Scaling.CPUTargetPercent != DefaultCPUTargetPercent {
		t.Errorf("cpu target = %d, want %d", c.Scaling.CPUTargetPercent, DefaultCPUTargetPercent)
	}
	if c.Scaling.MemoryMarginPercent != DefaultMemoryMarginPercent {
		t.Errorf("memory margin = %d, want %d", c.Scaling.MemoryMarginPercent, DefaultMemoryMarginPercent)
	}
	if c.Scaling.SampleInterval != DefaultSampleInterval {
		t.Errorf("sample interval = %s, want %s", c.Scaling.SampleInterval, DefaultSampleInterval)
	}

	override := writeConfig(t, `
pools:
  - id: o
    github:
      app:
        client_id: Iv1.test
        installation_id: 1
        private_key_path: /nope.pem
      scope:
        kind: organization
        owner: o
    scale_set:
      name: label
    capacity:
      max_runners: 1
capacity:
  max_runners: 1
  job_cpu_quota_percent: 100
  job_memory_max: 1G
runner:
  version: ""
  environment_file: /nope.env
scaling:
  admission_control: false
`)
	c2, err := Load(override)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Scaling.AdmissionControl {
		t.Error("explicit admission_control: false was overridden by the default")
	}
}

func TestEnsureDirsProtectsJITDirectory(t *testing.T) {
	root := t.TempDir()
	c := Config{Pools: []Pool{{ID: "org-a"}}, Paths: Paths{StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "cache"), LogDir: filepath.Join(root, "logs")}, Runtime: Runtime{JitDir: filepath.Join(root, "jit")}}
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(c.Runtime.JitDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Fatalf("JIT directory mode %o, want 700", got)
	}
}

func TestValidateMultiplePools(t *testing.T) {
	addSecondPool := func(config *Config) {
		second := config.Pools[0]
		second.ID = "org-b"
		second.GitHub.App.InstallationID = 43
		second.GitHub.Scope.Owner = "org-b"
		config.Pools = append(config.Pools, second)
		config.Capacity.MaxRunners = 8
	}

	t.Run("distinct pools", func(t *testing.T) {
		config := baseValid(t)
		addSecondPool(config)
		if err := config.Validate(); err != nil {
			t.Fatalf("Validate() = %v", err)
		}
	})
	t.Run("duplicate id", func(t *testing.T) {
		config := baseValid(t)
		addSecondPool(config)
		config.Pools[1].ID = config.Pools[0].ID
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates") {
			t.Fatalf("Validate() = %v, want duplicate pool ID", err)
		}
	})
	t.Run("duplicate scale set target", func(t *testing.T) {
		config := baseValid(t)
		addSecondPool(config)
		config.Pools[1].GitHub.Scope.Owner = config.Pools[0].GitHub.Scope.Owner
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "scale-set target") {
			t.Fatalf("Validate() = %v, want duplicate target", err)
		}
	})
	t.Run("minimums exceed host", func(t *testing.T) {
		config := baseValid(t)
		addSecondPool(config)
		config.Capacity.MaxRunners = 4
		config.Pools[0].Capacity.MinRunners = 3
		config.Pools[1].Capacity.MinRunners = 2
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "sum of pools") {
			t.Fatalf("Validate() = %v, want host minimum overflow", err)
		}
	})
	t.Run("unsafe id", func(t *testing.T) {
		config := baseValid(t)
		config.Pools[0].ID = "../org"
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "must match") {
			t.Fatalf("Validate() = %v, want unsafe ID rejection", err)
		}
	})
}
