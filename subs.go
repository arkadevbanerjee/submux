// subs.go answers "which subscription pays for this model id", the one
// question the relay itself cannot: one upstream address
// (http://127.0.0.1:8317) fronts several different paid subscriptions, and
// the aggregator's own /v1/models owned_by field sometimes names the
// PROTOCOL it speaks rather than the account it bills to (see §1 of the
// spec this file implements). This is a read-only diagnostic path used by
// `submux check` and `submux models`; it never runs on the `serve` hot path
// and never caches anything to disk.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// subscriptionOverride is the resolved form of rawSubscriptionOverride
// (config.go): a glob over model ids, matched with the SAME hand-rolled
// globMatch the router uses (router.go), because model ids from some
// aggregators contain slashes (e.g. "ogo/glm-5.3") that path.Match would
// refuse to let '*' cross.
type subscriptionOverride struct {
	match        string
	subscription string
}

// subscriptionResult is what resolveSubscription returns for one (route,
// model id) pair.
type subscriptionResult struct {
	// Status is one of "fixed" (route carries a fixed label, no network
	// call), "resolved" (an upstream /v1/models lookup found the id and
	// named a subscription, mapped or "(unmapped)"), "not_served" (the
	// upstream's catalogue does not contain this id), or "unknown"
	// (upstream unreachable, credential missing, or a non-2xx response).
	Status      string
	Name        string
	Evidence    string
	Suggestions []string // populated only for "not_served"
	Err         error    // populated only for "unknown"
}

// upstreamModel is the one shape this file cares about out of an
// OpenAI-style /v1/models response entry.
type upstreamModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
}

type upstreamModelsResponse struct {
	Data []upstreamModel `json:"data"`
}

// resolveSubscription implements the §2.2 resolution order:
//  1. a fixed route label wins outright, no network call (this is how the
//     claude-* passthrough route answers: only the caller holds that OAuth,
//     submux cannot list Anthropic's catalogue and must not try).
//  2. else fetch the route's own upstream /v1/models with the route's own
//     credential and look for modelID by exact id match.
//  3. an override glob beats the owned_by map, which beats a raw,
//     "(unmapped)"-flagged owned_by value.
//
// rt is passed by value and its authValue is resolved locally (via
// resolveRouteCredential) if needed; callers never need cfg.Routes mutated
// for this.
func resolveSubscription(cfg *config, rt route, modelID string) subscriptionResult {
	if rt.subscription != "" {
		return subscriptionResult{
			Status:   "fixed",
			Name:     rt.subscription,
			Evidence: "route label, no upstream lookup (passthrough forwards your own token)",
		}
	}

	if err := resolveRouteCredential(&rt); err != nil {
		return subscriptionResult{Status: "unknown", Err: fmt.Errorf("resolve credential: %w", err)}
	}

	models, err := fetchUpstreamModels(rt)
	if err != nil {
		return subscriptionResult{Status: "unknown", Err: err}
	}

	var ownedBy string
	found := false
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
		if m.ID == modelID {
			ownedBy, found = m.OwnedBy, true
		}
	}
	if !found {
		return subscriptionResult{Status: "not_served", Suggestions: nearestIDs(modelID, ids, 5)}
	}

	name, evidence := subscriptionNameForOwnedBy(cfg, modelID, ownedBy)
	return subscriptionResult{Status: "resolved", Name: name, Evidence: evidence}
}

// subscriptionNameForOwnedBy applies §2.2 step 3: an override glob beats the
// owned_by map, which beats a raw "(unmapped)"-flagged owned_by value. Used
// both by resolveSubscription (which also needs the evidence line) and by
// cmdModels (which only needs the name, for bucketing).
func subscriptionNameForOwnedBy(cfg *config, modelID, ownedBy string) (name, evidence string) {
	for _, ov := range cfg.SubscriptionOverrides {
		if globMatch(ov.match, modelID) {
			return ov.subscription, fmt.Sprintf("override match=%q  (upstream owned_by=%q)", ov.match, ownedBy)
		}
	}
	if mapped, ok := cfg.Subscriptions[ownedBy]; ok {
		return mapped, fmt.Sprintf("upstream /v1/models owned_by=%q", ownedBy)
	}
	return fmt.Sprintf("%s (unmapped)", ownedBy),
		fmt.Sprintf("upstream /v1/models owned_by=%q, no subscriptions mapping configured", ownedBy)
}

// fetchUpstreamModels performs the ONE network call this feature makes: a
// GET <route upstream>/v1/models with a 5s timeout, using the route's own
// resolved credential. Never called for a route with a fixed subscription
// label (§2.1) -- resolveSubscription short-circuits before this.
func fetchUpstreamModels(rt route) ([]upstreamModel, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	url := strings.TrimRight(rt.upstream, "/") + "/v1/models"

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	switch rt.authKind {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+rt.authValue)
	case "x-api-key":
		req.Header.Set("x-api-key", rt.authValue)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	var parsed upstreamModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", url, err)
	}
	return parsed.Data, nil
}

// nearestIDs returns up to limit ids from ids that look like a typo of
// query: ids with query as a prefix first, then ids merely containing query,
// each group sorted for stable output. Plain substring/prefix on purpose
// (§2.4) -- this exists to catch an operator's typo before a session
// launches on it, not to do fuzzy ranking.
func nearestIDs(query string, ids []string, limit int) []string {
	var prefix, substr []string
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, query):
			prefix = append(prefix, id)
		case strings.Contains(id, query):
			substr = append(substr, id)
		}
	}
	sort.Strings(prefix)
	sort.Strings(substr)
	out := append(prefix, substr...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
