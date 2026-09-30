package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestClassifyProbe(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{200, "", "ok"},
		{403, "active OpenCode Go subscription required", "ended"},
		{401, "", "ended"},
		{402, "", "ended"},
		{503, `auth_unavailable: last upstream error: unauthorized: Invalid token`, "ended"},
		{429, "rate limited", "unknown"},
		{500, "boom", "unknown"},
		{502, "upstream API error", "unknown"},
	}
	for _, c := range cases {
		if got := classifyProbe("S", c.status, c.body).State; got != c.want {
			t.Errorf("classifyProbe(%d, %q) = %s, want %s", c.status, c.body, got, c.want)
		}
	}
}

func probeServer(t *testing.T, status int, body string) (*config, subscriptionProbe) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Claude-Code-Session-Id") == "" {
			t.Errorf("probe sent %s with session header %q", r.URL.Path, r.Header.Get("X-Claude-Code-Session-Id"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	cfg := &config{Routes: []route{{match: "*", upstream: srv.URL, authKind: "none"}}}
	return cfg, subscriptionProbe{Subscription: "ChatGPT", Model: "gpt-5.5", Fallback: "OpenCode Zen (free)"}
}

func TestProbeSubscriptionEndedAndOk(t *testing.T) {
	cfg, p := probeServer(t, 403, "active subscription required")
	if r := probeSubscription(cfg, p); r.State != "ended" || !strings.Contains(r.Detail, "403") {
		t.Fatalf("403 probe = %+v, want ended", r)
	}
	cfg, p = probeServer(t, 200, "{}")
	if r := probeSubscription(cfg, p); r.State != "ok" {
		t.Fatalf("200 probe = %+v, want ok", r)
	}
}

func TestProbeUnreachableIsUnknown(t *testing.T) {
	cfg := &config{Routes: []route{{match: "*", upstream: "http://127.0.0.1:1", authKind: "none"}}}
	if r := probeSubscription(cfg, subscriptionProbe{Subscription: "S", Model: "m"}); r.State != "unknown" {
		t.Fatalf("unreachable probe = %+v, want unknown", r)
	}
}

func TestApplyProbesMarksEndedWithFallback(t *testing.T) {
	cfg := &config{SubscriptionProbes: []subscriptionProbe{
		{Subscription: "ChatGPT", Model: "gpt-5.5", Fallback: "OpenCode Zen (free)"},
		{Subscription: "SuperGrok", Model: "grok-3-mini"},
	}}
	cat := []subscriptionOption{
		{Name: "ChatGPT", ModelIDs: []string{"gpt-5.5"}},
		{Name: "SuperGrok", ModelIDs: []string{"grok-3-mini"}},
	}
	applyProbes(cfg, cat, map[string]probeResult{
		"ChatGPT":   {Subscription: "ChatGPT", State: "ended", Detail: "login expired or revoked (HTTP 503)"},
		"SuperGrok": {Subscription: "SuperGrok", State: "unknown", Detail: "HTTP 502"},
	})
	if !strings.Contains(cat[0].Unavailable, "ended") || !strings.Contains(cat[0].Unavailable, "free fallback: OpenCode Zen (free)") {
		t.Errorf("ended row Unavailable = %q, want ended plus the free fallback", cat[0].Unavailable)
	}
	if cat[1].Unavailable != "" {
		t.Errorf("unknown probe marked row unavailable: %q", cat[1].Unavailable)
	}
}

func TestPickerAppliesProbeResults(t *testing.T) {
	cfg := &config{SubscriptionProbes: []subscriptionProbe{{Subscription: "ChatGPT", Model: "gpt-5.5"}}}
	m := pickModel{cfg: cfg, catalog: []subscriptionOption{{Name: "ChatGPT", ModelIDs: []string{"gpt-5.5"}}}}
	// catalog rebuild inside the handler needs a live upstream; give it none
	// and check the stored result instead.
	next, _ := m.Update(probeResultsMsg{results: []probeResult{{Subscription: "ChatGPT", State: "ended", Detail: "x"}}})
	if got := next.(pickModel).probes["ChatGPT"].State; got != "ended" {
		t.Fatalf("stored probe state = %q, want ended", got)
	}
}

func TestLoadConfigRejectsEmptyProbe(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	bad := `{"routes":[{"match":"*","upstream":"http://127.0.0.1:1","auth":"none"}],"subscription_probes":[{"subscription":"S"}]}`
	if err := writeFileForTest(path, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted a probe with no model")
	}
}

func writeFileForTest(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }
