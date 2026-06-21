package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

func TestDefaultConfig_CorruptedTriageDisabledWithDefaults(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	ct := cfg.Health.CorruptedTriage
	if ct.Enabled == nil || *ct.Enabled {
		t.Fatalf("triage enabled = %v; want non-nil false", ct.Enabled)
	}
	if ct.MaxDeletesPerRun != 50 {
		t.Errorf("MaxDeletesPerRun = %d; want 50", ct.MaxDeletesPerRun)
	}
	if ct.MassEventThreshold != 500 {
		t.Errorf("MassEventThreshold = %d; want 500", ct.MassEventThreshold)
	}
	if ct.BackstopIntervalMinutes != 360 {
		t.Errorf("BackstopIntervalMinutes = %d; want 360", ct.BackstopIntervalMinutes)
	}
}

func TestCorruptedTriageAccessors_Defaults(t *testing.T) {
	c := &Config{} // zero values everywhere
	if c.GetCorruptedTriageEnabled() {
		t.Errorf("GetCorruptedTriageEnabled() = true; want false (fail-safe default)")
	}
	if got := c.GetCorruptedTriageMaxDeletesPerRun(); got != 50 {
		t.Errorf("MaxDeletesPerRun default = %d; want 50", got)
	}
	if got := c.GetCorruptedTriageMassEventThreshold(); got != 500 {
		t.Errorf("MassEventThreshold default = %d; want 500", got)
	}
	if got := c.GetCorruptedTriageBackstopInterval().Minutes(); got != 360 {
		t.Errorf("BackstopInterval default = %v min; want 360", got)
	}
}

func TestCorruptedTriageAccessors_Custom(t *testing.T) {
	enabled := true
	c := &Config{}
	c.Health.CorruptedTriage = CorruptedTriageConfig{
		Enabled:                 &enabled,
		MaxDeletesPerRun:        7,
		MassEventThreshold:      99,
		BackstopIntervalMinutes: 15,
	}
	if !c.GetCorruptedTriageEnabled() {
		t.Errorf("GetCorruptedTriageEnabled() = false; want true")
	}
	if got := c.GetCorruptedTriageMaxDeletesPerRun(); got != 7 {
		t.Errorf("MaxDeletesPerRun = %d; want 7", got)
	}
	if got := c.GetCorruptedTriageMassEventThreshold(); got != 99 {
		t.Errorf("MassEventThreshold = %d; want 99", got)
	}
	if got := c.GetCorruptedTriageBackstopInterval().Minutes(); got != 15 {
		t.Errorf("BackstopInterval = %v min; want 15", got)
	}
}

func TestEnsureCorruptedTriageDefaults_FillsDefaults(t *testing.T) {
	cfg := &Config{}
	ensureCorruptedTriageDefaults(cfg)

	if cfg.Health.CorruptedTriage.Enabled == nil || *cfg.Health.CorruptedTriage.Enabled {
		t.Errorf("Enabled not defaulted to non-nil false")
	}
	if cfg.Health.CorruptedTriage.MaxDeletesPerRun != 50 {
		t.Errorf("MaxDeletesPerRun = %d; want 50", cfg.Health.CorruptedTriage.MaxDeletesPerRun)
	}
	if cfg.Health.CorruptedTriage.MassEventThreshold != 500 {
		t.Errorf("MassEventThreshold = %d; want 500", cfg.Health.CorruptedTriage.MassEventThreshold)
	}
	if cfg.Health.CorruptedTriage.BackstopIntervalMinutes != 360 {
		t.Errorf("BackstopIntervalMinutes = %d; want 360", cfg.Health.CorruptedTriage.BackstopIntervalMinutes)
	}
}

// TestLoadConfig_DoesNotRewriteExistingConfigForTriage verifies the block is NOT
// written into a pre-existing config file at load time: defaults are filled in
// memory, but the on-disk file is left untouched (the block lands on disk only
// via a normal save, like every other config value).
func TestLoadConfig_DoesNotRewriteExistingConfigForTriage(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Start from a valid default config, then strip the corrupted_triage block to
	// simulate a config written before the feature existed.
	raw, err := yaml.Marshal(DefaultConfig(dir))
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal default: %v", err)
	}
	if h, ok := m["health"].(map[string]any); ok {
		delete(h, "corrupted_triage")
	} else {
		t.Fatal("health block missing from default config")
	}
	stripped, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("marshal stripped: %v", err)
	}
	if err := os.WriteFile(path, stripped, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// In-memory defaults are present so the feature works.
	if cfg.Health.CorruptedTriage.Enabled == nil || *cfg.Health.CorruptedTriage.Enabled {
		t.Errorf("loaded triage enabled = %v; want non-nil false", cfg.Health.CorruptedTriage.Enabled)
	}
	if cfg.Health.CorruptedTriage.MaxDeletesPerRun != 50 {
		t.Errorf("in-memory MaxDeletesPerRun = %d; want 50", cfg.Health.CorruptedTriage.MaxDeletesPerRun)
	}

	// The on-disk file must be byte-for-byte unchanged: load does not rewrite it.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("LoadConfig rewrote the existing config file; want it left untouched")
	}
	if strings.Contains(string(after), "corrupted_triage") {
		t.Errorf("corrupted_triage must not be written into an existing file at load")
	}
}
