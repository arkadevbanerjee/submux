package main

import (
	"testing"
	"time"
)

func testConfigForCatalog(upstream string) *config {
	return &config{
		Routes: []route{
			{match: "claude-*", upstream: "https://api.anthropic.com", subscription: "Claude Max", models: []string{"claude-opus-5", "claude-sonnet-5"}},
			{match: "*", upstream: upstream, authKind: "none"},
		},
	}
}

// Test spec §4.6: a fixed-label route with "models" configured appears as a
// selectable subscription with those ids; without the key it appears with
// the explanatory line and is not selectable (Unavailable is set).
func TestCatalogFixedLabelRouteWithAndWithoutModels(t *testing.T) {
	url, _ := modelsServer(t, nil)
	cfg := testConfigForCatalog(url)
	entries := map[string]cacheEntry{}
	cat := buildSubscriptionCatalog(cfg, entries, time.Now())

	var maxOpt *subscriptionOption
	for i := range cat {
		if cat[i].Name == "Claude Max" {
			maxOpt = &cat[i]
		}
	}
	if maxOpt == nil {
		t.Fatalf("catalog missing 'Claude Max' subscription: %+v", cat)
	}
	if maxOpt.Unavailable != "" {
		t.Fatalf("configured models: Unavailable = %q, want empty", maxOpt.Unavailable)
	}
	if len(maxOpt.ModelIDs) != 2 {
		t.Fatalf("configured models: ModelIDs = %v, want 2 entries", maxOpt.ModelIDs)
	}

	cfg2 := testConfigForCatalog(url)
	cfg2.Routes[0].models = nil
	cat2 := buildSubscriptionCatalog(cfg2, map[string]cacheEntry{}, time.Now())
	var maxOpt2 *subscriptionOption
	for i := range cat2 {
		if cat2[i].Name == "Claude Max" {
			maxOpt2 = &cat2[i]
		}
	}
	if maxOpt2 == nil {
		t.Fatalf("catalog missing 'Claude Max' subscription: %+v", cat2)
	}
	if maxOpt2.Unavailable == "" {
		t.Fatalf("no models configured: expected Unavailable to explain why, got empty")
	}
}

// Test spec §4.7 part 1: a fresh cache hit does no HTTP call at all.
func TestModelsForRouteFreshCacheDoesNoHTTP(t *testing.T) {
	url, hits := modelsServer(t, []upstreamModel{{ID: "grok-4.6", OwnedBy: "xai"}})
	rt := route{match: "*", upstream: url, authKind: "none"}
	entries := map[string]cacheEntry{
		url: {FetchedAt: time.Now(), Models: []cachedModel{{ID: "cached-id", OwnedBy: "xai"}}},
	}
	models, age, err := modelsForRoute(&config{}, rt, entries, time.Now())
	if err != nil {
		t.Fatalf("modelsForRoute: %v", err)
	}
	if age != "" {
		t.Fatalf("fresh cache hit: age = %q, want empty (not flagged)", age)
	}
	if len(models) != 1 || models[0].ID != "cached-id" {
		t.Fatalf("fresh cache hit: models = %v, want the cached entry unchanged", models)
	}
	if got := *hits; got != 0 {
		t.Fatalf("fresh cache hit made %d HTTP request(s), want 0", got)
	}
}

// Test spec §4.7 part 2: a stale cache entry is used immediately (no HTTP
// call either -- staleness alone never fires a request) and flagged with
// its age.
func TestModelsForRouteStaleCacheUsedAndFlagged(t *testing.T) {
	url, hits := modelsServer(t, []upstreamModel{{ID: "grok-4.6", OwnedBy: "xai"}})
	rt := route{match: "*", upstream: url, authKind: "none"}
	staleAt := time.Now().Add(-3 * time.Hour)
	entries := map[string]cacheEntry{
		url: {FetchedAt: staleAt, Models: []cachedModel{{ID: "stale-id", OwnedBy: "xai"}}},
	}
	models, age, err := modelsForRoute(&config{}, rt, entries, time.Now())
	if err != nil {
		t.Fatalf("modelsForRoute: %v", err)
	}
	if age == "" {
		t.Fatalf("stale cache hit: expected a non-empty age flag")
	}
	if len(models) != 1 || models[0].ID != "stale-id" {
		t.Fatalf("stale cache hit: models = %v, want the stale entry served as-is", models)
	}
	if got := *hits; got != 0 {
		t.Fatalf("stale cache hit made %d HTTP request(s), want 0 (staleness alone never fires a request)", got)
	}
}

// Test spec §4.7 part 3: no cache + a dead upstream degrades only that one
// subscription -- buildSubscriptionCatalog still returns the OTHER
// (fixed-label) subscription untouched.
func TestCatalogDeadUpstreamDegradesOnlyThatSubscription(t *testing.T) {
	cfg := &config{
		Routes: []route{
			{match: "claude-*", upstream: "https://api.anthropic.com", subscription: "Claude Max", models: []string{"claude-opus-5"}},
			{match: "*", upstream: "http://127.0.0.1:1", authKind: "none"}, // nothing listens here
		},
	}
	cat := buildSubscriptionCatalog(cfg, map[string]cacheEntry{}, time.Now())

	var maxOK, deadFound bool
	for _, opt := range cat {
		if opt.Name == "Claude Max" && opt.Unavailable == "" {
			maxOK = true
		}
		if opt.Unavailable != "" {
			deadFound = true
		}
	}
	if !maxOK {
		t.Fatalf("dead upstream should not affect the fixed-label subscription: %+v", cat)
	}
	if !deadFound {
		t.Fatalf("dead upstream's subscription should show as unavailable: %+v", cat)
	}
}
