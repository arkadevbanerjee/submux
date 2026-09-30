package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	// This test exercises the MODELS 'r' refresh; suppress the §S8 background
	// burn refresh so the drain loop below does not fire real provider
	// fetches and ccusage alongside it.
	m.burnRefreshChecked = true
	m.cacheEntries = entries
	// Seed the stale-flagged row directly: buildSubscriptionCatalog now
	// re-fetches a stale entry itself, which would count as a hit here.
	m.catalog = []subscriptionOption{{Name: "xai (unmapped)", ModelIDs: []string{"stale-id"}, CacheAge: "3h ago", Upstreams: []string{url}}}
	m.subCursor = 0
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
			{match: "claude-*", upstream: "https://api.anthropic.com", subscription: "Claude Max"},
		},
	}
	cachePath := filepath.Join(t.TempDir(), "models-cache.json")
	if err := saveModelsCache(cachePath, map[string]cacheEntry{"https://api.anthropic.com": {FetchedAt: time.Now(), Models: []cachedModel{{ID: "claude-opus-5"}}}}); err != nil {
		t.Fatal(err)
	}
	profiles := []Profile{{Name: "max-plan", Main: "claude-opus-5", Uses: 1, LastUsed: time.Now().Format(time.RFC3339)}}
	m := newPickModel(cfg, "/tmp/does-not-matter-profiles.json", cachePath, "", profiles, "")
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

