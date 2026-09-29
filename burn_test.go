package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBurnProvider(t *testing.T) {
	// 1. GLM Coding Plan
	glmHTML, err := os.ReadFile("testdata/glm.html")
	if err != nil {
		t.Fatalf("read testdata/glm.html: %v", err)
	}
	glmEntries, err := parseGLMBurn(string(glmHTML))
	if err != nil {
		t.Fatalf("parseGLMBurn: %v", err)
	}
	if len(glmEntries) == 0 {
		t.Fatal("parseGLMBurn returned 0 entries")
	}
	minWeight := 1e9
	for id, e := range glmEntries {
		if e.Weight < minWeight {
			minWeight = e.Weight
		}
		if e.Weight <= 0 || e.Weight > 10000 {
			t.Errorf("glm entry %s invalid weight: %f", id, e.Weight)
		}
	}
	if minWeight != 1.0 {
		t.Errorf("glm min weight = %f, want 1.0", minWeight)
	}
	// Verify suffixes distinct: glm-5.3 and glm-5.3-flash
	if _, ok := glmEntries["glm-5.3"]; !ok {
		t.Errorf("expected glm-5.3 entry")
	}
	if _, ok := glmEntries["glm-5.3-flash"]; !ok {
		t.Errorf("expected glm-5.3-flash distinct entry")
	}

	// 2. ChatGPT (Codex)
	cgHTML, err := os.ReadFile("testdata/chatgpt.html")
	if err != nil {
		t.Fatalf("read testdata/chatgpt.html: %v", err)
	}
	cgEntries, err := parseChatGPTBurn(string(cgHTML))
	if err != nil {
		t.Fatalf("parseChatGPTBurn: %v", err)
	}
	if len(cgEntries) == 0 {
		t.Fatal("parseChatGPTBurn returned 0 entries")
	}
	minWeight = 1e9
	for id, e := range cgEntries {
		if e.Weight < minWeight {
			minWeight = e.Weight
		}
		if e.Weight <= 0 || e.Weight > 10000 {
			t.Errorf("chatgpt entry %s invalid weight: %f", id, e.Weight)
		}
	}
	if minWeight != 1.0 {
		t.Errorf("chatgpt min weight = %f, want 1.0", minWeight)
	}
	// Suffix preservation: gpt-5.4-mini vs gpt-5.4
	if _, ok := cgEntries["gpt-5.4-mini"]; !ok {
		t.Errorf("expected gpt-5.4-mini distinct suffix entry")
	}
	if _, ok := cgEntries["gpt-5.4"]; !ok {
		t.Errorf("expected gpt-5.4 distinct entry")
	}
	if _, ok := cgEntries["gpt-5.6-luna"]; !ok {
		t.Errorf("expected gpt-5.6-luna distinct entry")
	}

	// 3. OpenCode Go
	opHTML, err := os.ReadFile("testdata/opencode.html")
	if err != nil {
		t.Fatalf("read testdata/opencode.html: %v", err)
	}
	opEntries, err := parseOpenCodeBurn(string(opHTML))
	if err != nil {
		t.Fatalf("parseOpenCodeBurn: %v", err)
	}
	if len(opEntries) == 0 {
		t.Fatal("parseOpenCodeBurn returned 0 entries")
	}
	minWeight = 1e9
	for id, e := range opEntries {
		if e.Weight < minWeight {
			minWeight = e.Weight
		}
		if e.Weight <= 0 || e.Weight > 10000 {
			t.Errorf("opencode entry %s invalid weight: %f", id, e.Weight)
		}
	}
	if minWeight != 1.0 {
		t.Errorf("opencode min weight = %f, want 1.0", minWeight)
	}
	// Verify models present
	if _, ok := opEntries["kimi-k3"]; !ok {
		t.Errorf("expected kimi-k3 entry")
	}
	if _, ok := opEntries["kimi-k2.7-code"]; !ok {
		t.Errorf("expected kimi-k2.7-code entry")
	}

	// 4. Shape-drift handling: empty/corrupted tables return errors
	if _, err := parseGLMBurn("<html><body>No tables</body></html>"); err == nil {
		t.Error("expected error for empty GLM table")
	}
	if _, err := parseChatGPTBurn("<html><body>No tables</body></html>"); err == nil {
		t.Error("expected error for empty ChatGPT table")
	}
	if _, err := parseOpenCodeBurn("<html><body>No rows</body></html>"); err == nil {
		t.Error("expected error for empty OpenCode rows")
	}
}

// ---- §S6/§S7: measured fallback + burnCell render states -------------------

