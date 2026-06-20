package config

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ensureCorruptedTriageBlock surfaces the health.corrupted_triage settings in the
// user's LIVE config file so the option is visible and editable, without enabling
// anything. On startup, if the file parses and is MISSING health.corrupted_triage,
// the documented block (with enabled: false) is injected non-destructively:
//
//   - The written default is enabled: false — this only surfaces the option, it
//     never auto-enables a delete feature.
//   - An existing block (the user's enabled: true / tuned thresholds) is left
//     completely untouched.
//   - All other settings, comments and formatting are preserved: the block is
//     spliced into the raw text and written atomically (temp file + rename).
//
// It returns whether the block was injected. A parse failure is treated as
// "leave the file alone" (returns false, nil) so a broken config is never
// rewritten.
func ensureCorruptedTriageBlock(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	// Structural check (ignores comments). If we can't parse it, do not touch it.
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return false, nil
	}
	if hv, exists := root["health"]; exists {
		h, ok := hv.(map[string]any)
		if !ok {
			// `health` exists but is not a mapping (e.g. a scalar like
			// `health: true`). We can't safely add child keys under it — leave
			// the file untouched.
			return false, nil
		}
		if _, present := h["corrupted_triage"]; present {
			return false, nil // already there — never overwrite
		}
	}

	newContent, changed := injectCorruptedTriage(string(raw))
	if !changed {
		return false, nil
	}
	if err := atomicWriteFile(path, []byte(newContent)); err != nil {
		return false, err
	}
	return true, nil
}

// injectCorruptedTriage splices the corrupted_triage block into the raw config
// text. If a top-level `health:` mapping exists, the block is inserted as its
// first child (matching the existing child indentation); otherwise a new
// `health:` section is appended. Returns the new content and whether it changed.
func injectCorruptedTriage(content string) (string, bool) {
	lines := strings.Split(content, "\n")

	// Find the top-level `health:` key (no leading whitespace).
	healthIdx := -1
	healthInline := false
	for i, ln := range lines {
		if ln == "" || ln[0] == ' ' || ln[0] == '\t' {
			continue
		}
		trimmed := strings.TrimRight(ln, " \t\r")
		parts := strings.SplitN(trimmed, ":", 2)
		if strings.TrimSpace(parts[0]) != "health" || len(parts) != 2 {
			continue
		}
		// Found the top-level `health` key. A block mapping header has nothing but
		// whitespace (or a trailing comment) after the colon, with children on the
		// following indented lines — we can splice into that. Inline forms — a flow
		// mapping (`health: { ... }`) or a scalar (`health: true`) — cannot take
		// block children, so we must not touch them (doing so would yield invalid
		// YAML or a duplicate `health` key).
		after := strings.TrimSpace(parts[1])
		if after == "" || strings.HasPrefix(after, "#") {
			healthIdx = i
		} else {
			healthInline = true
		}
		break
	}

	// Inline health value: leave the file unchanged rather than appending a
	// duplicate block or corrupting the inline mapping.
	if healthInline {
		return content, false
	}

	childIndent := "  "
	if healthIdx >= 0 {
		// Detect the indentation the existing health children use so the injected
		// keys line up (valid YAML requires consistent sibling indentation).
		for i := healthIdx + 1; i < len(lines); i++ {
			ln := lines[i]
			if strings.TrimSpace(ln) == "" {
				continue
			}
			leading := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
			if leading == "" {
				break // reached the next top-level key
			}
			childIndent = leading
			break
		}
	}
	block := buildTriageBlock(childIndent, childIndent+"  ")

	if healthIdx >= 0 {
		out := make([]string, 0, len(lines)+len(block))
		out = append(out, lines[:healthIdx+1]...)
		out = append(out, block...)
		out = append(out, lines[healthIdx+1:]...)
		return strings.Join(out, "\n"), true
	}

	// No health section at all — append one with 2-space children.
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	appended := append([]string{"", "health:"}, buildTriageBlock("  ", "    ")...)
	return content + strings.Join(appended, "\n") + "\n", true
}

// buildTriageBlock returns the commented corrupted_triage block, with keys at
// keyIndent and sub-keys at subIndent. Mirrors config.sample.yaml.
func buildTriageBlock(keyIndent, subIndent string) []string {
	return []string{
		keyIndent + "# Corrupted-file auto-delete triage. SOFT-DELETES (health DB row + .meta only —",
		keyIndent + "# never the library file under mount_path, never an arr's only copy) corrupted",
		keyIndent + "# records that are provably safe: file_removed zombies, dead+unowned, and",
		keyIndent + "# dead+arr-already-has-a-replacement. Off by default; fails closed (any arr",
		keyIndent + "# lookup error keeps the record, never treated as \"unowned\").",
		keyIndent + "corrupted_triage:",
		subIndent + "enabled: false # Master switch. Off by default — nothing is ever deleted while false (default: false)",
		subIndent + "max_deletes_per_run: 50 # Hard cap on soft-deletes per triage pass (default: 50)",
		subIndent + "mass_event_threshold: 500 # Abort the pass if more corrupted records than this exist, so a systemic failure never triggers mass deletion (default: 500)",
		subIndent + "backstop_interval_minutes: 60 # Base cadence of the adaptive backstop sweep; triage also runs on enter-corrupted and arr-webhook events (default: 60)",
	}
}

// atomicWriteFile writes data to path atomically (temp file in the same directory
// then rename), preserving the original file's permissions.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}

	tmp, err := os.CreateTemp(dir, ".altmount-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
