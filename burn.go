// burn.go persists subscription burn and quota indicators for `submux pick`
// models (runbook §S4). It mirrors modelscache.go: fail-open, cached,
// stale-tolerant, and never runs on the `serve` hot path.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	providerGLM      = "GLM Coding Plan"
	providerChatGPT  = "ChatGPT (Codex)"
	providerOpenCode = "OpenCode Go"
)

var (
	reHTMLTable   = regexp.MustCompile(`(?is)<table[^>]*>(.*?)</table>`)
	reHTMLTR      = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	reHTMLTD      = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	reHTMLTag     = regexp.MustCompile(`(?is)<[^>]+>`)
	reOpenCodeRow = regexp.MustCompile(`data-slot="model-row"[^>]*data-model="([^"]+)"`)
	reAllowance   = regexp.MustCompile(`(?is)data-slot="allowance"[^>]*>.*?\$</span>\s*<span[^>]*>(\d+)</span>`)
)

const (
	burnProviderFreshFor = 7 * 24 * time.Hour
	burnMeasuredFreshFor = 24 * time.Hour
	burnStaleWarnAfter   = 14 * 24 * time.Hour
)

type burnEntry struct {
	Weight    float64 `json:"weight"` // relative to THIS provider's lightest model = 1.0
	Unit      string  `json:"unit"`   // "credits/Mtok" | "$/mo cap" | "measured tok/day-of-use"
	Source    string  `json:"source"` // "provider" | "measured" | "none"
	SourceURL string  `json:"source_url,omitempty"`
	Note      string  `json:"note,omitempty"` // "⚠ 50% wk cap", "(effort ignored)"
	AsOf      string  `json:"as_of"`          // RFC3339
}

type burnFile struct {
	ProviderFetchedAt time.Time                       `json:"provider_fetched_at"`
	MeasuredFetchedAt time.Time                       `json:"measured_fetched_at"`
	Providers         map[string]map[string]burnEntry `json:"providers"` // subscription name -> model id -> entry
}

func burnPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "submux", "burn.json"), nil
}

func loadBurnFile(path string) burnFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return burnFile{Providers: map[string]map[string]burnEntry{}}
	}
	var f burnFile
	if err := json.Unmarshal(data, &f); err != nil || f.Providers == nil {
		return burnFile{Providers: map[string]map[string]burnEntry{}}
	}
	return f
}

func saveBurnFile(path string, f burnFile) error {
	if f.Providers == nil {
		f.Providers = map[string]map[string]burnEntry{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal burn file: %w", err)
	}
	return atomicWriteFile(path, data)
}

func cleanHTMLText(s string) string {
	s = reHTMLTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return strings.TrimSpace(s)
}

func cleanChatGPTModelID(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "(", "")
	name = strings.ReplaceAll(name, ")", "")
	fields := strings.Fields(name)
	return strings.Join(fields, "-")
}