// Raw float ratios (1.344887382, 13063.378845) render at a readable precision.
func TestFormatBurnWeight(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{1, "1"}, {1.344887382, "1.3"}, {2.3333333, "2.3"}, {9.96, "10"}, {10.4, "10"}, {2746.4001588584106, "2746"}, {13063.378845354988, "13063"},
	} {
		if got := formatBurnWeight(tc.in); got != tc.want {
			t.Errorf("formatBurnWeight(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ctrl+r on the model pane refreshes the highlighted subscription (plain 'r'
// there is a filter character), and the outcome shows in the status bar even
// when nothing changed upstream.
func TestCtrlRRefreshesOnModelPaneAndReportsOutcome(t *testing.T) {
	url, hits := modelsServer(t, []upstreamModel{{ID: "fresh-id", OwnedBy: "xai"}})
	cfg := &config{Routes: []route{{match: "*", upstream: url, authKind: "none"}}}
	m := newPickModel(cfg, "/tmp/x-profiles.json", "/tmp/x-cache.json", "", nil, "")
	m.mode = modeWizModel
	m.burnRefreshChecked = true
	m.cacheEntries = map[string]cacheEntry{url: {FetchedAt: time.Now(), Models: []cachedModel{{ID: "fresh-id", OwnedBy: "xai"}}}}
	m.catalog = []subscriptionOption{{Name: "xai (unmapped)", ModelIDs: []string{"fresh-id"}, Upstreams: []string{url}}}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	next := updated.(pickModel)
	if cmd == nil || !next.refreshing {
		t.Fatalf("ctrl+r on the model pane: cmd=%v refreshing=%v, want a refresh in flight", cmd != nil, next.refreshing)
	}
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
		var nc tea.Cmd
		updated, nc = next.Update(msg)
		next = updated.(pickModel)
		pending = append(pending, nc)
	}
	if *hits < 1 {
		t.Fatalf("ctrl+r: upstream got %d request(s), want >= 1", *hits)
	}
	if !strings.Contains(next.statusBar(), "refreshed xai (unmapped)") {
		t.Fatalf("status bar = %q, want a 'refreshed ...' outcome even with no change", next.statusBar())
	}
}

func fallbackModel() pickModel {
	cfg := &config{SubscriptionProbes: []subscriptionProbe{{Subscription: "ChatGPT", Model: "gpt-5.5", Fallback: "Zen"}}}
	m := newPickModel(cfg, "/tmp/x-profiles.json", "/tmp/x-cache.json", "", nil, "")
	m.mode = modeWizSub
	m.burnRefreshChecked, m.probesSent = true, true
	m.catalog = []subscriptionOption{
		{Name: "ChatGPT", ModelIDs: []string{"gpt-a"}, Unavailable: "subscription ended or inactive: login expired"},
		{Name: "Zen", ModelIDs: []string{"zen-a"}},
		{Name: "Dead", ModelIDs: []string{"d"}, Unavailable: "ended"},
	}
	return m
}

func press(m pickModel, k string) pickModel {
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	next, _ := m.Update(msg)
	return next.(pickModel)
}

// Enter on a dead row asks before offering the free fallback; it never switches by itself.
func TestEnterOnDeadRowAsksBeforeFallback(t *testing.T) {
	m := press(fallbackModel(), "enter")
	if m.mode != modeWizFallback || m.fbTo != 1 {
		t.Fatalf("Enter on a dead row: mode=%v fbTo=%d, want the confirm prompt offering Zen", m.mode, m.fbTo)
	}
	if v := m.View(); !strings.Contains(v, "Use ") || !strings.Contains(v, "Zen") {
		t.Fatalf("prompt does not name the fallback:\n%s", v)
	}
	no := press(m, "n")
	if no.mode != modeWizSub || no.wizPicks != [5]string{} || no.subCursor != 0 {
		t.Fatalf("n: mode=%v picks=%v cursor=%d, want back on the list, nothing chosen", no.mode, no.wizPicks, no.subCursor)
	}
	yes, cmd := func() (pickModel, tea.Cmd) {
		n, c := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
		return n.(pickModel), c
	}()
	if yes.mode != modeWizModel || yes.subCursor != 1 || yes.wizPicks != [5]string{} {
		t.Fatalf("y: mode=%v cursor=%d picks=%v, want Zen's model list with nothing chosen yet", yes.mode, yes.subCursor, yes.wizPicks)
	}
	if cmd == nil {
		t.Fatalf("y: no per-model probe dispatched for the fallback's models")
	}
}

func TestEnterOnDeadRowWithoutFallbackSaysSo(t *testing.T) {
	m := fallbackModel()
	m.subCursor = 2
	m = press(m, "enter")
	if m.mode != modeWizSub || !strings.Contains(m.notice, "no free fallback") {
		t.Fatalf("dead row with no fallback: mode=%v notice=%q", m.mode, m.notice)
	}
}

func TestModelProbeGreysAndBlocksModel(t *testing.T) {
	m := fallbackModel()
	m.subCursor = 1
	m.mode = modeWizModel
	m.catalog[1].ModelIDs = []string{"zen-a", "zen-b"}
	next, _ := m.Update(modelProbeMsg{sub: "Zen", results: map[string]probeResult{
		"zen-a": {State: "ended", Detail: "quota exhausted (HTTP 400)"},
		"zen-b": {State: "ok"},
	}})
	m = next.(pickModel)
	if v := m.View(); !strings.Contains(v, "quota exhausted") {
		t.Fatalf("dead model shows no reason:\n%s", v)
	}
	m = press(m, "enter") // cursor on zen-a
	if m.mode != modeWizModel || m.wizPicks[0] != "" || !strings.Contains(m.notice, "zen-a") {
		t.Fatalf("Enter on a dead model: mode=%v picks=%v notice=%q, want refused with a reason", m.mode, m.wizPicks, m.notice)
	}
	m.modelCursor = 1
	m = press(m, "enter")
	if m.wizPicks[0] != "zen-b" {
		t.Fatalf("Enter on a live model: picks=%v, want zen-b", m.wizPicks)
	}
}

func TestProbeModelsCmdProbesEachIDOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(readBody(r), `"model":"bad"`) {
			w.WriteHeader(402)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := &config{Routes: []route{{match: "*", upstream: srv.URL, authKind: "none"}}}
	msg := probeModelsCmd(cfg, "S", []string{"good", "bad"})().(modelProbeMsg)
	if hits.Load() != 2 || msg.results["good"].State != "ok" || msg.results["bad"].State != "ended" {
		t.Fatalf("hits=%d results=%+v", hits.Load(), msg.results)
	}
}

func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// `sc --alias` writes the alias with jq; a picker save must not drop it.
func TestAliasSurvivesSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, []byte(`{"profiles":[{"name":"p1","main":"gpt-6.1-sol","alias":"sol","uses":1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, _ := loadProfiles(path)
	ps = touchProfile(ps, "p1", time.Now())
	if err := saveProfiles(path, ps); err != nil {
		t.Fatal(err)
	}
	again, _ := loadProfiles(path)
	if len(again) != 1 || again[0].Alias != "sol" || again[0].Uses != 2 {
		t.Fatalf("after touch+save+reload: %+v, want alias sol kept and uses 2", again)
	}
}

func TestAliasKeyEditsAndRejectsDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	ps := []Profile{{Name: "one", Main: "a", Uses: 2}, {Name: "two", Main: "b", Alias: "taken", Uses: 1}}
	m := newPickModel(&config{}, path, "/tmp/x-cache.json", "", ps, "")
	m = press(m, "a")
	if m.mode != modeAlias || m.aliasTarget != "one" {
		t.Fatalf("a: mode=%v target=%q", m.mode, m.aliasTarget)
	}
	m = press(m, "taken")
	m = press(m, "enter")
	if m.mode != modeAlias || !strings.Contains(m.notice, "already used") {
		t.Fatalf("duplicate alias accepted: mode=%v notice=%q", m.mode, m.notice)
	}
	m.nameInput.SetValue("fast")
	m = press(m, "enter")
	if m.mode != modeHistory || m.profiles[0].Alias != "fast" {
		t.Fatalf("alias not saved: mode=%v profiles=%+v", m.mode, m.profiles)
	}
	saved, _ := loadProfiles(path)
	if saved[0].Alias != "fast" && saved[1].Alias != "fast" {
		t.Fatalf("alias not on disk: %+v", saved)
	}
	if v := m.View(); !strings.Contains(v, "fast  (one)") {
		t.Fatalf("history row does not show the alias:\n%s", v)
	}
}

func TestSkipModelProbeSkipsKiro(t *testing.T) {
	cfg := &config{SubscriptionProbes: []subscriptionProbe{{Subscription: "Kiro", Model: "k", SkipModelProbe: true}}}
	m := newPickModel(cfg, "/tmp/x-p.json", "/tmp/x-c.json", "", nil, "")
	m.mode = modeWizSub
	m.burnRefreshChecked, m.probesSent = true, true
	m.catalog = []subscriptionOption{{Name: "Kiro", ModelIDs: []string{"k1", "k2"}}}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || next.(pickModel).mode != modeWizModel {
		t.Fatalf("Kiro list opened with a probe cmd (cmd=%v) or wrong mode", cmd != nil)
	}
}
