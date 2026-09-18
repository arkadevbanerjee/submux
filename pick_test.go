package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// DEFECT 1, round 2: pressing 'r' on a refreshable subscription row bypasses
// the 10-minute freshness window (spec §2.5's "r to refresh") and updates
// the row once the fetch lands. RED CONTROL: against the pre-fix pick.go
// (no "r" case in updateWizSub), this fails at the first assertion because
// Update returns a nil cmd -- verified 2026-09-18:
//
//	pick_test.go:42: pressing 'r' on a refreshable row: cmd = nil, want a
//	refresh command dispatched
func TestRefreshKeyTriggersUpstreamRefetch(t *testing.T) {
	url, hits := modelsServer(t, []upstreamModel{{ID: "fresh-id", OwnedBy: "xai"}})
	cfg := &config{Routes: []route{{match: "*", upstream: url, authKind: "none"}}}

	staleAt := time.Now().Add(-3 * time.Hour)
	entries := map[string]cacheEntry{
		url: {FetchedAt: staleAt, Models: []cachedModel{{ID: "stale-id", OwnedBy: "xai"}}},
	}
	m := newPickModel(cfg, "/tmp/does-not-matter-profiles.json", "/tmp/does-not-matter-cache.json", "", nil, "")
	m.mode = modeWizSub
	m.cacheEntries = entries
	m.catalog = buildSubscriptionCatalog(cfg, entries, time.Now())
	m.subCursor = 0
	if len(m.catalog) == 0 {
		t.Fatalf("setup: expected at least one catalog entry")
	}
	if m.catalog[0].CacheAge == "" {
		t.Fatalf("setup: expected the seeded stale entry to be flagged, catalog=%+v", m.catalog)
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	next := updated.(pickModel)

	if cmd == nil {
		t.Fatalf("pressing 'r' on a refreshable row: cmd = nil, want a refresh command dispatched")
	}
	if !next.refreshing {
		t.Fatalf("pressing 'r': model.refreshing = false, want true while the fetch is in flight")
	}
	// Drain the dispatched command(s), feeding every resulting message back
	// into Update, same as the real event loop would (this also exercises
	// the spinner.Tick command bundled into the same tea.Batch).
	pending := []tea.Cmd{cmd}
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		if c == nil {
			continue
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			pending = append(pending, batch...)
			continue
		}
		var newCmd tea.Cmd
		updated, newCmd = next.Update(msg)
		next = updated.(pickModel)
		if newCmd != nil {
			pending = append(pending, newCmd)
		}
	}

	if got := *hits; got < 1 {
		t.Fatalf("pressing 'r': upstream received %d request(s), want >= 1 (bypassing the freshness window)", got)
	}
	if next.refreshing {
		t.Fatalf("after the refresh lands: model.refreshing = true, want false")
	}
	var refreshed *subscriptionOption
	for i := range next.catalog {
		if len(next.catalog[i].ModelIDs) > 0 && next.catalog[i].ModelIDs[0] == "fresh-id" {
			refreshed = &next.catalog[i]
		}
	}
	if refreshed == nil {
		t.Fatalf("after refresh: catalog does not show the freshly fetched model id; catalog=%+v", next.catalog)
	}
	if refreshed.CacheAge != "" {
		t.Fatalf("after a successful refresh: CacheAge = %q, want empty (fresh)", refreshed.CacheAge)
	}
}

// DEFECT 2, round 2: the history screen's expanded row must name the payer
// for each set slot (spec §2.2 "each with the subscription that pays for
// it"), not just the bare model id. RED CONTROL: against the pre-fix
// pick.go (expanded row = one "main=<id> fable=<id> ..." line, no
// subscription lookup anywhere), this fails to find "Claude Max" -- verified
// 2026-09-18:
//
//	pick_test.go:97: expanded row for a set slot: view does not name the
//	payer "Claude Max"; full view:
//	    ...
//	        main=claude-opus-5  fable=-  opus=-  sonnet=-  haiku=-
//	    ...
func TestExpandedRowShowsPayerForSetSlot(t *testing.T) {
	cfg := &config{
		Routes: []route{
			{match: "claude-*", upstream: "https://api.anthropic.com", subscription: "Claude Max", models: []string{"claude-opus-5"}},
		},
	}
	profiles := []Profile{{Name: "max-plan", Main: "claude-opus-5", Uses: 1, LastUsed: time.Now().Format(time.RFC3339)}}
	m := newPickModel(cfg, "/tmp/does-not-matter-profiles.json", "/tmp/does-not-matter-cache.json", "", profiles, "")
	m.cursor = 0 // expand the (only) row

	view := m.View()
	if !strings.Contains(view, "Claude Max") {
		t.Fatalf("expanded row for a set slot: view does not name the payer %q; full view:\n%s", "Claude Max", view)
	}
	// Unset slots must not render a bare "-" (spec: "— (session default)").
	if strings.Contains(view, "fable   -") || strings.Contains(view, "fable  -\n") {
		t.Fatalf("unset slot rendered as a bare '-'; full view:\n%s", view)
	}
	if !strings.Contains(view, "(session default)") {
		t.Fatalf("unset slot missing the '(session default)' label; full view:\n%s", view)
	}
}
