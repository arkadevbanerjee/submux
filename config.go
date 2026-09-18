package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultMaxBodyBytes = 64 * 1024 * 1024

// defaultFallbackStatusCodes and defaultCooldown apply when the
// corresponding config key is absent (§9.1).
var defaultFallbackStatusCodes = []int{429, 402, 403, 500, 502, 503, 529}

const defaultCooldown = 300 * time.Second

// rawConfig is the on-disk JSON shape.
type rawConfig struct {
	Listen                 string     `json:"listen"`
	MaxBodyBytes           int64      `json:"max_body_bytes"`
	DefaultFallback        []string   `json:"default_fallback,omitempty"`
	FallbackStatusCodes    []int      `json:"fallback_status_codes,omitempty"`
	CooldownDefaultSeconds int        `json:"cooldown_default_seconds,omitempty"`
	NoModelRoute           string     `json:"no_model_route,omitempty"`
	Routes                 []rawRoute `json:"routes"`
	// Subscriptions and SubscriptionOverrides are both OPTIONAL: a config
	// naming neither loads and behaves exactly as it did before "which
	// subscription pays for this" existed. See subs.go.
	Subscriptions         map[string]string         `json:"subscriptions,omitempty"`
	SubscriptionOverrides []rawSubscriptionOverride `json:"subscription_overrides,omitempty"`
}

type rawRoute struct {
	Match        string            `json:"match"`
	Upstream     string            `json:"upstream"`
	Auth         string            `json:"auth"`
	ModelRewrite map[string]string `json:"model_rewrite,omitempty"`
	// Fallback is a pointer so a config can distinguish the key being
	// ABSENT (nil -- inherit default_fallback) from present-and-empty
	// ("fallback": [] -- never fall back on this route, fail loud). A
	// len(x) == 0 check on a plain []string would collapse both cases and
	// silently disable an owner's explicit opt-out (§9.1).
	Fallback *[]string `json:"fallback,omitempty"`
	// Subscription (optional) is a FIXED label for the whole route: no
	// upstream /v1/models lookup is ever made for it. This is how the
	// claude-* passthrough route answers "which subscription" -- nobody but
	// the caller holds that OAuth, so submux cannot list Anthropic's
	// catalogue and must not try (see subs.go).
	Subscription string `json:"subscription,omitempty"`
	// Models is an OPTIONAL fixed id list for a route that also carries
	// Subscription (the claude-* passthrough route, whose upstream
	// /v1/models submux cannot call -- only the caller holds that OAuth,
	// spec §2.4). Ignored on a route with no Subscription: such a route's
	// model list always comes from a live upstream lookup (subs.go,
	// modelscache.go).
	Models []string `json:"models,omitempty"`
}

// rawSubscriptionOverride is one entry of the on-disk "subscription_overrides"
// list: a glob over model ids, evaluated in order, BEFORE the owned_by map,
// so a known-lying owned_by value (e.g. glm-5.3 reporting "anthropic") can be
// corrected without waiting on the aggregator to fix its own label.
type rawSubscriptionOverride struct {
	Match        string `json:"match"`
	Subscription string `json:"subscription"`
}

// config is the resolved, validated form the server actually runs on.
type config struct {
	Listen              string
	MaxBodyBytes        int64
	DefaultFallback     []string
	FallbackStatusCodes []int
	CooldownDefault     time.Duration
	Routes              []route

	// Subscriptions maps an upstream /v1/models "owned_by" value to a
	// human subscription name (§2.1). Nil/empty is valid: `submux check`
	// then prints the raw owned_by value with an "(unmapped)" marker
	// instead of silently guessing.
	Subscriptions map[string]string
	// SubscriptionOverrides is evaluated, in order, BEFORE Subscriptions.
	SubscriptionOverrides []subscriptionOverride

	// NoModelRoute is the route a request with no "model" field (or a
	// non-JSON body) is sent to, resolved once at load time. nil means no
	// override applies: such requests fall through to the ordinary
	// matchRoute(routes, "") lookup, i.e. today's behaviour (the "*" route,
	// if one is configured).
	NoModelRoute *route
}

// defaultConfigPath returns ~/.config/submux/config.json for the current user.
func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "submux", "config.json"), nil
}

