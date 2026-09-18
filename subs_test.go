package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// modelsServer spins up an httptest server that answers GET /v1/models with
// the given entries, and counts how many requests it received.
func modelsServer(t *testing.T, entries []upstreamModel) (url string, hits *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(upstreamModelsResponse{Data: entries})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// Test 1: an override beats owned_by, even when owned_by reports a
// protocol name ("anthropic") that isn't the actual payer.
func TestResolveSubscriptionOverrideBeatsOwnedBy(t *testing.T) {
	url, _ := modelsServer(t, []upstreamModel{{ID: "glm-5.3-flash", OwnedBy: "anthropic"}})
	cfg := &config{
		Subscriptions:         map[string]string{"anthropic": "Claude Max"},
		SubscriptionOverrides: []subscriptionOverride{{match: "glm-*", subscription: "GLM Coding Plan (z.ai)"}},
	}
	rt := route{match: "*", upstream: url, authKind: "none"}

	res := resolveSubscription(cfg, rt, "glm-5.3-flash")
	if res.Status != "resolved" || res.Name != "GLM Coding Plan (z.ai)" {
		t.Fatalf("resolveSubscription = %+v, want the override to beat owned_by=\"anthropic\"", res)
	}
}

// Test 2: the override glob must use the SAME slash-crossing matcher the
// router uses, or "ogo/*" would never match "ogo/grok-4.5".
func TestResolveSubscriptionSlashGlobOverride(t *testing.T) {
	url, _ := modelsServer(t, []upstreamModel{{ID: "ogo/grok-4.5", OwnedBy: "openai"}})
	cfg := &config{
		SubscriptionOverrides: []subscriptionOverride{{match: "ogo/*", subscription: "OpenCode Go"}},
	}
	rt := route{match: "*", upstream: url, authKind: "none"}

	res := resolveSubscription(cfg, rt, "ogo/grok-4.5")
	if res.Status != "resolved" || res.Name != "OpenCode Go" {
		t.Fatalf("resolveSubscription = %+v, want \"ogo/*\" to match \"ogo/grok-4.5\" and beat owned_by=\"openai\"", res)
	}
}

// Test 3: an owned_by map hit resolves correctly, and an owned_by value
// with NO map entry prints raw + "(unmapped)" rather than silently
// guessing.
func TestResolveSubscriptionOwnedByMapAndUnmapped(t *testing.T) {
	url, _ := modelsServer(t, []upstreamModel{
		{ID: "grok-4.6", OwnedBy: "xai"},
		{ID: "mystery-model", OwnedBy: "mystery-vendor"},
	})
	cfg := &config{Subscriptions: map[string]string{"xai": "SuperGrok"}}
	rt := route{match: "*", upstream: url, authKind: "none"}

	mapped := resolveSubscription(cfg, rt, "grok-4.6")
	if mapped.Status != "resolved" || mapped.Name != "SuperGrok" {
		t.Fatalf("resolveSubscription(grok-4.6) = %+v, want SuperGrok", mapped)
	}

	unmapped := resolveSubscription(cfg, rt, "mystery-model")
	if unmapped.Status != "resolved" || unmapped.Name != "mystery-vendor (unmapped)" {
		t.Fatalf("resolveSubscription(mystery-model) = %+v, want raw owned_by + \"(unmapped)\"", unmapped)
	}
}

// Test 4: a route with a fixed subscription label makes NO HTTP request at
// all -- the passthrough route answers from its config label alone, because
// only the caller holds that OAuth.
func TestResolveSubscriptionFixedRouteNoNetworkCall(t *testing.T) {
	url, hits := modelsServer(t, []upstreamModel{{ID: "claude-opus-5", OwnedBy: "anthropic"}})
	cfg := &config{}
	rt := route{
		match:        "claude-*",
		upstream:     url,
		authKind:     "passthrough",
		subscription: "Claude Max (this CLI's own login)",
	}

	res := resolveSubscription(cfg, rt, "claude-opus-5")
	if res.Status != "fixed" || res.Name != "Claude Max (this CLI's own login)" {
		t.Fatalf("resolveSubscription = %+v, want the fixed route label", res)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("upstream received %d request(s), want 0 for a fixed-label route", got)
	}
}

// Test 5: an id absent from the upstream's catalogue is NOT SERVED, with
// non-empty near-miss suggestions (the whole point: catch a typo before a
// session launches on it).
func TestResolveSubscriptionNotServed(t *testing.T) {
	url, _ := modelsServer(t, []upstreamModel{
		{ID: "grok-4.6", OwnedBy: "xai"},
		{ID: "grok-4.20-0309-non-reasoning", OwnedBy: "xai"},
	})
	cfg := &config{}
	rt := route{match: "*", upstream: url, authKind: "none"}

	res := resolveSubscription(cfg, rt, "grok-4")
	if res.Status != "not_served" {
		t.Fatalf("resolveSubscription status = %q, want not_served", res.Status)
	}
	if len(res.Suggestions) == 0 {
		t.Fatalf("resolveSubscription: expected non-empty suggestions for a near-miss id, got none")
	}

	// The commonest typo shape: the operator ADDED characters onto a real
	// id ("grok-4.60" for "grok-4.6"). That query is never a substring of
	// anything, so a one-directional prefix/contains check misses it
	// entirely (ROUND=2 bug report).
	added := resolveSubscription(cfg, rt, "grok-4.60")
	if added.Status != "not_served" {
		t.Fatalf("resolveSubscription status = %q, want not_served", added.Status)
	}
	found := false
	for _, s := range added.Suggestions {
		if s == "grok-4.6" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolveSubscription(%q).Suggestions = %v, want \"grok-4.6\" among them", "grok-4.60", added.Suggestions)
	}
}

// Test 6: an upstream 500 and an unreachable upstream both come back
// UNKNOWN with the error surfaced, never a panic.
func TestResolveSubscriptionUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg := &config{}

	res500 := resolveSubscription(cfg, route{match: "*", upstream: srv.URL, authKind: "none"}, "anything")
	if res500.Status != "unknown" || res500.Err == nil {
		t.Fatalf("resolveSubscription (500) = %+v, want unknown with a non-nil error", res500)
	}

	resUnreachable := resolveSubscription(cfg, route{match: "*", upstream: "http://127.0.0.1:1", authKind: "none"}, "anything")
	if resUnreachable.Status != "unknown" || resUnreachable.Err == nil {
		t.Fatalf("resolveSubscription (unreachable) = %+v, want unknown with a non-nil error", resUnreachable)
	}
}

// Test 7: a config using NONE of the new keys still loads and behaves
// exactly as it did before this feature existed.
func TestLoadConfigWithoutSubscriptionKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	plain := `{
		"listen": "127.0.0.1:0",
		"routes": [{"match": "*", "upstream": "http://127.0.0.1:1", "auth": "none"}]
	}`
	if err := os.WriteFile(path, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: unexpected error: %v", err)
	}
	if len(cfg.Subscriptions) != 0 {
		t.Errorf("cfg.Subscriptions = %v, want empty", cfg.Subscriptions)
	}
	if len(cfg.SubscriptionOverrides) != 0 {
		t.Errorf("cfg.SubscriptionOverrides = %v, want empty", cfg.SubscriptionOverrides)
	}
	if cfg.Routes[0].subscription != "" {
		t.Errorf("cfg.Routes[0].subscription = %q, want empty", cfg.Routes[0].subscription)
	}
}
