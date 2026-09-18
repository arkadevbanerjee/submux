// modelscache.go backs the create-wizard's "pick a model inside this
// subscription" pane (spec §2.4-§2.5): a small disk cache over the same
// upstream /v1/models lookup subs.go already makes, so filtering a model
// list by typing does not fire an HTTP request per keystroke. It never
// runs on the `serve` hot path.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// modelsCacheFreshFor is how long a cache entry is used without a refresh
// (spec §2.5).
const modelsCacheFreshFor = 10 * time.Minute

type cachedModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
}

type cacheEntry struct {
	FetchedAt time.Time     `json:"fetched_at"`
	Models    []cachedModel `json:"models"`
}

type modelsCacheFile struct {
	Entries map[string]cacheEntry `json:"entries"`
}

// modelsCachePath returns ~/.config/submux/models-cache.json.
func modelsCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "submux", "models-cache.json"), nil
}

// loadModelsCache reads path. A missing or corrupt file is not an error --
// it returns an empty cache, matching the profiles.json tolerance (§2.5
// "no cache" is an explicitly handled state, not a failure).
func loadModelsCache(path string) map[string]cacheEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]cacheEntry{}
	}
	var f modelsCacheFile
	if err := json.Unmarshal(data, &f); err != nil || f.Entries == nil {
		return map[string]cacheEntry{}
	}
	return f.Entries
}

// saveModelsCache writes entries to path atomically. Holds no credential
// (§2.5) -- only ids and their owned_by label.
func saveModelsCache(path string, entries map[string]cacheEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(modelsCacheFile{Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal models cache: %w", err)
	}
	return atomicWriteFile(path, data)
}

// subscriptionOption is one selectable entry in the wizard's "pick a
// subscription" pane: either a fixed-label route's configured id list, or a
// live upstream's models grouped by resolved subscription name (§2.2/§2.4).
type subscriptionOption struct {
	Name        string
	ModelIDs    []string
	Unavailable string // non-empty => shown but not selectable
	CacheAge    string // "" (fresh/no cache yet) or "cached 3h ago"

	// Upstreams is the set of route upstream base URLs whose /v1/models
	// feeds this option (empty for a fixed-label route, which has nothing
	// to fetch -- only the caller holds that OAuth). Non-empty is what
	// makes a row refreshable via the 'r' key (DEFECT 1, round 2).
	Upstreams []string
}

// buildSubscriptionCatalog assembles every selectable subscription across
// cfg's routes: fixed-label routes contribute their configured "models"
// list (or an explanatory unavailable entry if none is configured, §2.4),
// and every other route's live /v1/models is fetched-or-cached and grouped
// by subscriptionNameForOwnedBy, merging across routes that resolve to the
// same name. entries is mutated in place with any freshly fetched data;
// callers persist it via saveModelsCache.
func buildSubscriptionCatalog(cfg *config, entries map[string]cacheEntry, now time.Time) []subscriptionOption {
	byName := map[string]*subscriptionOption{}
	var order []string

	add := func(opt subscriptionOption) {
		existing, ok := byName[opt.Name]
		if !ok {
			o := opt
			byName[opt.Name] = &o
			order = append(order, opt.Name)
			return
		}
		if existing.Unavailable == "" {
			existing.ModelIDs = mergeSortedUnique(existing.ModelIDs, opt.ModelIDs)
		} else if opt.Unavailable == "" {
			existing.ModelIDs = opt.ModelIDs
			existing.Unavailable = ""
		}
		existing.Upstreams = mergeSortedUnique(existing.Upstreams, opt.Upstreams)
	}

	for _, rt := range cfg.Routes {
		if rt.subscription != "" {
			if len(rt.models) == 0 {
				add(subscriptionOption{
					Name:        rt.subscription,
					Unavailable: `no model list configured for this subscription -- add "models": [...] to its route`,
				})
				continue
			}
			ids := append([]string(nil), rt.models...)
			sort.Strings(ids)
			add(subscriptionOption{Name: rt.subscription, ModelIDs: ids})
			continue
		}

		models, cacheAge, err := modelsForRoute(cfg, rt, entries, now)
		if err != nil {
			add(subscriptionOption{Name: routeUnavailableName(rt), Unavailable: err.Error(), Upstreams: []string{rt.upstream}})
			continue
		}
		byGroup := map[string][]string{}
		for _, m := range models {
			name, _ := subscriptionNameForOwnedBy(cfg, m.ID, m.OwnedBy)
			byGroup[name] = append(byGroup[name], m.ID)
		}
		names := make([]string, 0, len(byGroup))
		for n := range byGroup {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ids := byGroup[n]
			sort.Strings(ids)
			add(subscriptionOption{Name: n, ModelIDs: ids, CacheAge: cacheAge, Upstreams: []string{rt.upstream}})
		}
	}

	sort.Strings(order)
	out := make([]subscriptionOption, 0, len(order))
	seen := map[string]bool{}
	for _, n := range order {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, *byName[n])
	}
	return out
}

