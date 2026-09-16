package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads and strict-decodes the YAML config at path, applies defaults
// for omitted fields, and normalizes values. It does not validate: call
// Validate afterwards.
//
// Strict decoding rejects unknown fields so a typo cannot silently
// disable a safety knob.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config %s: no YAML documents found", path)
		}
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	// Reject trailing documents: a second `---` document would otherwise
	// be ignored silently.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("config %s: multiple YAML documents are not allowed", path)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	// Booleans whose default is true need presence tracking: an explicit
	// `false` in YAML must win over the default. Walk the raw node tree
	// (the strict decoder above already proved the shape is legal).
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err == nil && len(root.Content) == 1 {
		applyDefaults(&c,
			mappingHasPath(root.Content[0], "runner", "disable_update"),
			mappingHasPath(root.Content[0], "observability", "ship_diag"),
			mappingHasPath(root.Content[0], "scaling", "admission_control"),
			mappingHasPath(root.Content[0], "scaling", "memory_margin_percent"),
		)
	} else {
		applyDefaults(&c, false, false, false, false)
	}
	return &c, nil
}

// applyDefaults fills omitted fields with the documented defaults.
// disableUpdateSet and shipDiagSet report whether the operator wrote the
// key explicitly (so an explicit false survives).
func applyDefaults(c *Config, disableUpdateSet, shipDiagSet, admissionSet, marginSet bool) {
	for i := range c.Pools {
		pool := &c.Pools[i]
		if pool.GitHub.URL == "" {
			pool.GitHub.URL = DefaultGitHubURL
		}
		pool.GitHub.URL = strings.TrimRight(pool.GitHub.URL, "/")
		if pool.ScaleSet.RunnerGroup == "" {
			pool.ScaleSet.RunnerGroup = DefaultRunnerGroup
		}
	}

	if c.Runner.WorkDirectory == "" {
		c.Runner.WorkDirectory = DefaultWorkDir
	}
	if c.Runner.User == "" {
		c.Runner.User = DefaultRunnerUser
	}
	if !disableUpdateSet {
		c.Runner.DisableUpdate = true
	}
	if len(c.Runner.SharedCachePaths) == 0 {
		c.Runner.SharedCachePaths = slices.Clone(DefaultSharedCachePaths)
	}

	if c.Paths.StateDir == "" {
		c.Paths.StateDir = DefaultStateDir
	}
	if c.Paths.CacheDir == "" {
		c.Paths.CacheDir = DefaultCacheDir
	}
	if c.Paths.LogDir == "" {
		c.Paths.LogDir = DefaultLogDir
	}

	if c.Runtime.Backend == "" {
		c.Runtime.Backend = DefaultBackend
	}
	if c.Runtime.JitDir == "" {
		c.Runtime.JitDir = DefaultJitDir
	}
	// A zero duration means "unset" in YAML terms; explicit zero is not
	// meaningful and is caught by Validate on the defaulted value.
	if c.Runtime.SlotStartTimeout == 0 {
		c.Runtime.SlotStartTimeout = DefaultSlotStartTimeout
	}
	if c.Runtime.SlotStopTimeout == 0 {
		c.Runtime.SlotStopTimeout = DefaultSlotStopTimeout
	}
	if c.Runtime.CleanupTimeout == 0 {
		c.Runtime.CleanupTimeout = DefaultCleanupTimeout
	}
	if c.Runtime.AcquireGrace == 0 {
		c.Runtime.AcquireGrace = DefaultAcquireGrace
	}
	if c.Runtime.IdleGrace == 0 {
		c.Runtime.IdleGrace = DefaultIdleGrace
	}

	if c.Observability.Listen == "" {
		c.Observability.Listen = DefaultListen
	}
	if c.Observability.LogLevel == "" {
		c.Observability.LogLevel = DefaultLogLevel
	}
	if !shipDiagSet {
		c.Observability.ShipDiag = true
	}

	if c.Observability.DiagMaxAge == 0 {
		c.Observability.DiagMaxAge = DefaultDiagMaxAge
	}
	if c.Observability.DiagMaxBytes == 0 {
		c.Observability.DiagMaxBytes = DefaultDiagMaxBytes
	}

	// Scaling defaults.
	if !admissionSet {
		c.Scaling.AdmissionControl = true
	}
	if c.Scaling.CPUTargetPercent == 0 {
		c.Scaling.CPUTargetPercent = DefaultCPUTargetPercent
	}
	if !marginSet {
		c.Scaling.MemoryMarginPercent = DefaultMemoryMarginPercent
	}
	if c.Scaling.SampleInterval == 0 {
		c.Scaling.SampleInterval = DefaultSampleInterval
	}
}

// mappingHasPath reports whether the mapping node contains the nested
// sequence of keys, e.g. mappingHasPath(root, "runner", "disable_update").
func mappingHasPath(node *yaml.Node, path ...string) bool {
	cur := node
	for i, key := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return false
		}
		found := false
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == key {
				if i == len(path)-1 {
					return true
				}
				cur = cur.Content[j+1]
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return false
}