// parseGLMBurn parses the static HTML table from https://docs.z.ai/devpack/overview.
// Returns map[modelID]burnEntry or error on shape drift.
func parseGLMBurn(html string) (map[string]burnEntry, error) {
	tables := reHTMLTable.FindAllStringSubmatch(html, -1)
	scalars := map[string]float64{}
	now := time.Now().UTC().Format(time.RFC3339)

	for _, t := range tables {
		tableHTML := t[1]
		if !strings.Contains(tableHTML, "Product") || !strings.Contains(tableHTML, "Multiplier") {
			continue
		}
		rows := reHTMLTR.FindAllStringSubmatch(tableHTML, -1)
		for _, r := range rows {
			cells := reHTMLTD.FindAllStringSubmatch(r[1], -1)
			if len(cells) == 0 {
				continue
			}
			cleanCells := make([]string, len(cells))
			for i, c := range cells {
				cleanCells[i] = cleanHTMLText(c[1])
			}
			// Skip MCP server rows
			isMCP := false
			for _, c := range cleanCells {
				if strings.Contains(c, "MCP Server") || strings.Contains(c, "Web Search") || strings.Contains(c, "Web Reader") || strings.Contains(c, "Zread") {
					isMCP = true
					break
				}
			}
			if isMCP {
				continue
			}
			var modelName, inpStr, outStr string
			if len(cleanCells) == 5 && strings.Contains(cleanCells[0], "Model") {
				modelName = cleanCells[1]
				inpStr = cleanCells[2]
				outStr = cleanCells[4]
			} else if len(cleanCells) == 4 && strings.Contains(cleanCells[0], "Flash") {
				modelName = cleanCells[0]
				inpStr = cleanCells[1]
				outStr = cleanCells[3]
			} else {
				continue
			}
			if idx := strings.Index(modelName, "\n"); idx != -1 {
				modelName = modelName[:idx]
			}
			if idx := strings.Index(modelName, "("); idx != -1 {
				modelName = modelName[:idx]
			}
			modelID := strings.ToLower(strings.TrimSpace(modelName))
			inp, err1 := strconv.ParseFloat(inpStr, 64)
			out, err2 := strconv.ParseFloat(outStr, 64)
			if err1 != nil || err2 != nil || inp <= 0 || out <= 0 {
				continue
			}
			scalars[modelID] = inp + out
		}
	}

	if len(scalars) == 0 {
		return nil, fmt.Errorf("burn: %s shape drift, 0 rows parsed", providerGLM)
	}

	minScalar := 1e9
	for _, s := range scalars {
		if s < minScalar {
			minScalar = s
		}
	}
	if minScalar <= 0 {
		return nil, fmt.Errorf("burn: %s shape drift, min scalar <= 0", providerGLM)
	}

	out := map[string]burnEntry{}
	for id, s := range scalars {
		weight := s / minScalar
		if weight <= 0 || weight > 10000 {
			return nil, fmt.Errorf("burn: %s shape drift, invalid weight %f for %s", providerGLM, weight, id)
		}
		out[id] = burnEntry{
			Weight:    weight,
			Unit:      "credits/Mtok",
			Source:    "provider",
			SourceURL: "https://docs.z.ai/devpack/overview",
			AsOf:      now,
		}
	}
	return out, nil
}

// parseChatGPTBurn parses the static HTML table from https://learn.chatgpt.com/docs/pricing.
// Returns map[modelID]burnEntry or error on shape drift.
func parseChatGPTBurn(html string) (map[string]burnEntry, error) {
	tables := reHTMLTable.FindAllStringSubmatch(html, -1)
	scalars := map[string]float64{}
	now := time.Now().UTC().Format(time.RFC3339)

	for _, t := range tables {
		tableHTML := t[1]
		if !strings.Contains(tableHTML, "Credits per 1M") && !strings.Contains(tableHTML, "Input Tokens") {
			continue
		}
		rows := reHTMLTR.FindAllStringSubmatch(tableHTML, -1)
		for _, r := range rows {
			cells := reHTMLTD.FindAllStringSubmatch(r[1], -1)
			if len(cells) < 4 {
				continue
			}
			cleanCells := make([]string, len(cells))
			for i, c := range cells {
				cleanCells[i] = cleanHTMLText(c[1])
			}
			rawName := cleanCells[0]
			inpStr := strings.ReplaceAll(strings.ReplaceAll(cleanCells[1], "credits", ""), ",", "")
			outStr := strings.ReplaceAll(strings.ReplaceAll(cleanCells[3], "credits", ""), ",", "")
			inp, err1 := strconv.ParseFloat(strings.TrimSpace(inpStr), 64)
			out, err2 := strconv.ParseFloat(strings.TrimSpace(outStr), 64)
			if err1 != nil || err2 != nil || inp <= 0 || out <= 0 {
				continue
			}
			slug := cleanChatGPTModelID(rawName)
			scalars[slug] = inp + out
			if strings.HasPrefix(slug, "gpt-image-2") {
				if _, ok := scalars["gpt-image-2"]; !ok {
					scalars["gpt-image-2"] = inp + out
				}
			}
		}
	}

	if len(scalars) == 0 {
		return nil, fmt.Errorf("burn: %s shape drift, 0 rows parsed", providerChatGPT)
	}

	minScalar := 1e9
	for _, s := range scalars {
		if s < minScalar {
			minScalar = s
		}
	}
	if minScalar <= 0 {
		return nil, fmt.Errorf("burn: %s shape drift, min scalar <= 0", providerChatGPT)
	}

	out := map[string]burnEntry{}
	for id, s := range scalars {
		weight := s / minScalar
		if weight <= 0 || weight > 10000 {
			return nil, fmt.Errorf("burn: %s shape drift, invalid weight %f for %s", providerChatGPT, weight, id)
		}
		out[id] = burnEntry{
			Weight:    weight,
			Unit:      "credits/Mtok",
			Source:    "provider",
			SourceURL: "https://learn.chatgpt.com/docs/pricing",
			AsOf:      now,
		}
	}
	return out, nil
}

// parseOpenCodeBurn parses the static HTML model rows from https://opencode.ai/go.
// Returns map[modelID]burnEntry or error on shape drift.
func parseOpenCodeBurn(html string) (map[string]burnEntry, error) {
	matches := reOpenCodeRow.FindAllStringSubmatchIndex(html, -1)
	caps := map[string]float64{}
	now := time.Now().UTC().Format(time.RFC3339)

	for _, loc := range matches {
		matchStr := html[loc[0]:loc[1]]
		mSub := reOpenCodeRow.FindStringSubmatch(matchStr)
		if len(mSub) < 2 {
			continue
		}
		modelID := strings.ToLower(strings.TrimSpace(mSub[1]))
		end := loc[1] + 2500
		if end > len(html) {
			end = len(html)
		}
		chunk := html[loc[1]:end]
		allowMatch := reAllowance.FindStringSubmatch(chunk)
		if len(allowMatch) >= 2 {
			capVal, err := strconv.ParseFloat(allowMatch[1], 64)
			if err == nil && capVal > 0 {
				caps[modelID] = capVal
			}
		}
	}

	if len(caps) == 0 {
		return nil, fmt.Errorf("burn: %s shape drift, 0 rows parsed", providerOpenCode)
	}

	maxCap := 0.0
	for _, c := range caps {
		if c > maxCap {
			maxCap = c
		}
	}
	if maxCap <= 0 {
		return nil, fmt.Errorf("burn: %s shape drift, max cap <= 0", providerOpenCode)
	}

	scalars := map[string]float64{}
	minScalar := 1e9
	for id, c := range caps {
		sc := maxCap / c
		scalars[id] = sc
		if sc < minScalar {
			minScalar = sc
		}
	}
	if minScalar <= 0 {
		return nil, fmt.Errorf("burn: %s shape drift, min scalar <= 0", providerOpenCode)
	}

	out := map[string]burnEntry{}
	for id, sc := range scalars {
		weight := sc / minScalar
		if weight <= 0 || weight > 10000 {
			return nil, fmt.Errorf("burn: %s shape drift, invalid weight %f for %s", providerOpenCode, weight, id)
		}
		out[id] = burnEntry{
			Weight:    weight,
			Unit:      "$/mo cap",
			Source:    "provider",
			SourceURL: "https://opencode.ai/go",
			AsOf:      now,
		}
	}
	return out, nil
}

func fetchGLMBurn() (map[string]burnEntry, error) {
	resp, err := http.Get("https://docs.z.ai/devpack/overview")
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", providerGLM, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", providerGLM, err)
	}
	return parseGLMBurn(string(body))
}

func fetchChatGPTBurn() (map[string]burnEntry, error) {
	resp, err := http.Get("https://learn.chatgpt.com/docs/pricing")
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", providerChatGPT, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", providerChatGPT, err)
	}
	return parseChatGPTBurn(string(body))
}

func fetchOpenCodeBurn() (map[string]burnEntry, error) {
	resp, err := http.Get("https://opencode.ai/go")
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", providerOpenCode, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", providerOpenCode, err)
	}
	return parseOpenCodeBurn(string(body))
}

// ---- measured fallback (§S6) ----------------------------------------------
//
// ccusageDailyJSON is the subset of `ccusage daily --breakdown --json
// --offline` output burn.go consumes. Verified shape 2026-09-22: daily[]
// entries key their period and carry modelBreakdowns[]; a breakdown has NO
// totalTokens field, so the per-model total is the sum of its four token
// counts. This is consumption from the local transcript stores, never a
// price, so it satisfies the no-API-$/M hard constraint.

type ccusageBreakdown struct {
	ModelName           string `json:"modelName"`
	InputTokens         int64  `json:"inputTokens"`
	OutputTokens        int64  `json:"outputTokens"`
	CacheCreationTokens int64  `json:"cacheCreationTokens"`
	CacheReadTokens     int64  `json:"cacheReadTokens"`
}

type ccusageDay struct {
	Period          string             `json:"period"`
	ModelBreakdowns []ccusageBreakdown `json:"modelBreakdowns"`
}

type ccusageDailyJSON struct {
	Daily []ccusageDay `json:"daily"`
}

