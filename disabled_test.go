package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// disabledServer answers every request with status and body, counting hits.
func disabledServer(t *testing.T, status int, body string) (*atomic.Int32, *httptest.Server) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &hits, srv
}

const orgDisabledBody = `{"type":"error","error":{"type":"permission_error","message":"Your organization has disabled Claude subscription access for Claude Code"}}`

func disabledTestServer(t *testing.T, anthURL, kiroURL string) *server {
	t.Helper()
	cfg := &config{MaxBodyBytes: 1 << 20, FallbackStatusCodes: []int{429, 403}, CooldownDefault: time.Minute, Routes: []route{
		{match: "claude-*", upstream: anthURL, upstreamURL: mustParseURL(t, anthURL), auth: "passthrough", authKind: "passthrough", subscription: "Claude Max", fallback: &[]string{}},
		{match: "kiro/*", upstream: kiroURL, upstreamURL: mustParseURL(t, kiroURL), auth: "none", authKind: "none", fallback: &[]string{}},
		{match: "*", upstream: kiroURL, upstreamURL: mustParseURL(t, kiroURL), auth: "none", authKind: "none"},
	}}
	s := newServer(cfg, false)
	s.kiroLiveFn = func() []string { return kiroTestLive }
	return s
}

func postModel(s *server, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+model+`"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// A disabled subscription fails over to the same model on Kiro, and the next
// request skips the disabled upstream entirely.
func TestDisabledSubscriptionFailsOverToSameModel(t *testing.T) {
	anthHits, anth := disabledServer(t, 403, orgDisabledBody)
	kiro, kiroSrv := newCaptureServer(t)
	s := disabledTestServer(t, anth.URL, kiroSrv.URL)

	rec := postModel(s, "claude-opus-4-8")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if _, body := kiro.snapshot(t); !strings.Contains(string(body), `"kiro/claude-opus-4.8"`) {
		t.Fatalf("kiro body = %s", body)
	}
	if n := anthHits.Load(); n != 1 {
		t.Fatalf("anthropic hits = %d, want 1", n)
	}
	if rec := postModel(s, "claude-opus-4-8"); rec.Code != 200 {
		t.Fatalf("second status %d", rec.Code)
	}
	if n := anthHits.Load(); n != 1 {
		t.Fatalf("disabled upstream hit again: %d", n)
	}
}

// A 403 without the disabled wording passes through with its body intact.
func TestPlain403PassesThroughUnchanged(t *testing.T) {
	body := `{"type":"error","error":{"type":"permission_error","message":"not allowed for this model"}}`
	_, anth := disabledServer(t, 403, body)
	kiro, kiroSrv := newCaptureServer(t)
	s := disabledTestServer(t, anth.URL, kiroSrv.URL)

	rec := postModel(s, "claude-opus-4-8")
	if rec.Code != 403 || rec.Body.String() != body {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	if kiro.receivedRequest() {
		t.Fatal("plain 403 failed over")
	}
}

// A 401 on a passthrough route is one client's token and never disables it.
func TestPassthrough401NeverDisables(t *testing.T) {
	anthHits, anth := disabledServer(t, 401, `{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired; subscription access disabled"}}`)
	kiro, kiroSrv := newCaptureServer(t)
	s := disabledTestServer(t, anth.URL, kiroSrv.URL)

	if rec := postModel(s, "claude-opus-4-8"); rec.Code != 401 {
		t.Fatalf("got %d", rec.Code)
	}
	postModel(s, "claude-opus-4-8")
	if anthHits.Load() != 2 || kiro.receivedRequest() {
		t.Fatalf("401 disabled the route: hits=%d kiro=%v", anthHits.Load(), kiro.receivedRequest())
	}
}

// After disabledFor the next request goes to the original upstream again.
func TestDisabledReEnablesAfterTTL(t *testing.T) {
	anth, anthSrv := newCaptureServer(t)
	_, kiroSrv := newCaptureServer(t)
	s := disabledTestServer(t, anthSrv.URL, kiroSrv.URL)
	s.disabled.markCooling("Claude Max", time.Now().Add(-time.Second))

	if rec := postModel(s, "claude-opus-4-8"); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !anth.receivedRequest() {
		t.Fatal("expired disable still skipped the upstream")
	}
}

// No same-model alternative: the client gets the real upstream error.
func TestDisabledWithNoAlternativeReturnsUpstreamError(t *testing.T) {
	_, anth := disabledServer(t, 403, orgDisabledBody)
	_, kiroSrv := newCaptureServer(t)
	s := disabledTestServer(t, anth.URL, kiroSrv.URL)
	s.kiroLiveFn = func() []string { return nil }

	rec := postModel(s, "claude-opus-4-8")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "disabled Claude subscription") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

// A kiro/* spelling the upstream does not list maps onto the live list.
func TestUnknownKiroSpellingIsResolved(t *testing.T) {
	_, anth := disabledServer(t, 500, `{}`)
	kiro, kiroSrv := newCaptureServer(t)
	s := disabledTestServer(t, anth.URL, kiroSrv.URL)
	s.kiroLiveFn = func() []string { return []string{"kiro/claude-sonnet-5.5", "kiro/claude-sonnet-5-5[1m]"} }

	if rec := postModel(s, "kiro/claude-sonnet-5-5"); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if _, body := kiro.snapshot(t); !strings.Contains(string(body), `"kiro/claude-sonnet-5.5"`) {
		t.Fatalf("kiro body = %s", body)
	}
	// A listed id is sent unchanged.
	postModel(s, "kiro/claude-sonnet-5-5[1m]")
	if _, body := kiro.snapshot(t); !strings.Contains(string(body), `"kiro/claude-sonnet-5-5[1m]"`) {
		t.Fatalf("listed id rewritten: %s", body)
	}
}

func TestDisabledBodyRe(t *testing.T) {
	for _, c := range []struct {
		body string
		want bool
	}{
		{"Your organization has disabled Claude subscription access for Claude Code", true},
		{"Your subscription has expired", true},
		{"plan ended on 2026-10-01", true},
		{"invalid x-api-key", false},
		{"OAuth token has expired", false},
		{"rate limited", false},
	} {
		if got := disabledBodyRe.MatchString(c.body); got != c.want {
			t.Errorf("%q: got %v want %v", c.body, got, c.want)
		}
	}
}
