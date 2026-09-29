package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testConfigForCatalog(upstream string) *config {
	return &config{
		Routes: []route{
			{match: "claude-*", upstream: upstream, subscription: "Claude Max", authKind: "none"},
		},
	}
}

// A fixed-label route's ids come from its live /v1/models, all filed under
// the label regardless of owned_by. Nothing is configured: a model the
// upstream adds shows up, one it drops disappears.
func TestCatalogFixedLabelRouteListsLiveModels(t *testing.T) {
	url, hits := modelsServer(t, []upstreamModel{{ID: "claude-opus-5-5", OwnedBy: "anthropic"}, {ID: "claude-new-6", OwnedBy: "whoever"}})
	cfg := testConfigForCatalog(url)
	cat := buildSubscriptionCatalog(cfg, map[string]cacheEntry{}, time.Now())
	if len(cat) != 1 || cat[0].Name != "Claude Max" || cat[0].Unavailable != "" {
		t.Fatalf("catalog = %+v, want one selectable 'Claude Max' row", cat)
	}
	if got := cat[0].ModelIDs; len(got) != 2 || got[0] != "claude-new-6" || got[1] != "claude-opus-5-5" {
		t.Fatalf("ModelIDs = %v, want the live list [claude-new-6 claude-opus-5-5]", got)
	}
	if len(cat[0].Upstreams) != 1 {
		t.Fatalf("Upstreams = %v, want the route upstream so 'r' can refresh it", cat[0].Upstreams)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}
}

// A passthrough route lists Anthropic's catalogue with the local Claude Code
// login and follows has_more/last_id paging.
func TestFetchUpstreamModelsPassthroughUsesLoginAndPages(t *testing.T) {
	old := claudeLoginToken
	claudeLoginToken = func() (string, error) { return "tok-123", nil }
	t.Cleanup(func() { claudeLoginToken = old })

	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization")+"|"+r.Header.Get("anthropic-beta"))
		if r.URL.Query().Get("after_id") == "" {
			fmt.Fprint(w, `{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"claude-b"}],"has_more":false,"last_id":"claude-b"}`)
	}))
	t.Cleanup(srv.Close)

	got, err := fetchUpstreamModels(route{upstream: srv.URL, authKind: "passthrough"})
	if err != nil {
		t.Fatalf("fetchUpstreamModels: %v", err)
	}
	if len(got) != 2 || got[0].ID != "claude-a" || got[1].ID != "claude-b" {
		t.Fatalf("models = %+v, want both pages", got)
	}
	for _, a := range auths {
		if a != "Bearer tok-123|oauth-2025-04-20" {
			t.Fatalf("request auth = %q, want the login token with the oauth beta", a)
		}
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
	okURL, _ := modelsServer(t, []upstreamModel{{ID: "claude-opus-5-5"}})
	cfg := &config{
		Routes: []route{
			{match: "claude-*", upstream: okURL, subscription: "Claude Max", authKind: "none"},
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
