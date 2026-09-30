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
	resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
	if err != nil {
		res.Detail = "probe failed: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return classifyProbe(p.Subscription, resp.StatusCode, string(snippet))
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
		strings.Contains(lower, "unauthorized") || strings.Contains(lower, "invalid token"):
		res.State, res.Detail = "ended", fmt.Sprintf("login expired or revoked (HTTP %d)", status)
		if strings.Contains(lower, "subscription") {
			res.Detail = fmt.Sprintf("active subscription required (HTTP %d)", status)
		}
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
		if !ok || r.State != "ended" {
			continue
		}
		msg := "subscription ended or inactive: " + r.Detail
		if fb := fallbacks[catalog[i].Name]; fb != "" {
			msg += " · free fallback: " + fb
		}
		catalog[i].Unavailable = msg
	}
}
