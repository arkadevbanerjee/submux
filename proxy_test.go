package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captured is a thread-safe record of what the upstream test server
// actually received, so assertions can run after the round trip completes.
type captured struct {
	mu     sync.Mutex
	header http.Header
	body   []byte
	got    bool
}

func (c *captured) set(h http.Header, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.header = h.Clone()
	c.body = append([]byte(nil), body...)
	c.got = true
}

func (c *captured) snapshot(t *testing.T) (http.Header, []byte) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.got {
		t.Fatal("upstream never received a request")
	}
	return c.header, c.body
}

// receivedRequest reports whether this upstream ever received a request,
// without failing the test -- used where the assertion is "this upstream
// must NOT have been hit" as well as "must have been hit".
func (c *captured) receivedRequest() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got
}

func newCaptureServer(t *testing.T) (*captured, *httptest.Server) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.set(r.Header, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}

func headerValuesContain(h http.Header, marker string) []string {
	var hits []string
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, marker) {
				hits = append(hits, fmt.Sprintf("%s=%s", k, v))
			}
		}
	}
	return hits
}

// TestMaxTokenNeverLeaves is the red control (§5.3): a subscription bearer
// must NEVER reach a non-passthrough upstream, and must arrive verbatim at
// a passthrough one. A test that only checked one direction would not
// catch a policy that always strips (fails passthrough silently) or always
// forwards (fails the security guarantee silently).
func TestMaxTokenNeverLeaves(t *testing.T) {
	const fakeBearer = "Bearer sk-ant-oat01-FAKE"
	const configuredCred = "configured-aggregator-token"

	nonPassCapture, nonPassSrv := newCaptureServer(t)
	passCapture, passSrv := newCaptureServer(t)

	routes := []route{
		{
			match:       "glm-*",
			upstream:    nonPassSrv.URL,
			upstreamURL: mustParseURL(t, nonPassSrv.URL),
			authKind:    "bearer",
			authSource:  "env:SUBMUX_TEST_TOKEN",
			authValue:   configuredCred,
		},
		{
			match:       "claude-*",
			upstream:    passSrv.URL,
			upstreamURL: mustParseURL(t, passSrv.URL),
			authKind:    "passthrough",
		},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes}
	s := newServer(cfg, false)

	// Direction 1: non-passthrough route. The fake subscription bearer must
	// not reach the upstream anywhere, and Authorization must be the
	// configured credential instead.
	body := []byte(`{"model":"glm-5.3-flash"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", fakeBearer)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	gotHeader, _ := nonPassCapture.snapshot(t)
	if hits := headerValuesContain(gotHeader, "sk-ant-"); len(hits) > 0 {
		t.Errorf("non-passthrough upstream received a subscription token marker: %v", hits)
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer "+configuredCred {
		t.Errorf("non-passthrough upstream Authorization = %q, want %q", got, "Bearer "+configuredCred)
	}

	// Direction 2: passthrough route. The same fake bearer must arrive
	// verbatim.
	body2 := []byte(`{"model":"claude-fable-5-1"}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body2))
	req2.Header.Set("Authorization", fakeBearer)
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, req2)

	gotHeader2, _ := passCapture.snapshot(t)
	if got := gotHeader2.Get("Authorization"); got != fakeBearer {
		t.Errorf("passthrough upstream Authorization = %q, want it to arrive verbatim as %q", got, fakeBearer)
	}
}