// RED CONTROL (runbook §S9 item 1, the hard rule): a model with no burn
// entry must render the dim not-disclosed form and NEVER "1x". Against a
// naive burnCell that renders a default "~1x" for an absent entry instead
// of the dim form, this fails -- verified 2026-09-22 by temporarily
// replacing the missing-entry return with the naive form:
//
//	burn_test.go: RED burnCell missing entry: got "▇  ~1x", want it to name
//	(not disclosed) and contain no multiplier
func TestBurnCellMissingRendersNotDisclosed(t *testing.T) {
	m := pickModel{burn: burnFile{Providers: map[string]map[string]burnEntry{
		"Claude Max": {
			// A Source:"none" entry is the on-disk form of "no data".
			"claude-opus-5":   {Source: "none"},
			"claude-sonnet-5": {Weight: 2, Source: "measured"},
		},
	}}}
	for _, tc := range []struct{ name, provider, id string }{
		{"absent id", "Claude Max", "claude-unknown-9"},
		{"source none", "Claude Max", "claude-opus-5"},
		{"absent provider", "No Such Plan", "claude-sonnet-5"},
	} {
		cell := m.burnCell(tc.provider, tc.id)
		if !strings.Contains(cell, "(not disclosed)") {
			t.Errorf("%s: burnCell = %q, want it to name (not disclosed)", tc.name, cell)
		}
		if strings.Contains(cell, "~") {
			t.Errorf("%s: burnCell = %q, want NO multiplier for a missing entry (never \"~1x\")", tc.name, cell)
		}
	}
}

// §S7 provider state: log-scaled bar + ~Nx multiplier, no "measured" label.
func TestBurnCellProvider(t *testing.T) {
	m := pickModel{burn: burnFile{Providers: map[string]map[string]burnEntry{
		"GLM Coding Plan": {
			"glm-5.3":       {Weight: 12, Unit: "credits/Mtok", Source: "provider"},
			"glm-5.3-flash": {Weight: 1, Unit: "credits/Mtok", Source: "provider"},
		},
	}}}
	heavy := m.burnCell("GLM Coding Plan", "glm-5.3")
	if !strings.Contains(heavy, "~12x") {
		t.Errorf("provider cell = %q, want ~12x", heavy)
	}
	if !strings.HasPrefix(heavy, "▇▇▇▇") {
		t.Errorf("provider cell = %q, want a bar of 4 glyphs (log2(12)=3 +1)", heavy)
	}
	if strings.Contains(heavy, "measured") {
		t.Errorf("provider cell = %q, want no measured label", heavy)
	}
	// A PRESENT lightest entry legitimately shows ~1x; only missing data is
	// barred from it. Its bar must be strictly shorter than the heavy one's.
	light := m.burnCell("GLM Coding Plan", "glm-5.3-flash")
	if !strings.Contains(light, "~1x") {
		t.Errorf("light provider cell = %q, want ~1x", light)
	}
	if burnBarLen(12) <= burnBarLen(1) {
		t.Errorf("burnBarLen: heaviest (%d) must be longer than lightest (%d)", burnBarLen(12), burnBarLen(1))
	}
}

// §S7 measured state: same bar, "(measured)" suffix, plus the hardcoded
// Anthropic notes (§S6).
func TestBurnCellMeasured(t *testing.T) {
	m := pickModel{burn: burnFile{Providers: map[string]map[string]burnEntry{
		"Claude Max": {
			"claude-fable-5":   {Weight: 3.5, Unit: "measured tok/day-of-use", Source: "measured", Note: "⚠ 50% wk cap"},
			"claude-haiku-4-5": {Weight: 1, Unit: "measured tok/day-of-use", Source: "measured", Note: "(effort ignored)"},
		},
	}}}
	f := m.burnCell("Claude Max", "claude-fable-5")
	if !strings.Contains(f, "~3.5x (measured)") {
		t.Errorf("measured cell = %q, want ~3.5x (measured)", f)
	}
	if !strings.Contains(f, "⚠ 50% wk cap") {
		t.Errorf("measured cell = %q, want the fable weekly-cap note", f)
	}
	h := m.burnCell("Claude Max", "claude-haiku-4-5")
	if !strings.Contains(h, "(measured)") || !strings.Contains(h, "(effort ignored)") {
		t.Errorf("measured haiku cell = %q, want (measured) and (effort ignored)", h)
	}
}