// runCCUsageDaily shells out once to ccusage (§S6). --offline keeps it to
// transcript-store reads, no pricing calls. It is ONLY ever called from the
// background refresh cmd: ccusage can run for minutes and must never sit on
// the picker's foreground path (CCUSAGE-SLOW).
func runCCUsageDaily() ([]byte, error) {
	bin := "ccusage"
	if _, err := exec.LookPath(bin); err != nil {
		// Recorded install path (runbook §S0); LookPath first keeps other
		// machines working.
		bin = "/opt/homebrew/bin/ccusage"
		if _, err := exec.LookPath(bin); err != nil {
			return nil, fmt.Errorf("ccusage not found -- measured burn unavailable")
		}
	}
	out, err := exec.Command(bin, "daily", "--breakdown", "--json", "--offline").Output()
	if err != nil {
		return nil, fmt.Errorf("run %s daily: %w", bin, err)
	}
	return out, nil
}

// fetchMeasuredBurn builds per-provider measured entries from ccusage (§S6).
func fetchMeasuredBurn(cfg *config, entries map[string]cacheEntry) (map[string]map[string]burnEntry, error) {
	out, err := runCCUsageDaily()
	if err != nil {
		return nil, err
	}
	var daily ccusageDailyJSON
	if err := json.Unmarshal(out, &daily); err != nil {
		return nil, fmt.Errorf("parse ccusage daily: %w", err)
	}
	return buildMeasuredBurn(cfg, entries, daily), nil
}

// buildMeasuredBurn aggregates ccusage daily breakdowns per model: scalar =
// total tokens / number of distinct periods the model appears in
// ("days of use"), normalised within each provider so its lightest model is
// exactly 1.0. Model ids are mapped to providers with the EXISTING
// payerForModelID (modelscache.go) -- no second resolver. An id the loaded
// catalog cannot resolve is skipped: there is no provider key to file it
// under, and the picker renders it not-disclosed rather than letting it
// inherit another provider's numbers. The one published Anthropic fact rides
// a Note, never a weight.
func buildMeasuredBurn(cfg *config, entries map[string]cacheEntry, daily ccusageDailyJSON) map[string]map[string]burnEntry {
	type usage struct {
		tokens int64
		days   map[string]bool
	}
	byModel := map[string]*usage{}
	for _, d := range daily.Daily {
		if d.Period == "" {
			continue
		}
		for _, b := range d.ModelBreakdowns {
			if b.ModelName == "" {
				continue
			}
			u := byModel[b.ModelName]
			if u == nil {
				u = &usage{days: map[string]bool{}}
				byModel[b.ModelName] = u
			}
			u.tokens += b.InputTokens + b.OutputTokens + b.CacheCreationTokens + b.CacheReadTokens
			u.days[d.Period] = true
		}
	}
	scalars := map[string]map[string]float64{} // provider -> model -> tokens/day-of-use
	for name, u := range byModel {
		if u.tokens <= 0 || len(u.days) == 0 {
			continue
		}
		provider := payerForModelID(cfg, entries, name)
		if provider == "" {
			continue
		}
		if scalars[provider] == nil {
			scalars[provider] = map[string]float64{}
		}
		scalars[provider][name] = float64(u.tokens) / float64(len(u.days))
	}
	now := time.Now().UTC().Format(time.RFC3339)
	out := map[string]map[string]burnEntry{}
	for provider, models := range scalars {
		min := math.MaxFloat64
		for _, s := range models {
			if s < min {
				min = s
			}
		}
		if min <= 0 {
			continue
		}
		for id, s := range models {
			e := burnEntry{
				Weight: s / min,
				Unit:   "measured tok/day-of-use",
				Source: "measured",
				AsOf:   now,
			}
			switch {
			case strings.Contains(id, "fable"):
				e.Note = "⚠ 50% wk cap" // the one published Anthropic fact
			case strings.Contains(id, "haiku"):
				e.Note = "(effort ignored)"
			}
			if out[provider] == nil {
				out[provider] = map[string]burnEntry{}
			}
			out[provider][id] = e
		}
	}
	return out
}

// mergeMeasuredBurn folds measured entries into dst without overwriting a
// provider-published weight for the same id: published data beats a local
// measurement (§S6).
func mergeMeasuredBurn(dst *burnFile, measured map[string]map[string]burnEntry) {
	for provider, models := range measured {
		if dst.Providers[provider] == nil {
			dst.Providers[provider] = map[string]burnEntry{}
		}
		for id, e := range models {
			if existing, ok := dst.Providers[provider][id]; ok && existing.Source == "provider" {
				continue
			}
			dst.Providers[provider][id] = e
		}
	}
}

