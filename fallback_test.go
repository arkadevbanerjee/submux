package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestFallbackOnRateLimit (§9.5): a 429 on the first hop falls to the next
// chain entry; the client sees the answering model's response, and
// x-submux-fallback announces the substitution.
func TestFallbackOnRateLimit(t *testing.T) {
	var calls int32
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	t.Cleanup(rateLimited.Close)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","model":"claude-fable-5-1","content":[]}`))
	}))
	t.Cleanup(healthy.Close)

	routes := []route{
		{match: "claude-fable-*", upstream: rateLimited.URL, upstreamURL: mustParseURL(t, rateLimited.URL), authKind: "none", fallback: &[]string{"glm-5.3"}},
		{match: "glm-*", upstream: healthy.URL, upstreamURL: mustParseURL(t, healthy.URL), authKind: "none"},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes, FallbackStatusCodes: []int{429}, CooldownDefault: time.Minute}
	s := newServer(cfg, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if got["model"] != "glm-5.3" {
		t.Errorf("response model = %v, want %q (the model that actually answered)", got["model"], "glm-5.3")
	}
	if hdr := rec.Header().Get("x-submux-fallback"); hdr != "claude-fable-5-1 -> glm-5.3" {
		t.Errorf("x-submux-fallback = %q, want %q", hdr, "claude-fable-5-1 -> glm-5.3")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("rate-limited upstream calls = %d, want 1", calls)
	}
}

// TestFallbackEmptyListMeansNever (§9.5, §9.1): an explicit "fallback": []
// on the route must NOT inherit default_fallback -- the pointer distinction
// this whole design depends on.
func TestFallbackEmptyListMeansNever(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	t.Cleanup(upstream.Close)

	empty := []string{}
	routes := []route{
		{match: "claude-*", upstream: upstream.URL, upstreamURL: mustParseURL(t, upstream.URL), authKind: "none", fallback: &empty},
	}
	cfg := &config{
		Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes,
		DefaultFallback: []string{"glm-5.3"}, FallbackStatusCodes: []int{429}, CooldownDefault: time.Minute,
	}
	s := newServer(cfg, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (explicit empty fallback fails loud, never inherits default_fallback)", rec.Code)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("upstream calls = %d, want exactly 1", calls)
	}
}

// TestDefaultFallbackApplies (§9.5, §9.1): a route with no "fallback" key at
// all (nil pointer) inherits default_fallback.
func TestDefaultFallbackApplies(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	t.Cleanup(failing.Close)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","model":"glm-5.3"}`))
	}))
	t.Cleanup(healthy.Close)

	routes := []route{
		{match: "claude-*", upstream: failing.URL, upstreamURL: mustParseURL(t, failing.URL), authKind: "none"}, // no fallback key: nil pointer
		{match: "glm-*", upstream: healthy.URL, upstreamURL: mustParseURL(t, healthy.URL), authKind: "none"},
	}
	cfg := &config{
		Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes,
		DefaultFallback: []string{"glm-5.3"}, FallbackStatusCodes: []int{429}, CooldownDefault: time.Minute,
	}
	s := newServer(cfg, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (default_fallback should have applied)", rec.Code)
	}
}

// TestNoFallbackAfterFirstByte (§9.5, the corruption guard): once one
// response byte has reached the client, a later upstream failure must NOT
// trigger a retry -- that would splice two models' output into one answer.
func TestNoFallbackAfterFirstByte(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: chunk1\n\n"))
		flusher.Flush()
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close() // abrupt disconnect mid-stream, AFTER the first byte
			}
		}
	}))
	t.Cleanup(upstream.Close)

	routes := []route{
		{match: "claude-*", upstream: upstream.URL, upstreamURL: mustParseURL(t, upstream.URL), authKind: "none", fallback: &[]string{"glm-5.3"}},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes, FallbackStatusCodes: []int{429, 500, 502, 503}, CooldownDefault: time.Minute}
	s := newServer(cfg, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "chunk1") {
		t.Errorf("client body = %q, want it to contain the already-delivered chunk1", rec.Body.String())
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("upstream calls = %d, want exactly 1 (no retry once a byte has reached the client)", calls)
	}
}