// TestBodyBytesUnchanged: a passthrough request's upstream body must be
// byte-identical to the inbound body, including key order and whitespace.
func TestBodyBytesUnchanged(t *testing.T) {
	capture, srv := newCaptureServer(t)
	routes := []route{
		{match: "claude-*", upstream: srv.URL, upstreamURL: mustParseURL(t, srv.URL), authKind: "passthrough"},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes}
	s := newServer(cfg, false)

	// Deliberately odd whitespace and key order: re-marshaling would change
	// this, so a byte-for-byte match proves the original bytes were
	// forwarded untouched.
	body := []byte("{\"z\": 1,   \"model\":\"claude-fable-5-1\", \"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	_, gotBody := capture.snapshot(t)
	if !bytes.Equal(gotBody, body) {
		t.Errorf("upstream body = %q, want byte-identical to %q", gotBody, body)
	}
}

// TestOAuthBetaStripped: oauth-* entries in anthropic-beta are meaningless
// (and sometimes rejected) by a non-Anthropic upstream, so they are
// stripped there, but must arrive intact at a passthrough upstream.
func TestOAuthBetaStripped(t *testing.T) {
	const betaHeader = "oauth-2025-04-20,interleaved-thinking-2025-05-14"

	nonPassCapture, nonPassSrv := newCaptureServer(t)
	passCapture, passSrv := newCaptureServer(t)

	routes := []route{
		{match: "glm-*", upstream: nonPassSrv.URL, upstreamURL: mustParseURL(t, nonPassSrv.URL), authKind: "none"},
		{match: "claude-*", upstream: passSrv.URL, upstreamURL: mustParseURL(t, passSrv.URL), authKind: "passthrough"},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes}
	s := newServer(cfg, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"glm-5.3-flash"}`)))
	req.Header.Set("anthropic-beta", betaHeader)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	gotHeader, _ := nonPassCapture.snapshot(t)
	if got := gotHeader.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("non-passthrough anthropic-beta = %q, want %q", got, "interleaved-thinking-2025-05-14")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	req2.Header.Set("anthropic-beta", betaHeader)
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, req2)
	gotHeader2, _ := passCapture.snapshot(t)
	if got := gotHeader2.Get("anthropic-beta"); got != betaHeader {
		t.Errorf("passthrough anthropic-beta = %q, want unchanged %q", got, betaHeader)
	}
}

// TestStreamingFlushes proves FlushInterval: -1 is wired correctly: the
// first SSE chunk must be readable by the client before the second chunk
// is even written by the upstream. A buffering proxy would make the first
// read block until the whole response (i.e. until release fires), which
// this test's timeout would catch.
func TestStreamingFlushes(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: chunk1\n\n"))
		flusher.Flush()
		<-release
		_, _ = w.Write([]byte("data: chunk2\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	routes := []route{
		{match: "claude-*", upstream: upstream.URL, upstreamURL: mustParseURL(t, upstream.URL), authKind: "passthrough"},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes}
	s := newServer(cfg, false)
	submux := httptest.NewServer(s)
	t.Cleanup(submux.Close)

	resp, err := http.Post(submux.URL+"/v1/messages", "application/json", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	firstLine := make(chan string, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		line, _ := br.ReadString('\n')
		firstLine <- line
	}()

	select {
	case line := <-firstLine:
		if !strings.Contains(line, "chunk1") {
			t.Fatalf("first line = %q, want it to contain chunk1", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first chunk: the proxy is buffering instead of flushing")
	}

	close(release)
}

// mustLoadConfig writes configJSON to a temp file and loads it through the
// real loadConfig path, so these tests exercise the actual no_model_route
// resolution and validation logic, not a hand-assembled config struct.
func mustLoadConfig(t *testing.T, configJSON string) *config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: unexpected error: %v", err)
	}
	return cfg
}

// TestNoModelRoutesToPassthroughByDefault: with no "no_model_route" key, a
// model-less body must reach the first passthrough route in order, even
// though the "*" route (which would otherwise catch it) is NOT passthrough.
func TestNoModelRoutesToPassthroughByDefault(t *testing.T) {
	passCapture, passSrv := newCaptureServer(t)
	starCapture, starSrv := newCaptureServer(t)

	cfg := mustLoadConfig(t, fmt.Sprintf(`{
		"listen": "127.0.0.1:0",
		"routes": [
			{"match": "claude-*", "upstream": %q, "auth": "passthrough"},
			{"match": "*", "upstream": %q, "auth": "none"}
		]
	}`, passSrv.URL, starSrv.URL))

	s := newServer(cfg, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	passCapture.snapshot(t)
	if starCapture.receivedRequest() {
		t.Errorf("the non-passthrough \"*\" route received the no-model request; it should have gone to the default passthrough route instead")
	}
}

// TestNoModelRouteExplicit: an explicit "no_model_route" overrides the
// passthrough default.
func TestNoModelRouteExplicit(t *testing.T) {
	passCapture, passSrv := newCaptureServer(t)
	starCapture, starSrv := newCaptureServer(t)

	cfg := mustLoadConfig(t, fmt.Sprintf(`{
		"listen": "127.0.0.1:0",
		"no_model_route": "*",
		"routes": [
			{"match": "claude-*", "upstream": %q, "auth": "passthrough"},
			{"match": "*", "upstream": %q, "auth": "none"}
		]
	}`, passSrv.URL, starSrv.URL))

	s := newServer(cfg, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	starCapture.snapshot(t)
	if passCapture.receivedRequest() {
		t.Errorf("the passthrough route received the no-model request; the explicit no_model_route=\"*\" should have won")
	}
}

// TestNoModelRouteUnknownValueIsStartupError: a "no_model_route" value that
// matches no configured route's "match" fails config load, naming the bad
// value and listing the available route matches.
func TestNoModelRouteUnknownValueIsStartupError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	badConfig := `{
		"listen": "127.0.0.1:0",
		"no_model_route": "does-not-exist-*",
		"routes": [
			{"match": "claude-*", "upstream": "http://127.0.0.1:1", "auth": "passthrough"},
			{"match": "*", "upstream": "http://127.0.0.1:2", "auth": "none"}
		]
	}`
	if err := os.WriteFile(path, []byte(badConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig: expected an error for an unknown no_model_route value, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist-*") {
		t.Errorf("loadConfig error = %q, want it to name the bad value %q", err.Error(), "does-not-exist-*")
	}
	if !strings.Contains(err.Error(), "available route matches") || !strings.Contains(err.Error(), "claude-*") {
		t.Errorf("loadConfig error = %q, want it to list the available route matches", err.Error())
	}
}

// TestNoModelNoPassthroughRouteFallsBack: with no "no_model_route" key AND
// no passthrough route at all, a model-less request keeps today's
// behaviour and lands on the "*" fallback route.
func TestNoModelNoPassthroughRouteFallsBack(t *testing.T) {
	claudeCapture, claudeSrv := newCaptureServer(t)
	starCapture, starSrv := newCaptureServer(t)

	cfg := mustLoadConfig(t, fmt.Sprintf(`{
		"listen": "127.0.0.1:0",
		"routes": [
			{"match": "claude-*", "upstream": %q, "auth": "none"},
			{"match": "*", "upstream": %q, "auth": "none"}
		]
	}`, claudeSrv.URL, starSrv.URL))

	s := newServer(cfg, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	starCapture.snapshot(t)
	if claudeCapture.receivedRequest() {
		t.Errorf("a non-passthrough, non-\"*\" route received the no-model request; with zero passthrough routes it should have fallen back to \"*\"")
	}
}

// TestModelBearingRequestUnaffected: a normal request that DOES carry a
// model field must route exactly as before, regardless of any
// no_model_route override in effect.
func TestModelBearingRequestUnaffected(t *testing.T) {
	passCapture, passSrv := newCaptureServer(t)
	starCapture, starSrv := newCaptureServer(t)

	cfg := mustLoadConfig(t, fmt.Sprintf(`{
		"listen": "127.0.0.1:0",
		"no_model_route": "*",
		"routes": [
			{"match": "claude-*", "upstream": %q, "auth": "passthrough"},
			{"match": "*", "upstream": %q, "auth": "none"}
		]
	}`, passSrv.URL, starSrv.URL))

	s := newServer(cfg, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1"}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	passCapture.snapshot(t)
	if starCapture.receivedRequest() {
		t.Errorf("a model-bearing request was diverted to the no_model_route override; it must route on its own model id, unaffected")
	}
}

// TestLogLinePathPresent: a 404 (or any status) is otherwise untraceable to
// the endpoint that produced it, since the log line named everything except
// the request path. The per-request log line must carry the path, including
// the query string, so an operator can grep straight to the offending
// endpoint.
func TestLogLinePathPresent(t *testing.T) {
	_, srv := newCaptureServer(t)
	routes := []route{
		{match: "claude-*", upstream: srv.URL, upstreamURL: mustParseURL(t, srv.URL), authKind: "passthrough"},
	}
	cfg := &config{Listen: "127.0.0.1:0", MaxBodyBytes: defaultMaxBodyBytes, Routes: routes}
	s := newServer(cfg, false)

	var logBuf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(orig)

	body := []byte(`{"model":"claude-fable-5-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	line := logBuf.String()
	if !strings.Contains(line, `path="/v1/messages?beta=true"`) {
		t.Errorf("log line = %q, want it to contain path=\"/v1/messages?beta=true\"", line)
	}
}
