package config

import (
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

func TestEnsureCorruptedTriageDefaults_FillsAndReportsMissing(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	cfg := &Config{}
	missing := ensureCorruptedTriageDefaults(cfg)
	if !missing {
		t.Fatalf("wasMissing = false; want true (viper has no key)")
	}
	if cfg.Health.CorruptedTriage.Enabled == nil || *cfg.Health.CorruptedTriage.Enabled {
		t.Errorf("Enabled not defaulted to non-nil false")
	}
	if cfg.Health.CorruptedTriage.MaxDeletesPerRun != 50 {
		t.Errorf("MaxDeletesPerRun = %d; want 50", cfg.Health.CorruptedTriage.MaxDeletesPerRun)
	}
}

func TestEnsureCorruptedTriageDefaults_PresentNotMissing(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	viper.Set("health.corrupted_triage.enabled", true)

	cfg := &Config{}
	if missing := ensureCorruptedTriageDefaults(cfg); missing {
		t.Fatalf("wasMissing = true; want false (viper has the key)")
	}
}

// TestLoadConfig_InjectsTriageBlockIntoExistingConfig verifies the first-run
// injection: an existing config that predates the block gets it added (disabled)
// and persisted back to disk.
func TestLoadConfig_InjectsTriageBlockIntoExistingConfig(t *testing.T) {
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
	if strings.Contains(string(stripped), "corrupted_triage") {
		t.Fatal("precondition failed: stripped config still contains corrupted_triage")
	}
	if err := os.WriteFile(path, stripped, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// The in-memory config has the block, disabled.
	if cfg.Health.CorruptedTriage.Enabled == nil || *cfg.Health.CorruptedTriage.Enabled {
		t.Errorf("loaded triage enabled = %v; want non-nil false", cfg.Health.CorruptedTriage.Enabled)
	}

	// The block was persisted back to disk, surfacing the option.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if !strings.Contains(string(after), "corrupted_triage") {
		t.Errorf("config file was not updated with corrupted_triage block:\n%s", after)
	}
}
