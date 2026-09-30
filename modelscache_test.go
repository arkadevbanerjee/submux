package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A stale cache entry is re-fetched when the catalog is built (once per
// picker session), so the list tracks the upstream instead of freezing.
func TestModelsForRouteStaleCacheRefetched(t *testing.T) {
	url, hits := modelsServer(t, []upstreamModel{{ID: "grok-4.6", OwnedBy: "xai"}})
	rt := route{match: "*", upstream: url, authKind: "none"}
	entries := map[string]cacheEntry{
		url: {FetchedAt: time.Now().Add(-3 * time.Hour), Models: []cachedModel{{ID: "stale-id", OwnedBy: "xai"}}},
	}
	now := time.Now()
	models, age, err := modelsForRoute(&config{}, rt, entries, now)
	if err != nil {
		t.Fatalf("modelsForRoute: %v", err)
	}
	if age != "" {
		t.Fatalf("stale entry re-fetched: age = %q, want empty (fresh)", age)
	}
	if len(models) != 1 || models[0].ID != "grok-4.6" {
		t.Fatalf("stale entry re-fetched: models = %v, want the live list", models)
	}
	if *hits != 1 {
		t.Fatalf("stale entry made %d HTTP request(s), want 1", *hits)
	}
	if !entries[url].FetchedAt.Equal(now) {
		t.Fatalf("cache entry not updated after the re-fetch")
	}
}

// A stale entry whose upstream is down is still served, flagged with its age,
// so the list never blanks.
func TestModelsForRouteStaleCacheServedWhenUpstreamDown(t *testing.T) {
	const dead = "http://127.0.0.1:1"
	rt := route{match: "*", upstream: dead, authKind: "none"}
	entries := map[string]cacheEntry{
		dead: {FetchedAt: time.Now().Add(-3 * time.Hour), Models: []cachedModel{{ID: "stale-id", OwnedBy: "xai"}}},
	}
	models, age, err := modelsForRoute(&config{}, rt, entries, time.Now())
	if err != nil {
		t.Fatalf("modelsForRoute: %v", err)
	}
	if age == "" {
		t.Fatalf("stale fallback: expected a non-empty age flag")
	}
	if len(models) != 1 || models[0].ID != "stale-id" {
		t.Fatalf("stale fallback: models = %v, want the stale entry", models)
	}
}

// The picker is for Claude Code: image/video/audio ids and kirocc's
// "kiro/kiro/..." alias never reach the catalog.
func TestCatalogDropsNonCodingAndAliasIDs(t *testing.T) {
	url, _ := modelsServer(t, []upstreamModel{
		{ID: "grok-4.6", OwnedBy: "xai"},
		{ID: "grok-imagine-video-1.5", OwnedBy: "xai"},
		{ID: "grok-imagine-image", OwnedBy: "xai"},
		{ID: "gpt-image-2.5", OwnedBy: "openai"},
		{ID: "kiro/claude-opus-5-5[1m]", OwnedBy: "anthropic"},
		{ID: "kiro/kiro/claude-opus-5-5[1m]", OwnedBy: "anthropic"},
	})
	cfg := &config{
		Routes:        []route{{match: "*", upstream: url, authKind: "none"}},
		Subscriptions: map[string]string{"xai": "SuperGrok", "openai": "ChatGPT", "anthropic": "Kiro"},
	}
	cat := buildSubscriptionCatalog(cfg, map[string]cacheEntry{}, time.Now())
	var all []string
	for _, opt := range cat {
		all = append(all, opt.ModelIDs...)
	}
	want := map[string]bool{"grok-4.6": true, "kiro/claude-opus-5-5[1m]": true}
	if len(all) != len(want) {
		t.Fatalf("catalog ids = %v, want only %v", all, want)
	}
	for _, id := range all {
		if !want[id] {
			t.Fatalf("catalog kept %q, want it dropped", id)
		}
	}
}

// A 1M-context Anthropic id also offers the "[1m]" variant Claude Code accepts;
// a 200k id does not.
func TestCatalogAddsOneMillionVariant(t *testing.T) {
	url, _ := modelsServer(t, []upstreamModel{
		{ID: "claude-opus-5", OwnedBy: "anthropic", MaxInputTokens: 1_000_000},
		{ID: "claude-haiku-4-5", OwnedBy: "anthropic", MaxInputTokens: 200_000},
	})
	cfg := &config{Routes: []route{{match: "claude-*", upstream: url, authKind: "none", subscription: "Claude Max"}}}
	cat := buildSubscriptionCatalog(cfg, map[string]cacheEntry{}, time.Now())
	if len(cat) != 1 {
		t.Fatalf("catalog = %+v, want one bucket", cat)
	}
	got := strings.Join(cat[0].ModelIDs, ",")
	if want := "claude-haiku-4-5,claude-opus-5,claude-opus-5[1m]"; got != want {
		t.Fatalf("ids = %s, want %s", got, want)
	}
}

// An id the relay routes to another route (claude-* goes to Claude Max) is not
// offered under a different subscription that merely lists the same name.
func TestCatalogSkipsIDsRoutedElsewhere(t *testing.T) {
	url, _ := modelsServer(t, []upstreamModel{
		{ID: "claude-sonnet-4-6", OwnedBy: "antigravity"},
		{ID: "gemini-3-flash", OwnedBy: "antigravity"},
	})
	cfg := &config{
		Routes: []route{
			{match: "claude-*", upstream: "http://127.0.0.1:1", authKind: "none", subscription: "Claude Max"},
			{match: "*", upstream: url, authKind: "none"},
		},
		Subscriptions: map[string]string{"antigravity": "Antigravity"},
	}
	entries := map[string]cacheEntry{"http://127.0.0.1:1": {FetchedAt: time.Now()}}
	for _, opt := range buildSubscriptionCatalog(cfg, entries, time.Now()) {
		if opt.Name == "Antigravity" && strings.Join(opt.ModelIDs, ",") != "gemini-3-flash" {
			t.Fatalf("Antigravity ids = %v, want only gemini-3-flash", opt.ModelIDs)
		}
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
