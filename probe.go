// probe.go answers "is this subscription still usable": a 1-token chat per
// configured subscription, sent through the same route the relay would use.
// A model list (/v1/models) keeps answering 200 after a plan lapses, so the
// list alone cannot tell an ended subscription from a live one.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// subscriptionProbe is one entry of config.json "subscription_probes".
type subscriptionProbe struct {
	Subscription string `json:"subscription"`
	Model        string `json:"model"`
	Fallback     string `json:"fallback,omitempty"` // subscription to point at when this one has ended
}

// probeResult is the outcome of one probe. State is "ok", "ended" (the
// provider refused the account: 401/402/403) or "unknown" (timeout, 429, 5xx,
// no route: never shown as ended, a hiccup must not lock a live plan).
type probeResult struct {
	Subscription string
	State        string
	Detail       string
}

const probeTimeout = 15 * time.Second

var probeRetryDelay = 2 * time.Second

// probeSubscription sends one 1-token Anthropic-format message for p.Model.
func probeSubscription(cfg *config, p subscriptionProbe) probeResult {
	res := probeResult{Subscription: p.Subscription, State: "unknown"}
	rt, ok := matchRoute(cfg.Routes, p.Model)
	if !ok {
		res.Detail = "no route for " + p.Model
		return res
	}
	if rt.authKind == "passthrough" {
		res.Detail = "passthrough route is not probed"
		return res
	}
	if err := resolveRouteCredential(&rt); err != nil {
		res.Detail = err.Error()
		return res
	}
	model := p.Model
	if rewritten, ok := rt.modelRewrite[model]; ok {
		model = rewritten
	}
	body := fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, model)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(rt.upstream, "/")+"/v1/messages", bytes.NewReader([]byte(body)))
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", "submux-probe") // kirocc rejects a request without one
	switch rt.authKind {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+rt.authValue)
	case "x-api-key":
		req.Header.Set("x-api-key", rt.authValue)
	}
	var out probeResult
	for attempt := range 2 {
		if attempt > 0 {
			time.Sleep(probeRetryDelay)
			req = req.Clone(req.Context())
			req.Body = io.NopCloser(bytes.NewReader([]byte(body)))
		}
		resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
		if err != nil {
			res.Detail = "probe failed: " + err.Error()
			return res
		}
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		out = classifyProbe(p.Subscription, resp.StatusCode, string(snippet))
		if out.State != "down" { // only a 5xx is retried: one blip must not hide a live model
			break
		}
	}
	return out
}

// classifyProbe maps an HTTP status and body to a probe state. cliproxy
// reports a provider's dead login as a 503 "auth_unavailable ... unauthorized",
// so a body that says so counts as ended even though the status is 5xx.
func classifyProbe(sub string, status int, body string) probeResult {
	res := probeResult{Subscription: sub}
	lower := strings.ToLower(body)
	switch {
	case status >= 200 && status < 300:
		res.State = "ok"
	case status == http.StatusPaymentRequired:
		res.State, res.Detail = "ended", "payment required or credits exhausted (HTTP 402)"
	case status == http.StatusUnauthorized || status == http.StatusForbidden ||
		strings.Contains(lower, "unauthorized") || strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "invalid_refresh_token"):
		res.State, res.Detail = "ended", fmt.Sprintf("login expired or revoked (HTTP %d)", status)
		if strings.Contains(lower, "subscription") {
			res.Detail = fmt.Sprintf("active subscription required (HTTP %d)", status)
		}
	case strings.Contains(lower, "monthly_request_count") || strings.Contains(lower, "quota") ||
		strings.Contains(lower, "reached the limit"):
		res.State, res.Detail = "ended", fmt.Sprintf("quota exhausted (HTTP %d)", status)
	case status >= 500:
		// kirocc turns Kiro's quota error into a bare 502 "upstream API error", so a
		// 5xx that survived probeSubscription's retry means the model cannot answer.
		res.State, res.Detail = "down", fmt.Sprintf("HTTP %d %s", status, firstLine(body))
	default:
		res.State = "unknown"
		res.Detail = fmt.Sprintf("HTTP %d %s", status, firstLine(body))
	}
	return res
}

