package main

import (
	"fmt"
	"net/url"
)

// globMatch matches s against pattern, treating pattern as an opaque glob
// over the WHOLE string: '*' matches any run of any characters (including
// none, including '/'), '?' matches exactly one character. This is
// deliberately NOT path.Match or filepath.Match: both of those treat '/' as
// a path-segment boundary that '*' may not cross, which silently breaks
// matching for model ids that contain slashes (e.g. "ogo/glm-5",
// "z-ai/glm-5.2"). Model ids are opaque strings here, not paths.
//
// Standard greedy two-pointer wildcard algorithm: walk s and pattern in
// lockstep; on a '*' remember the position and try to match zero characters
// first, backtracking (advancing the remembered star's consumption by one)
// whenever a later mismatch is hit.
func globMatch(pattern, s string) bool {
	var si, pi int
	var starIdx = -1
	var starMatch int

	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]) {
			si++
			pi++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			starMatch = si
			pi++
			continue
		}
		if starIdx != -1 {
			pi = starIdx + 1
			starMatch++
			si = starMatch
			continue
		}
		return false
	}

	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}

	return pi == len(pattern)
}

// route is one entry of the resolved config: a glob pattern, an upstream
// base URL, an auth mode, and an optional model_rewrite map.
type route struct {
	match        string
	upstream     string
	upstreamURL  *url.URL
	auth         string // raw auth string from config, e.g. "bearer:env:VAR"; never a secret
	authKind     string // "passthrough" | "bearer" | "x-api-key" | "none"
	authSource   string // logging-safe description of where the credential came from, e.g. "env:VAR"
	authValue    string // resolved credential; NEVER logged, NEVER printed except redacted in --debug-headers
	modelRewrite map[string]string

	// fallback is a pointer: nil means the key was absent in config (inherit
	// default_fallback), non-nil (possibly pointing at an empty slice) means
	// the route explicitly set its own chain, including an explicit "never
	// fall back". See rawRoute.Fallback in config.go for why this must not
	// collapse to a plain []string.
	fallback *[]string

	// subscription is a FIXED "which subscription pays for this" label for
	// the whole route (§2.1 in the subs spec). Empty means "look it up from
	// the upstream's /v1/models owned_by field instead" (subs.go).
	subscription string
}

// fallbackChain resolves the chain of model ids this route falls back to,
// applying the §9.1 absent-vs-empty distinction.
func (r route) fallbackChain(cfg *config) []string {
	if r.fallback != nil {
		return *r.fallback
	}
	return cfg.DefaultFallback
}

// matchRoute returns the first route in order whose match pattern matches
// modelID, and true. If nothing matches (which should not happen once
// config validation has required a trailing "*" route) it returns the zero
// value and false.
func matchRoute(routes []route, modelID string) (route, bool) {
	for _, r := range routes {
		if globMatch(r.match, modelID) {
			return r, true
		}
	}
	return route{}, false
}

// describeRoute renders a one-line human summary, used by `submux routes`
// and `submux check`. Never includes the resolved credential value. A route
// with a fixed subscription label (§2.6) appends subscription="<name>"; this
// stays an OFFLINE string format, no network call.
func describeRoute(r route) string {
	s := fmt.Sprintf("match=%q upstream=%s auth=%s", r.match, r.upstream, r.auth)
	if r.subscription != "" {
		s += fmt.Sprintf(" subscription=%q", r.subscription)
	}
	return s
}
