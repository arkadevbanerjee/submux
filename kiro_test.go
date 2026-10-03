package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

var kiroTestLive = []string{
	"kiro/auto", "kiro/claude-auto", "kiro/gpt-5.6-sol",
	"kiro/claude-haiku-4.5",
	"kiro/claude-opus-4.5", "kiro/claude-opus-4.8", "kiro/claude-opus-4-8[1m]",
	"kiro/claude-opus-5", "kiro/claude-opus-5[1m]",
	"kiro/claude-opus-5.5", "kiro/claude-opus-5-5[1m]",
	"kiro/claude-sonnet-4", "kiro/claude-sonnet-4-6", "kiro/claude-sonnet-4.6", "kiro/claude-sonnet-4-6[1m]",
	"kiro/claude-sonnet-5", "kiro/claude-sonnet-5[1m]", "kiro/claude-sonnet-5.5",
	"kiro/kiro/claude-opus-5-5[1m]",
}

func TestKiroResolve(t *testing.T) {
	tests := []struct {
		name, req, want string
		ok              bool
	}{
		{"dash to dot", "claude-opus-5-5", "kiro/claude-opus-5.5", true},
		{"dash to dot 4.8", "claude-opus-4-8", "kiro/claude-opus-4.8", true},
		{"1m kept", "claude-opus-4-8[1m]", "kiro/claude-opus-4-8[1m]", true},
		{"1m kept 5.5", "claude-opus-5-5[1m]", "kiro/claude-opus-5-5[1m]", true},
		{"1m missing falls to plain", "claude-opus-4-5[1m]", "kiro/claude-opus-4.5", true},
		{"major only", "claude-sonnet-5", "kiro/claude-sonnet-5", true},
		{"major only 1m", "claude-sonnet-5[1m]", "kiro/claude-sonnet-5[1m]", true},
		{"dated", "claude-haiku-4-5-20251001", "kiro/claude-haiku-4.5", true},
		{"dot sonnet", "claude-sonnet-5.5", "kiro/claude-sonnet-5.5", true},
		{"exact live dash id wins", "claude-sonnet-4-6", "kiro/claude-sonnet-4-6", true},
		{"dated prefers dot spelling", "claude-sonnet-4-6-20260101", "kiro/claude-sonnet-4.6", true},
		{"fable maps to newest opus", "claude-fable-5-1", "kiro/claude-opus-5.5", true},
		{"fable 1m maps to newest opus 1m", "claude-fable-5-1[1m]", "kiro/claude-opus-5-5[1m]", true},
		{"unknown opus version -> newest opus", "claude-opus-9-9", "kiro/claude-opus-5.5", true},
		{"unknown sonnet version -> newest sonnet", "claude-sonnet-9", "kiro/claude-sonnet-5.5", true},
		{"already kiro", "kiro/claude-opus-5.5", "kiro/claude-opus-5.5", true},
		{"already kiro dash", "kiro/claude-opus-5-5", "kiro/claude-opus-5.5", true},
		{"already kiro 1m", "kiro/claude-opus-5-5[1m]", "kiro/claude-opus-5-5[1m]", true},
		{"non-claude passthrough", "gpt-5.6-sol", "kiro/gpt-5.6-sol", true},
		{"auto", "auto", "kiro/auto", true},
		{"non-claude missing", "gpt-9", "", false},
		{"family absent", "claude-fable-5-1x", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, closest, ok := kiroResolve(tc.req, kiroTestLive)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("kiroResolve(%q) = %q, %v; want %q, %v", tc.req, got, ok, tc.want, tc.ok)
			}
			if !ok && len(closest) == 0 {
				t.Fatalf("no closest ids for failed %q", tc.req)
			}
		})
	}
}

func TestKiroResolveHaikuMissing(t *testing.T) {
	live := []string{"kiro/claude-opus-5.5", "kiro/claude-sonnet-5"}
	got, closest, ok := kiroResolve("claude-haiku-4-5", live)
	if ok || got != "" {
		t.Fatalf("got %q ok=%v, want failure", got, ok)
	}
	if !reflect.DeepEqual(closest, []string{"kiro/claude-opus-5.5", "kiro/claude-sonnet-5"}) {
		t.Fatalf("closest = %v", closest)
	}
}

// TestKiroModeNeverReachesAnthropic drives the handler: the /kiro prefix is
// stripped, claude-* ids are mapped to kiro/*, and a miss is a 400 that
// leaves both upstreams untouched.
func TestKiroModeNeverReachesAnthropic(t *testing.T) {
	anth, anthSrv := newCaptureServer(t)
	kiro, kiroSrv := newCaptureServer(t)
	cfg := &config{MaxBodyBytes: 1 << 20, Routes: []route{
		{match: "claude-*", upstream: anthSrv.URL, upstreamURL: mustParseURL(t, anthSrv.URL), auth: "passthrough", authKind: "passthrough"},
		{match: "*", upstream: kiroSrv.URL, upstreamURL: mustParseURL(t, kiroSrv.URL), auth: "none", authKind: "none"},
	}}
	s := newServer(cfg, false)
	s.kiroLiveFn = func() []string { return kiroTestLive }
	post := func(path, model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"`+model+`"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("/kiro/v1/messages", "claude-opus-4-8"); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if _, body := kiro.snapshot(t); !strings.Contains(string(body), `"kiro/claude-opus-4.8"`) {
		t.Fatalf("kiro upstream body = %s", body)
	}
	if anth.receivedRequest() {
		t.Fatal("anthropic upstream was hit in kiro mode")
	}

	rec := post("/kiro/v1/messages", "gpt-9")
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 400 || !strings.Contains(string(body), "invalid_request_error") || !strings.Contains(string(body), "gpt-9") {
		t.Fatalf("want 400 invalid_request_error naming the id, got %d %s", rec.Code, body)
	}
	if anth.receivedRequest() {
		t.Fatal("anthropic upstream was hit on a kiro-mode miss")
	}

	// No marker: unchanged behaviour, claude-* still goes to Anthropic.
	post("/v1/messages", "claude-opus-4-8")
	if !anth.receivedRequest() {
		t.Fatal("unmarked request no longer reaches the claude-* route")
	}
}