// loadConfig reads, parses, resolves credentials for, and validates the
// config at path. On success every route's authValue is populated (for
// non-passthrough/non-none routes) and ready to use.
func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg := &config{
		Listen:       raw.Listen,
		MaxBodyBytes: raw.MaxBodyBytes,
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8787"
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	cfg.DefaultFallback = raw.DefaultFallback
	if len(raw.FallbackStatusCodes) > 0 {
		cfg.FallbackStatusCodes = raw.FallbackStatusCodes
	} else {
		cfg.FallbackStatusCodes = defaultFallbackStatusCodes
	}
	if raw.CooldownDefaultSeconds > 0 {
		cfg.CooldownDefault = time.Duration(raw.CooldownDefaultSeconds) * time.Second
	} else {
		cfg.CooldownDefault = defaultCooldown
	}

	cfg.Subscriptions = raw.Subscriptions
	for i, ov := range raw.SubscriptionOverrides {
		if ov.Match == "" {
			return nil, fmt.Errorf("config %s: subscription_overrides[%d]: match must not be empty", path, i)
		}
		if ov.Subscription == "" {
			return nil, fmt.Errorf("config %s: subscription_overrides[%d] (match=%q): subscription must not be empty", path, i, ov.Match)
		}
		cfg.SubscriptionOverrides = append(cfg.SubscriptionOverrides, subscriptionOverride{match: ov.Match, subscription: ov.Subscription})
	}

	if len(raw.Routes) == 0 {
		return nil, fmt.Errorf("config %s: routes must not be empty", path)
	}

	for i, rr := range raw.Routes {
		if rr.Match == "" {
			return nil, fmt.Errorf("config %s: routes[%d]: match must not be empty", path, i)
		}
		if rr.Match == "*" && i != len(raw.Routes)-1 {
			return nil, fmt.Errorf("config %s: routes[%d]: a \"*\" route must be last (it is the fallback); found one at position %d of %d", path, i, i, len(raw.Routes))
		}
		if rr.Upstream == "" {
			return nil, fmt.Errorf("config %s: routes[%d] (match=%q): upstream must not be empty", path, i, rr.Match)
		}
		upstreamURL, err := url.Parse(rr.Upstream)
		if err != nil {
			return nil, fmt.Errorf("config %s: routes[%d] (match=%q): invalid upstream %q: %w", path, i, rr.Match, rr.Upstream, err)
		}

		r := route{
			match:        rr.Match,
			upstream:     rr.Upstream,
			upstreamURL:  upstreamURL,
			auth:         rr.Auth,
			modelRewrite: rr.ModelRewrite,
			fallback:     rr.Fallback,
			subscription: rr.Subscription,
			models:       rr.Models,
		}

		kind, source, err := parseAuth(rr.Auth)
		if err != nil {
			return nil, fmt.Errorf("config %s: routes[%d] (match=%q): %w", path, i, rr.Match, err)
		}
		r.authKind = kind
		r.authSource = source

		cfg.Routes = append(cfg.Routes, r)
	}

	// §no_model_route: resolve once, after every route is built, so pointers
	// into cfg.Routes are stable (no further appends follow).
	if raw.NoModelRoute != "" {
		var matches []string
		for i := range cfg.Routes {
			matches = append(matches, cfg.Routes[i].match)
			if cfg.Routes[i].match == raw.NoModelRoute {
				cfg.NoModelRoute = &cfg.Routes[i]
				break
			}
		}
		if cfg.NoModelRoute == nil {
			return nil, fmt.Errorf("config %s: no_model_route %q matches no configured route; available route matches: %v", path, raw.NoModelRoute, matches)
		}
	} else {
		for i := range cfg.Routes {
			if cfg.Routes[i].authKind == "passthrough" {
				cfg.NoModelRoute = &cfg.Routes[i]
				break
			}
		}
		// No passthrough route at all: cfg.NoModelRoute stays nil, and a
		// model-less request falls through to matchRoute(routes, ""), which
		// is exactly today's behaviour (the "*" route, if configured).
	}

	return cfg, nil
}