// §S6 aggregation + per-provider normalisation (runbook §S9 item 2): scalar
// = total tokens / distinct days of use, lightest model exactly 1.0, ids the
// catalog cannot resolve are dropped rather than filed under a wrong payer.
func TestBuildMeasuredBurnNormalization(t *testing.T) {
	cfg := &config{Routes: []route{{
		match: "claude-*", upstream: "https://api.anthropic.com",
		subscription: "Claude Max",
	}}}
	liveList := map[string]cacheEntry{"https://api.anthropic.com": {FetchedAt: time.Now(), Models: []cachedModel{
		{ID: "claude-fable-5"}, {ID: "claude-haiku-4-5"}, {ID: "claude-sonnet-5"},
	}}}
	daily := ccusageDailyJSON{Daily: []ccusageDay{
		{Period: "20260921", ModelBreakdowns: []ccusageBreakdown{
			{ModelName: "claude-fable-5", InputTokens: 1000, OutputTokens: 1000},
			{ModelName: "claude-haiku-4-5", InputTokens: 100, OutputTokens: 100},
			{ModelName: "mystery-model"}, // unresolved payer: must be dropped
		}},
		{Period: "20260922", ModelBreakdowns: []ccusageBreakdown{
			{ModelName: "claude-fable-5", InputTokens: 1000, OutputTokens: 1000},
			{ModelName: "claude-sonnet-5", InputTokens: 500, OutputTokens: 500, CacheReadTokens: 500},
		}},
	}}
	out := buildMeasuredBurn(cfg, liveList, daily)
	entries := out["Claude Max"]
	if len(entries) != 3 {
		t.Fatalf("buildMeasuredBurn: %d entries, want 3 (mystery-model dropped); got %+v", len(entries), out)
	}
	fable := entries["claude-fable-5"]   // 4000 tok over 2 days = 2000/day
	haiku := entries["claude-haiku-4-5"] // 200 tok over 1 day  = 200/day (min)
	sonnet := entries["claude-sonnet-5"] // 1500 tok over 1 day = 1500/day
	if haiku.Weight != 1.0 {
		t.Errorf("lightest model weight = %v, want exactly 1.0", haiku.Weight)
	}
	if fable.Weight != 10.0 {
		t.Errorf("fable weight = %v, want 10.0 (4000 tok / 2 days-of-use / 200)", fable.Weight)
	}
	if sonnet.Weight != 7.5 {
		t.Errorf("sonnet weight = %v, want 7.5 (1500 / 200)", sonnet.Weight)
	}
	if fable.Source != "measured" || fable.Unit != "measured tok/day-of-use" {
		t.Errorf("fable entry = %+v, want Source measured and the measured unit", fable)
	}
	if fable.Note != "⚠ 50% wk cap" {
		t.Errorf("fable note = %q, want the weekly-cap note", fable.Note)
	}
	if haiku.Note != "(effort ignored)" {
		t.Errorf("haiku note = %q, want (effort ignored)", haiku.Note)
	}
	if burnBarLen(fable.Weight) <= burnBarLen(haiku.Weight) {
		t.Errorf("bar: heaviest (%d glyphs) must be longer than lightest (%d)", burnBarLen(fable.Weight), burnBarLen(haiku.Weight))
	}
}

// §S6/§S8: mergeMeasuredBurn never overwrites a provider-published weight
// with a local measurement.
func TestMergeMeasuredBurnKeepsProviderEntries(t *testing.T) {
	dst := burnFile{Providers: map[string]map[string]burnEntry{
		"GLM Coding Plan": {"glm-5.3": {Weight: 12, Source: "provider"}},
	}}
	measured := map[string]map[string]burnEntry{
		"GLM Coding Plan": {
			"glm-5.3":       {Weight: 99, Source: "measured"},
			"glm-5.3-flash": {Weight: 1, Source: "measured"},
		},
	}
	mergeMeasuredBurn(&dst, measured)
	if got := dst.Providers["GLM Coding Plan"]["glm-5.3"]; got.Source != "provider" || got.Weight != 12 {
		t.Errorf("merge overwrote a provider entry: %+v", got)
	}
	if got := dst.Providers["GLM Coding Plan"]["glm-5.3-flash"]; got.Source != "measured" {
		t.Errorf("merge did not add the new measured entry: %+v", got)
	}
}

