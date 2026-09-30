package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServedStoreRecordsAndRenders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "served.json")
	st := newServedStore(path)
	now := time.Now()
	st.record("s1", "gpt-6.1-sol", servedEntry{Served: "gpt-6.1-sol", Status: 200, LatencyMS: 4100, At: now})
	st.record("s1", "claude-haiku", servedEntry{Served: "claude-haiku", Status: 200, LatencyMS: 300, At: now})

	f := newServedStore(path).data // reloaded from disk
	if got := statusLineText(f, "s1", "gpt-6.1-sol", now); !strings.Contains(got, "served: gpt-6.1-sol") || !strings.Contains(got, "4.1s") {
		t.Fatalf("status line = %q", got)
	}
	if got := statusLineText(f, "nope", "x", now); !strings.Contains(got, "waiting") {
		t.Fatalf("unknown session = %q", got)
	}
	st.record("s1", "gpt-6.1-sol", servedEntry{Served: "glm-5.3", Status: 200, At: now, Fallback: true})
	if got := statusLineText(st.data, "s1", "gpt-6.1-sol", now); !strings.Contains(got, "FALLBACK") || !strings.Contains(got, "glm-5.3") {
		t.Fatalf("fallback not loud: %q", got)
	}
	st.record("s1", "gpt-6.1-sol", servedEntry{Served: "gpt-6.1-sol", Status: 503, At: now})
	if got := statusLineText(st.data, "s1", "gpt-6.1-sol", now); !strings.HasPrefix(got, "✗") || !strings.Contains(got, "503") {
		t.Fatalf("error not loud: %q", got)
	}
}

func TestServedStorePrunesOldSessions(t *testing.T) {
	st := newServedStore(filepath.Join(t.TempDir(), "served.json"))
	old := time.Now().Add(-48 * time.Hour)
	st.record("old", "m", servedEntry{At: old})
	st.record("new", "m", servedEntry{At: time.Now()})
	if _, ok := st.data.Sessions["old"]; ok {
		t.Fatalf("session older than %s was kept", servedKeepFor)
	}
}

// The relay records the model it really sent upstream, per Claude session.
func TestProxyRecordsServedModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg := &config{MaxBodyBytes: defaultMaxBodyBytes, Routes: []route{
		{match: "*", upstream: up.URL, upstreamURL: mustParseURL(t, up.URL), authKind: "none"},
	}}
	s := newServer(cfg, false)
	s.served = newServedStore(filepath.Join(t.TempDir(), "served.json"))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-6.1-sol"}`))
	req.Header.Set("X-Claude-Code-Session-Id", "sess-1")
	s.ServeHTTP(httptest.NewRecorder(), req)
	ct := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"other"}`))
	ct.Header.Set("X-Claude-Code-Session-Id", "sess-1")
	s.ServeHTTP(httptest.NewRecorder(), ct)

	e, ok := s.served.data.Sessions["sess-1"].Models["gpt-6.1-sol"]
	if !ok || e.Served != "gpt-6.1-sol" || e.Status != 200 || e.Fallback {
		t.Fatalf("served entry = %+v ok=%v", e, ok)
	}
	if _, bad := s.served.data.Sessions["sess-1"].Models["other"]; bad {
		t.Fatalf("count_tokens call was recorded as a served model")
	}
}