// ---- background refresh + staleness (§S8) ---------------------------------

// burnRefreshMsg is what refreshBurnCmd sends back to Update once the
// background burn refresh finishes: burn is the merged next state -- it
// always starts from the previous file, so a failure keeps every stale
// entry rather than blanking the table -- and err names the first failure,
// surfaced in the status bar exactly like the models-cache refresh error.
type burnRefreshMsg struct {
	burn burnFile
	err  string
}

// cloneBurnProviders deep-copies a provider -> model -> entry map: fresh
// outer AND inner maps, so writes into the clone can never reach the source.
func cloneBurnProviders(src map[string]map[string]burnEntry) map[string]map[string]burnEntry {
	if src == nil {
		return map[string]map[string]burnEntry{}
	}
	dst := make(map[string]map[string]burnEntry, len(src))
	for k, inner := range src {
		innerDst := make(map[string]burnEntry, len(inner))
		for ik, iv := range inner {
			innerDst[ik] = iv
		}
		dst[k] = innerDst
	}
	return dst
}

// refreshBurnCmd refreshes whichever halves of the burn cache are stale
// (provider pages and/or ccusage), persists, and returns the merged file.
// It runs off the UI goroutine, mirroring refreshSubscriptionCmd's shape:
// never on the foreground path, failures leave the old data in place.
func refreshBurnCmd(cfg *config, entries map[string]cacheEntry, prev burnFile, providers, measured bool) tea.Cmd {
	// Clone on the caller's (UI) goroutine, BEFORE the closure runs: prev
	// aliases pickModel.burn, which the UI keeps reading (burnCell) while
	// this cmd executes in a Bubbletea goroutine, and `next := prev` below
	// would share these maps with it (review s11 r1, Finding 1).
	clonedProviders := cloneBurnProviders(prev.Providers)
	// Same rule for entries (review s11 r2, Finding 1): it aliases
	// m.cacheEntries, which the UI goroutine keeps writing in place
	// (applyRefreshResult) while this cmd's fetchMeasuredBurn ->
	// payerForModelID ranges it. A shallow copy suffices: cacheEntry.Models
	// slices are only ever replaced whole, never mutated in place.
	var clonedEntries map[string]cacheEntry
	if entries != nil {
		clonedEntries = make(map[string]cacheEntry, len(entries))
		for k, v := range entries {
			clonedEntries[k] = v
		}
	}
	return func() tea.Msg {
		next := prev
		next.Providers = clonedProviders
		var firstErr string
		fail := func(err error) {
			if firstErr == "" {
				firstErr = err.Error()
			}
		}
		if providers {
			fetchers := []struct {
				name string
				fn   func() (map[string]burnEntry, error)
			}{
				{providerGLM, fetchGLMBurn},
				{providerChatGPT, fetchChatGPTBurn},
				{providerOpenCode, fetchOpenCodeBurn},
			}
			ok := 0
			for _, f := range fetchers {
				m, err := f.fn()
				if err != nil {
					// NET-FETCH / SHAPE-DRIFT: keep the cached entries for
					// this provider, surface the error, continue.
					fail(err)
					continue
				}
				next.Providers[f.name] = m
				ok++
			}
			if ok > 0 {
				next.ProviderFetchedAt = time.Now()
			}
		}
		if measured {
			measuredEntries, err := fetchMeasuredBurn(cfg, clonedEntries)
			if err != nil {
				// CCUSAGE-ABSENT / CCUSAGE-SLOW: keep stale measured data.
				fail(err)
			} else {
				mergeMeasuredBurn(&next, measuredEntries)
				next.MeasuredFetchedAt = time.Now()
			}
		}
		if p, err := burnPath(); err != nil {
			fail(err)
		} else if err := saveBurnFile(p, next); err != nil {
			fail(err)
		}
		return burnRefreshMsg{burn: next, err: firstErr}
	}
}

// oldestDataAge returns the age of the oldest data actually present (the
// older of the two fetched-at stamps). Zero when there is no data at all, so
// a cold file never warns as "0d old" -- it simply renders not-disclosed.
func (f burnFile) oldestDataAge(now time.Time) time.Duration {
	oldest := time.Time{}
	for _, at := range []time.Time{f.ProviderFetchedAt, f.MeasuredFetchedAt} {
		if at.IsZero() {
			continue
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return now.Sub(oldest)
}