// TestCooldownSkipsUpstream (§9.5): once a model id is cooling, a later
// request for that id must not call its upstream at all.
func TestCooldownSkipsUpstream(t *testing.T) {
	var coolingCalls int32
	cooling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&coolingCalls, 1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	t.Cleanup(cooling.Close)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","model":"glm-5.3"}`))
	}))
	t.Cleanup(healthy.Close)

	routes := []route{
		{match: "claude-*", upstream: cooling.URL, upstreamURL: mustParseURL(t, cooling.URL), authKind: "none", fallback: &[]string{"glm-5.3"}},
		{match: "glm-*", upstream: healthy.URL, upstreamURL: mustParseURL(t, healthy.URL), authKind: "none"},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes, FallbackStatusCodes: []int{429}, CooldownDefault: time.Minute}
	s := newServer(cfg, false)
	body := []byte(`{"model":"claude-fable-5-1"}`)

	rec1 := httptest.NewRecorder()
	s.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
	if got := atomic.LoadInt32(&coolingCalls); got != 1 {
		t.Fatalf("first request: cooling upstream calls = %d, want 1", got)
	}

	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200 via fallback", rec2.Code)
	}
	if got := atomic.LoadInt32(&coolingCalls); got != 1 {
		t.Errorf("cooling upstream calls after second request = %d, want still 1 (cooldown must skip it entirely)", got)
	}
}

// TestChainExhausted (§9.5, §9.4): when every hop fails, the LAST upstream
// error reaches the client verbatim, never a synthesized one.
func TestChainExhausted(t *testing.T) {
	fail := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"down"}}`))
		}))
	}
	s1, s2 := fail(), fail()
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)

	routes := []route{
		{match: "claude-*", upstream: s1.URL, upstreamURL: mustParseURL(t, s1.URL), authKind: "none", fallback: &[]string{"glm-5.3"}},
		{match: "glm-*", upstream: s2.URL, upstreamURL: mustParseURL(t, s2.URL), authKind: "none"},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes, FallbackStatusCodes: []int{503}, CooldownDefault: time.Minute}
	s := newServer(cfg, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (the last upstream's real error, passed through)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "overloaded_error") {
		t.Errorf("body = %q, want the last upstream's real error body verbatim", rec.Body.String())
	}
	if hdr := rec.Header().Get("x-submux-fallback"); hdr == "" {
		t.Errorf("x-submux-fallback header missing on chain exhaustion")
	}
}

// TestNoFallbackLoop (§9.5, §9.2.2): a chain that names the originally
// requested id must not retry it.
func TestNoFallbackLoop(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	t.Cleanup(upstream.Close)

	routes := []route{
		{match: "claude-*", upstream: upstream.URL, upstreamURL: mustParseURL(t, upstream.URL), authKind: "none",
			fallback: &[]string{"claude-fable-5-1"}}, // the chain names the requested id itself
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes, FallbackStatusCodes: []int{429}, CooldownDefault: time.Minute}
	s := newServer(cfg, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (chain exhausted, no self-retry loop)", rec.Code)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("upstream calls = %d, want exactly 1 (the self-referencing chain entry must never be retried)", calls)
	}
}

// TestStreamingFallbackModelRewrite (§9.5, §9.3): the message_start event's
// message.model is rewritten to the model that actually answered, and the
// rest of the stream is untouched.
func TestStreamingFallbackModelRewrite(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	t.Cleanup(failing.Close)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"internal-glm-name\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(healthy.Close)

	routes := []route{
		{match: "claude-*", upstream: failing.URL, upstreamURL: mustParseURL(t, failing.URL), authKind: "none", fallback: &[]string{"glm-5.3"}},
		{match: "glm-*", upstream: healthy.URL, upstreamURL: mustParseURL(t, healthy.URL), authKind: "none"},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes, FallbackStatusCodes: []int{429}, CooldownDefault: time.Minute}
	s := newServer(cfg, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1","stream":true}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"model":"glm-5.3"`) {
		t.Errorf("streamed message_start does not carry the answering model %q: %s", "glm-5.3", body)
	}
	if strings.Contains(body, "internal-glm-name") {
		t.Errorf("streamed message_start still names the upstream's own claimed model instead of the routed one: %s", body)
	}
	if !strings.Contains(body, "content_block_delta") {
		t.Errorf("rest of the stream missing or corrupted: %s", body)
	}
}