// firstLine trims s to its first line, capped at 80 runes, for a status label.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}

// probeResultsMsg carries a finished probe batch back to the picker.
type probeResultsMsg struct{ results []probeResult }

// probeCmd runs the probes for names (all probes when names is empty),
// concurrently, off the UI goroutine.
func probeCmd(cfg *config, names ...string) tea.Cmd {
	var todo []subscriptionProbe
	for _, p := range cfg.SubscriptionProbes {
		if len(names) == 0 || containsString(names, p.Subscription) {
			todo = append(todo, p)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	return func() tea.Msg {
		out := make([]probeResult, len(todo))
		done := make(chan struct{}, len(todo))
		for i, p := range todo {
			go func(i int, p subscriptionProbe) {
				out[i] = probeSubscription(cfg, p)
				done <- struct{}{}
			}(i, p)
		}
		for range todo {
			<-done
		}
		return probeResultsMsg{results: out}
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// applyProbes marks every catalog row whose probe says "ended" as unusable,
// naming the free fallback when one is configured. A probe that is ok or
// unknown clears an earlier mark only through a rebuilt catalog, so callers
// pass the full result map each time.
func applyProbes(cfg *config, catalog []subscriptionOption, results map[string]probeResult) {
	fallbacks := map[string]string{}
	for _, p := range cfg.SubscriptionProbes {
		fallbacks[p.Subscription] = p.Fallback
	}
	for i := range catalog {
		r, ok := results[catalog[i].Name]
		if !ok || (r.State != "ended" && r.State != "down") {
			continue
		}
		msg := "subscription ended or inactive: " + r.Detail
		if r.State == "down" {
			msg = "not answering right now: " + r.Detail
		}
		if fb := fallbacks[catalog[i].Name]; fb != "" {
			msg += " · free fallback: " + fb
		}
		catalog[i].Unavailable = msg
	}
}

// fallbackFor returns the free fallback subscription configured for sub's
// probe ("" when none).
func fallbackFor(cfg *config, sub string) string {
	for _, p := range cfg.SubscriptionProbes {
		if p.Subscription == sub {
			return p.Fallback
		}
	}
	return ""
}

const (
	maxModelProbes     = 24 // each probe is a real 1-token request, and Kiro bills per request
	modelProbeParallel = 6
	modelProbeFreshFor = 10 * time.Minute
)

// modelProbeMsg carries one subscription's per-model probe batch back to the picker.
type modelProbeMsg struct {
	sub     string
	results map[string]probeResult // model id -> result
}

// probeModelsCmd probes up to maxModelProbes of ids in sub, a few at a time,
// off the UI goroutine. Models it does not reach stay selectable (unchecked).
func probeModelsCmd(cfg *config, sub string, ids []string) tea.Cmd {
	if len(ids) > maxModelProbes {
		ids = ids[:maxModelProbes]
	}
	if len(ids) == 0 {
		return nil
	}
	ids = append([]string(nil), ids...)
	return func() tea.Msg {
		results := make(map[string]probeResult, len(ids))
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, modelProbeParallel)
		for _, id := range ids {
			wg.Add(1)
			sem <- struct{}{}
			go func(id string) {
				defer wg.Done()
				defer func() { <-sem }()
				r := probeSubscription(cfg, subscriptionProbe{Subscription: sub, Model: id})
				mu.Lock()
				results[id] = r
				mu.Unlock()
			}(id)
		}
		wg.Wait()
		return modelProbeMsg{sub: sub, results: results}
	}
}

// modelBlocked reports whether a per-model probe says id cannot answer, and why.
func modelBlocked(r probeResult, ok bool) (string, bool) {
	if !ok || (r.State != "ended" && r.State != "down") {
		return "", false
	}
	if r.State == "down" {
		return "not answering: " + r.Detail, true
	}
	return r.Detail, true
}