// parseAuth validates an "auth" string's SYNTAX only -- it does no I/O and
// fetches no secret. It returns the auth kind and a logging-safe source
// description. Offline tools (`submux routes`, `submux check`) only need
// this: they must work against a config naming a keychain entry that does
// not exist on this machine, since config.example.json does exactly that.
// Actually resolving the credential is resolveCredentials's job, run only
// by `submux serve`.
func parseAuth(auth string) (kind, source string, err error) {
	switch {
	case auth == "passthrough":
		return "passthrough", "", nil
	case auth == "none":
		return "none", "", nil
	case strings.HasPrefix(auth, "bearer:env:"):
		v := strings.TrimPrefix(auth, "bearer:env:")
		if v == "" {
			return "", "", fmt.Errorf("auth %q: missing env var name", auth)
		}
		return "bearer", "env:" + v, nil
	case strings.HasPrefix(auth, "bearer:keychain:"):
		v := strings.TrimPrefix(auth, "bearer:keychain:")
		service, account, ok := strings.Cut(v, "/")
		if !ok || service == "" || account == "" {
			return "", "", fmt.Errorf("auth %q: expected bearer:keychain:<service>/<account>", auth)
		}
		return "bearer", "keychain:" + service + "/" + account, nil
	case strings.HasPrefix(auth, "x-api-key:env:"):
		v := strings.TrimPrefix(auth, "x-api-key:env:")
		if v == "" {
			return "", "", fmt.Errorf("auth %q: missing env var name", auth)
		}
		return "x-api-key", "env:" + v, nil
	default:
		return "", "", fmt.Errorf("unrecognized auth mode %q", auth)
	}
}

// resolveCredentials fetches every route's actual credential (env lookup or
// a single `security` keychain read per route) and populates authValue in
// place. Called ONCE, only by `submux serve` at process startup, so a
// missing secret is a startup error naming the service and account, never
// a silent 401 on the first proxied request.
func resolveCredentials(cfg *config) error {
	for i := range cfg.Routes {
		if err := resolveRouteCredential(&cfg.Routes[i]); err != nil {
			return fmt.Errorf("routes[%d] (match=%q): %w", i, cfg.Routes[i].match, err)
		}
	}
	return nil
}

// resolveRouteCredential populates r.authValue in place for a single route
// (a no-op for "passthrough"/"none"). Factored out of resolveCredentials so
// `submux check` and `submux models` (subs.go) can resolve just the ONE
// route a command needs, on demand, without paying for every other route's
// keychain lookup or env read.
func resolveRouteCredential(r *route) error {
	switch r.authKind {
	case "passthrough", "none":
		return nil
	case "bearer", "x-api-key":
		val, err := resolveCredentialValue(r.authSource)
		if err != nil {
			return fmt.Errorf("auth=%q: %w", r.auth, err)
		}
		r.authValue = val
		return nil
	default:
		return nil
	}
}

// resolveCredentialValue fetches the secret named by an authSource string
// produced by parseAuth, e.g. "env:VAR" or "keychain:service/account".
func resolveCredentialValue(source string) (string, error) {
	kind, v, ok := strings.Cut(source, ":")
	if !ok {
		return "", fmt.Errorf("malformed auth source %q", source)
	}
	switch kind {
	case "env":
		val, ok := os.LookupEnv(v)
		if !ok || val == "" {
			return "", fmt.Errorf("environment variable %s is not set", v)
		}
		return val, nil
	case "keychain":
		service, account, ok := strings.Cut(v, "/")
		if !ok || service == "" || account == "" {
			return "", fmt.Errorf("malformed keychain source %q", v)
		}
		val, err := keychainLookup(service, account)
		if err != nil {
			return "", fmt.Errorf("keychain entry for service=%q account=%q: %w", service, account, err)
		}
		return val, nil
	default:
		return "", fmt.Errorf("unrecognized auth source %q", source)
	}
}

// keychainLookup shells out to the macOS `security` CLI exactly once per
// route at startup; the result is cached by the caller (it lives in the
// resolved route for the lifetime of the process).
func keychainLookup(service, account string) (string, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("security find-generic-password failed (is the entry present in this user's login keychain?): %w", err)
	}
	val := strings.TrimRight(string(out), "\n")
	if val == "" {
		return "", fmt.Errorf("security find-generic-password returned an empty value")
	}
	return val, nil
}
