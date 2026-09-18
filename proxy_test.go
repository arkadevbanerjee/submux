package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