// Review s11 r1 Finding 1: cloneBurnProviders must detach BOTH map levels.
// Red control: copy only the outer map in cloneBurnProviders (inner maps
// shared) -> the inner-map mutation below reaches src and this test fails.
func TestCloneBurnProvidersDetached(t *testing.T) {
	src := map[string]map[string]burnEntry{
		"GLM Coding Plan": {"glm-5.3": {Weight: 12, Source: "provider"}},
	}
	dst := cloneBurnProviders(src)
	if len(dst) != len(src) {
		t.Fatalf("clone has %d providers, want %d", len(dst), len(src))
	}
	if dst["GLM Coding Plan"]["glm-5.3"] != (burnEntry{Weight: 12, Source: "provider"}) {
		t.Errorf("clone lost an entry: %+v", dst["GLM Coding Plan"]["glm-5.3"])
	}
	// Mutate dst at both levels; src must stay unchanged.
	dst["GLM Coding Plan"]["glm-5.3"] = burnEntry{Weight: 99, Source: "measured"}
	dst["ChatGPT"] = map[string]burnEntry{"gpt-5.4": {Weight: 5}}
	if got := src["GLM Coding Plan"]["glm-5.3"]; got.Weight != 12 || got.Source != "provider" {
		t.Errorf("inner map shared with clone: src entry = %+v, want the original", got)
	}
	if _, ok := src["ChatGPT"]; ok {
		t.Error("outer map shared with clone: src gained a provider the clone added")
	}
	if nilMap := cloneBurnProviders(nil); nilMap == nil {
		t.Error("cloneBurnProviders(nil) = nil, want a non-nil empty map")
	}
}

// Review s11 r1 Finding 1: the file refreshBurnCmd returns must not alias
// prev.Providers, whose maps pickModel.burn keeps reading on the UI goroutine
// while the cmd runs. Red control: revert refreshBurnCmd to `next := prev`
// with no clone -> msg.burn.Providers aliases prev, so the mutations below
// reach prev and this test fails (verified against the pre-fix body).
func TestRefreshBurnCmdDetachesPrev(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // burnPath() lands in the temp dir, off the real cache
	prev := burnFile{Providers: map[string]map[string]burnEntry{
		"GLM Coding Plan": {"glm-5.3": {Weight: 12, Source: "provider"}},
	}}
	cmd := refreshBurnCmd(nil, nil, prev, false, false) // disk-only path: no fetches
	msg := cmd().(burnRefreshMsg)
	if msg.err != "" {
		t.Fatalf("refreshBurnCmd err = %q, want none (temp HOME, no network)", msg.err)
	}
	// Mutate the returned file at both levels the way a later merge would;
	// prev -- what the UI still holds -- must stay intact.
	msg.burn.Providers["GLM Coding Plan"]["glm-5.3"] = burnEntry{Weight: 99, Source: "measured"}
	msg.burn.Providers["ChatGPT"] = map[string]burnEntry{"gpt-5.4": {Weight: 5}}
	if got := prev.Providers["GLM Coding Plan"]["glm-5.3"]; got != (burnEntry{Weight: 12, Source: "provider"}) {
		t.Errorf("prev inner map mutated via the returned file: %+v", got)
	}
	if _, ok := prev.Providers["ChatGPT"]; ok {
		t.Error("prev outer map mutated via the returned file")
	}
}

// Review s11 r2 Finding 1: refreshBurnCmd's background closure ranges the
// entries map (m.cacheEntries) via fetchMeasuredBurn -> payerForModelID
// while the UI goroutine writes it in place (applyRefreshResult), so the cmd
// must snapshot entries before the closure exists. ccusage is stubbed on a
// private PATH so the real measured path runs end to end, synchronously.
// Red control: hand the closure the live map (drop the clone) -> after the
// delete below payerForModelID cannot resolve glm-5.3, buildMeasuredBurn
// drops it, and the measured entry is missing (verified against pre-fix body).
func TestRefreshBurnCmdDetachesEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // burnPath() lands in the temp dir, off the real cache
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// echo is a shell builtin, so the stub needs nothing else from PATH.
	script := "#!/bin/sh\necho '" +
		`{"daily":[{"period":"20260921","modelBreakdowns":[{"modelName":"glm-5.3","inputTokens":1000,"outputTokens":0}]}]}` + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "ccusage"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin) // runCCUsageDaily's LookPath finds the stub
	cfg := &config{Subscriptions: map[string]string{"Z.ai": "GLM Coding Plan"}}
	entries := map[string]cacheEntry{
		"https://open.bigmodel.cn": {Models: []cachedModel{{ID: "glm-5.3", OwnedBy: "Z.ai"}}},
	}
	cmd := refreshBurnCmd(cfg, entries, burnFile{}, false, true)
	// UI-goroutine write after the cmd exists, before the closure runs --
	// the exact applyRefreshResult shape the race detector caught.
	delete(entries, "https://open.bigmodel.cn")
	msg := cmd().(burnRefreshMsg)
	if msg.err != "" {
		t.Fatalf("refreshBurnCmd err = %q, want none (stubbed ccusage, temp HOME)", msg.err)
	}
	if _, ok := msg.burn.Providers["GLM Coding Plan"]["glm-5.3"]; !ok {
		t.Fatalf("closure saw the mutated entries map: glm-5.3 has no measured entry, want the snapshot's payer resolution; burn = %+v", msg.burn)
	}
}
