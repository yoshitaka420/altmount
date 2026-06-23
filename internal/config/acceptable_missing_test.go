package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// TestAcceptableMissing_SerializedAtZeroForUI guards the exposure fix: the JSON
// tag must NOT use omitempty, so the API response always carries the field even
// at the 0 default. The frontend slider renders only when the value is defined
// (`!== undefined`); with omitempty, 0 was dropped and the slider was hidden,
// making the knob uneditable from the UI until set via config.yaml.
func TestAcceptableMissing_SerializedAtZeroForUI(t *testing.T) {
	cfg := DefaultConfig(t.TempDir()) // value == 0
	b, err := json.Marshal(cfg.Health)
	if err != nil {
		t.Fatalf("marshal health: %v", err)
	}
	if !strings.Contains(string(b), `"acceptable_missing_segments_percentage"`) {
		t.Errorf("API JSON omits acceptable_missing_segments_percentage at 0; the UI slider would be hidden. JSON: %s", b)
	}
}

// acceptable_missing_test.go proves the config side of the chain:
// yaml on disk → viper.Unmarshal → Config.Health.AcceptableMissingSegmentsPercentage
// → GetAcceptableMissingSegmentsPercentage(). The checker side (accessor → condemnation)
// is covered in internal/health/checker_tolerance_test.go.

func TestAcceptableMissing_DefaultIsZero(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	if cfg.Health.AcceptableMissingSegmentsPercentage != 0 {
		t.Errorf("default field = %v; want 0", cfg.Health.AcceptableMissingSegmentsPercentage)
	}
	if got := cfg.GetAcceptableMissingSegmentsPercentage(); got != 0 {
		t.Errorf("default accessor = %v; want 0 (zero tolerance, unchanged for existing users)", got)
	}
}

// TestAcceptableMissing_LoadedFromYAML is the end-to-end config proof: a value set
// in config.yaml is actually read back through viper and surfaced by the accessor.
func TestAcceptableMissing_LoadedFromYAML(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Start from a valid default config and set the tolerance to 3 in the health block.
	raw, err := yaml.Marshal(DefaultConfig(dir))
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal default: %v", err)
	}
	h, ok := m["health"].(map[string]any)
	if !ok {
		t.Fatal("health block missing from default config")
	}
	h["acceptable_missing_segments_percentage"] = 3
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("marshal modified: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Health.AcceptableMissingSegmentsPercentage != 3 {
		t.Errorf("loaded field = %v; want 3", cfg.Health.AcceptableMissingSegmentsPercentage)
	}
	if got := cfg.GetAcceptableMissingSegmentsPercentage(); got != 3 {
		t.Errorf("loaded accessor = %v; want 3 (config.yaml value must flow through viper to the accessor)", got)
	}
}

func TestAcceptableMissing_AccessorClamps(t *testing.T) {
	c := &Config{}
	c.Health.AcceptableMissingSegmentsPercentage = 150
	if got := c.GetAcceptableMissingSegmentsPercentage(); got != 100 {
		t.Errorf("clamp high = %v; want 100", got)
	}
	c.Health.AcceptableMissingSegmentsPercentage = -5
	if got := c.GetAcceptableMissingSegmentsPercentage(); got != 0 {
		t.Errorf("clamp low = %v; want 0", got)
	}
}
