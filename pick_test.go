package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Test spec §4.5: --out emits only the set slots, and exactly the §2.6 key
// names (SUBMUX_MAIN/FABLE/OPUS/SONNET/HAIKU/PROFILE).
func TestWriteOutFileOnlySetSlotsExactKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pick.env")

	p := picks{Main: "claude-opus-5", Sonnet: "grok-4.6", ProfileName: "max-plan-grok-code"}
	if err := writeOutFile(path, p); err != nil {
		t.Fatalf("writeOutFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read --out file: %v", err)
	}
	content := string(data)

	mustContain := []string{
		"SUBMUX_MAIN='claude-opus-5'",
		"SUBMUX_SONNET='grok-4.6'",
		"SUBMUX_PROFILE='max-plan-grok-code'",
	}
	for _, want := range mustContain {
		if !strings.Contains(content, want) {
			t.Fatalf("--out file missing %q; got:\n%s", want, content)
		}
	}
	mustNotContain := []string{"SUBMUX_FABLE=", "SUBMUX_OPUS=", "SUBMUX_HAIKU="}
	for _, unwanted := range mustNotContain {
		if strings.Contains(content, unwanted) {
			t.Fatalf("--out file has unset-slot key %q; got:\n%s", unwanted, content)
		}
	}
	// exactly 3 lines: no stray keys beyond the ones set.
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("--out file has %d lines, want 3; got:\n%s", len(lines), content)
	}
}

// Test: writeOutFile with nothing set writes an empty file (no bare "=").
func TestWriteOutFileNothingSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pick.env")
	if err := writeOutFile(path, picks{}); err != nil {
		t.Fatalf("writeOutFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read --out file: %v", err)
	}
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("writeOutFile with nothing set: got %q, want empty", data)
	}
}

// Test: the rendered first screen (history, Update()-driven golden per
// spec §5). RED CONTROL: this fails if the header/status-bar chrome or the
// history row line is dropped from View() -- e.g. delete the
// styleTitle.Render("submux pick") line and this goes red on the title
// assertion below.
func TestViewHistoryFirstScreenGolden(t *testing.T) {
	now := time.Date(2026, 9, 18, 18, 0, 0, 0, time.UTC)
	profiles := []Profile{
		{Name: "max-plan-grok-code", Main: "claude-opus-5", Sonnet: "grok-4.6", Uses: 14, LastUsed: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		{Name: "cheap-sweep", Haiku: "glm-5.3-flash", Uses: 3, LastUsed: now.Add(-30 * time.Hour).Format(time.RFC3339)},
	}
	m := newPickModel(&config{}, "/tmp/does-not-matter-profiles.json", "/tmp/does-not-matter-cache.json", "", profiles, "")
	m.now = now
	view := m.View()

	for _, want := range []string{
		"submux pick",
		"max-plan-grok-code",
		"14 use(s)",
		"cheap-sweep",
		"+ create a new setup",
		"Mode: history",
		"q quit",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("history screen missing %q; full view:\n%s", want, view)
		}
	}
}

// Test: empty history jumps straight into the wizard with a one-line
// explanation (spec §2.2), never showing an empty history screen.
func TestEmptyHistoryJumpsIntoWizard(t *testing.T) {
	m := newPickModel(&config{}, "/tmp/x", "/tmp/y", "", nil, "")
	if m.mode != modeWizSub {
		t.Fatalf("empty history: mode = %v, want modeWizSub", m.mode)
	}
	if m.warning == "" {
		t.Fatalf("empty history: expected a one-line explanation in the warning/status area")
	}
}