// routeUnavailableName labels a subscription bucket that could not be
// resolved at all (upstream unreachable, no cache) so it still shows up in
// the list rather than silently vanishing (spec §2.4 "the wizard stays
// usable for the others").
func routeUnavailableName(rt route) string {
	return fmt.Sprintf("(%s)", rt.upstream)
}

// modelsForRoute returns rt's model list, using entries[rt.upstream] if
// fresh, falling back to a stale-but-present entry immediately (flagging
// its age), and only hitting the network when there is no usable cache at
// all. entries is updated in place on a successful fetch.
func modelsForRoute(cfg *config, rt route, entries map[string]cacheEntry, now time.Time) ([]cachedModel, string, error) {
	if e, ok := entries[rt.upstream]; ok {
		age := now.Sub(e.FetchedAt)
		if age < modelsCacheFreshFor {
			return e.Models, "", nil
		}
		// Stale but present: used IMMEDIATELY, no network call, per §2.5 --
		// "a spinner on every keystroke is not nice". A manual refresh is a
		// distinct, explicit action ('r'), not implied by staleness.
		return e.Models, relativeTime(e.FetchedAt.Format(time.RFC3339), now), nil
	}

	fresh, err := fetchAndConvert(rt)
	if err != nil {
		return nil, "", err
	}
	entries[rt.upstream] = cacheEntry{FetchedAt: now, Models: fresh}
	return fresh, "", nil
}

func fetchAndConvert(rt route) ([]cachedModel, error) {
	if err := resolveRouteCredential(&rt); err != nil {
		return nil, err
	}
	models, err := fetchUpstreamModels(rt)
	if err != nil {
		return nil, err
	}
	out := make([]cachedModel, 0, len(models))
	for _, m := range models {
		out = append(out, cachedModel{ID: m.ID, OwnedBy: m.OwnedBy})
	}
	return out, nil
}

// refreshResultMsg is what refreshSubscriptionCmd sends back to Update once
// every upstream backing one subscription option has been re-fetched
// (DEFECT 1, round 2: the 'r' key). subName lets the caller re-locate the
// row after a catalog rebuild even if sort order shifted.
type refreshResultMsg struct {
	subName         string
	upstreams       map[string]cacheEntry // upstream -> freshly fetched entry, success only
	failedUpstreams []string
	firstErr        error
}

// refreshSubscriptionCmd re-fetches every upstream in upstreams directly,
// bypassing modelsForRoute's 10-minute freshness window on purpose -- this
// is the explicit manual refresh the spec carves out as the ONLY way to
// force a re-fetch (staleness alone never fires one). It runs off the UI
// goroutine; failures leave entries untouched so the caller can keep
// serving the stale data (spec: "never blank the list").
func refreshSubscriptionCmd(cfg *config, subName string, upstreams []string) tea.Cmd {
	return func() tea.Msg {
		result := refreshResultMsg{subName: subName, upstreams: map[string]cacheEntry{}}
		for _, up := range upstreams {
			rt, ok := findRouteByUpstream(cfg, up)
			if !ok {
				result.failedUpstreams = append(result.failedUpstreams, up)
				if result.firstErr == nil {
					result.firstErr = fmt.Errorf("no route configured for %s", up)
				}
				continue
			}
			fresh, err := fetchAndConvert(rt)
			if err != nil {
				result.failedUpstreams = append(result.failedUpstreams, up)
				if result.firstErr == nil {
					result.firstErr = err
				}
				continue
			}
			result.upstreams[up] = cacheEntry{FetchedAt: time.Now(), Models: fresh}
		}
		return result
	}
}

// findRouteByUpstream locates the (non-fixed-label) route serving upstream,
// so refreshSubscriptionCmd can resolve its credential. Fixed-label routes
// are never returned: they have nothing to fetch.
func findRouteByUpstream(cfg *config, upstream string) (route, bool) {
	for _, rt := range cfg.Routes {
		if rt.subscription == "" && rt.upstream == upstream {
			return rt, true
		}
	}
	return route{}, false
}

// payerForModelID answers "who pays for this id" (DEFECT 2, round 2: the
// history screen's expanded row) by reusing subscriptionNameForOwnedBy
// against the ALREADY-LOADED cached catalogue -- entries came from
// loadModelsCache/ensureCatalog, never a fresh network call per keystroke.
// A fixed-label route's configured "models" list resolves directly (its
// subscription name IS the payer, no owned_by involved). Returns "" if id
// is not found in any route's config or any cached upstream entry.
func payerForModelID(cfg *config, entries map[string]cacheEntry, id string) string {
	if id == "" {
		return ""
	}
	for _, rt := range cfg.Routes {
		if rt.subscription == "" {
			continue
		}
		for _, m := range rt.models {
			if m == id {
				return rt.subscription
			}
		}
	}
	for _, entry := range entries {
		for _, m := range entry.Models {
			if m.ID == id {
				name, _ := subscriptionNameForOwnedBy(cfg, id, m.OwnedBy)
				return name
			}
		}
	}
	return ""
}

func mergeSortedUnique(a, b []string) []string {
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	out := append([]string(nil), a...)
	for _, x := range b {
		if !seen[x] {
			out = append(out, x)
			seen[x] = true
		}
	}
	sort.Strings(out)
	return out
}
