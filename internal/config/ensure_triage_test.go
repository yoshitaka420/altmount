package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func parseHealth(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &root), "result must be valid YAML")
	h, _ := root["health"].(map[string]any)
	return h
}

// A config that has a health block but NOT corrupted_triage gets the block
// injected with enabled:false, and every other setting/comment is preserved.
func TestEnsureTriage_InjectsWhenMissing(t *testing.T) {
	src := `# my altmount config
webdav:
  port: 8080 # keep me

health:
  enabled: true # health on
  library_dir: '/mnt/lib'
  max_retries: 3

mount_path: '/mnt/altmount'
`
	path := writeTempConfig(t, src)

	injected, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.True(t, injected, "block should have been injected")

	h := parseHealth(t, path)
	require.NotNil(t, h)

	// New block present and DISABLED by default.
	ct, ok := h["corrupted_triage"].(map[string]any)
	require.True(t, ok, "corrupted_triage must exist")
	assert.Equal(t, false, ct["enabled"], "must be injected disabled")
	assert.Equal(t, 50, ct["max_deletes_per_run"])
	assert.Equal(t, 500, ct["mass_event_threshold"])
	assert.Equal(t, 60, ct["backstop_interval_minutes"])

	// Existing settings preserved.
	assert.Equal(t, true, h["enabled"])
	assert.Equal(t, "/mnt/lib", h["library_dir"])
	assert.Equal(t, 3, h["max_retries"])

	raw, _ := os.ReadFile(path)
	text := string(raw)
	assert.Contains(t, text, "port: 8080 # keep me", "unrelated comments preserved")
	assert.Contains(t, text, "mount_path: '/mnt/altmount'", "unrelated keys preserved")
	assert.Contains(t, text, "# Corrupted-file auto-delete triage", "safety comments injected")
	assert.Contains(t, strings.ToLower(text), "soft-delete", "soft-delete comment present (case-insensitive)")
	assert.Contains(t, strings.ToLower(text), "fails closed", "fail-closed comment present")
}

// A config that already has corrupted_triage (enabled:true, tuned thresholds)
// is left byte-for-byte untouched.
func TestEnsureTriage_LeavesExistingUntouched(t *testing.T) {
	src := `health:
  enabled: true
  corrupted_triage:
    enabled: true # USER TURNED IT ON
    max_deletes_per_run: 7
    mass_event_threshold: 12345
    backstop_interval_minutes: 5
`
	path := writeTempConfig(t, src)
	before, _ := os.ReadFile(path)

	injected, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.False(t, injected, "must NOT inject when block already present")

	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after), "file must be byte-for-byte unchanged")

	// And the user's values are intact.
	h := parseHealth(t, path)
	ct := h["corrupted_triage"].(map[string]any)
	assert.Equal(t, true, ct["enabled"], "user's enabled:true preserved")
	assert.Equal(t, 7, ct["max_deletes_per_run"])
	assert.Equal(t, 12345, ct["mass_event_threshold"])
}

// Idempotency: a second pass after injection is a no-op.
func TestEnsureTriage_Idempotent(t *testing.T) {
	src := "health:\n  enabled: false\n"
	path := writeTempConfig(t, src)

	injected1, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.True(t, injected1)
	afterFirst, _ := os.ReadFile(path)

	injected2, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.False(t, injected2, "second pass must be a no-op")
	afterSecond, _ := os.ReadFile(path)
	assert.Equal(t, string(afterFirst), string(afterSecond))
}

// When there's no health section at all, one is appended with the block.
func TestEnsureTriage_AppendsHealthWhenAbsent(t *testing.T) {
	src := "webdav:\n  port: 8080\n"
	path := writeTempConfig(t, src)

	injected, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.True(t, injected)

	h := parseHealth(t, path)
	require.NotNil(t, h, "a health section should now exist")
	ct := h["corrupted_triage"].(map[string]any)
	assert.Equal(t, false, ct["enabled"])
}

// Child indentation is detected so the result is valid even with 4-space indent.
func TestEnsureTriage_PreservesIndentation(t *testing.T) {
	src := "health:\n    enabled: true\n    max_retries: 3\n"
	path := writeTempConfig(t, src)

	injected, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.True(t, injected)

	// Must still be valid YAML with the nested block readable.
	h := parseHealth(t, path)
	require.NotNil(t, h)
	ct, ok := h["corrupted_triage"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, ct["enabled"])

	raw, _ := os.ReadFile(path)
	assert.Contains(t, string(raw), "    corrupted_triage:", "key aligned to 4-space children")
}

// A scalar `health` value (not a mapping) is never touched — we can't add child
// keys under it, so the file is left exactly as-is.
func TestEnsureTriage_SkipsScalarHealth(t *testing.T) {
	src := "health: true\nmount_path: '/mnt/x'\n"
	path := writeTempConfig(t, src)
	before, _ := os.ReadFile(path)

	injected, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.False(t, injected, "must not inject under a scalar health value")
	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after), "scalar-health file must be left untouched")
}

// `health:` with only a trailing comment (no children) parses as null, not a
// mapping, so it is left untouched.
func TestEnsureTriage_SkipsHealthWithComment(t *testing.T) {
	src := "health: # comment\nmount_path: '/mnt/x'\n"
	path := writeTempConfig(t, src)
	before, _ := os.ReadFile(path)

	injected, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.False(t, injected, "must not inject under a commented/null health")
	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after))
}

// An inline flow mapping (`health: { ... }`) cannot take block children, so the
// file is left untouched rather than getting a duplicate health block appended.
func TestEnsureTriage_SkipsHealthWithFlowMapping(t *testing.T) {
	src := "health: { enabled: true }\nmount_path: '/mnt/x'\n"
	path := writeTempConfig(t, src)
	before, _ := os.ReadFile(path)

	injected, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.False(t, injected, "must not inject under an inline flow mapping")
	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after))
}

// A file we cannot parse is never rewritten.
func TestEnsureTriage_SkipsUnparseableFile(t *testing.T) {
	src := "this: : : not valid yaml\n  - broken\n"
	path := writeTempConfig(t, src)
	before, _ := os.ReadFile(path)

	injected, err := ensureCorruptedTriageBlock(path)
	require.NoError(t, err)
	assert.False(t, injected)
	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after), "unparseable file must be left alone")
}
