package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"claude-*", "claude-fable-5-1", true},
		{"claude-*", "claude-opus-5", true},
		{"claude-*", "glm-5.3-flash", false},
		{"*", "glm-5.3-flash", true},
		// The whole point of hand-rolling this matcher: real model ids
		// contain slashes, and path.Match/filepath.Match refuse to let
		// '*' cross a '/'. These must still match.
		{"*", "ogo/glm-5", true},
		{"*", "z-ai/glm-5.2", true},
		{"claude-*", "ogo/glm-5", false},
		{"z-ai/*", "z-ai/glm-5.2", true},
		{"z-ai/*", "ogo/z-ai/glm-5.2", false},
		{"*z-ai/*", "ogo/z-ai/glm-5.2", true},
		{"claude-fable-?-?", "claude-fable-5-1", true},
		{"claude-fable-?-?", "claude-fable-5-12", false},
		{"", "", true},
		{"", "x", false},
		{"*", "", true},
	}
	for _, c := range cases {
		got := globMatch(c.pattern, c.s)
		if got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestRouteMatchOrder(t *testing.T) {
	routes := []route{
		{match: "claude-*", auth: "passthrough"},
		{match: "*", auth: "bearer:env:X"},
	}

	tests := []struct {
		modelID   string
		wantMatch string
	}{
		{"claude-fable-5-1", "claude-*"},
		{"claude-opus-5", "claude-*"},
		// Slash-bearing real aggregator ids must hit the "*" fallback, not
		// error and not accidentally match "claude-*". A test using only
		// slash-free ids would pass against a broken filepath.Match-based
		// router and prove nothing about the trap this exists to catch.
		{"ogo/glm-5", "*"},
		{"z-ai/glm-5.2", "*"},
		{"glm-5.3-flash", "*"},
	}

	for _, tt := range tests {
		rt, ok := matchRoute(routes, tt.modelID)
		if !ok {
			t.Errorf("matchRoute(%q): no match, want %q", tt.modelID, tt.wantMatch)
			continue
		}
		if rt.match != tt.wantMatch {
			t.Errorf("matchRoute(%q) = %q, want %q", tt.modelID, rt.match, tt.wantMatch)
		}
	}
}

func TestRouteMatchOrderNoFallback(t *testing.T) {
	routes := []route{
		{match: "claude-*", auth: "passthrough"},
	}
	if _, ok := matchRoute(routes, "glm-5.3-flash"); ok {
		t.Errorf("matchRoute: expected no match with no fallback route configured")
	}
}

func TestConfigRejectsNonLastWildcard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	badConfig := `{
		"listen": "127.0.0.1:0",
		"routes": [
			{"match": "*", "upstream": "http://127.0.0.1:1", "auth": "none"},
			{"match": "claude-*", "upstream": "http://127.0.0.1:2", "auth": "passthrough"}
		]
	}`
	if err := os.WriteFile(path, []byte(badConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig: expected an error for a non-last \"*\" route, got nil")
	}
	if !strings.Contains(err.Error(), "must be last") {
		t.Errorf("loadConfig error = %q, want it to mention the \"*\" route must be last", err.Error())
	}
}

func TestConfigAcceptsLastWildcard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	goodConfig := `{
		"listen": "127.0.0.1:0",
		"routes": [
			{"match": "claude-*", "upstream": "http://127.0.0.1:2", "auth": "passthrough"},
			{"match": "*", "upstream": "http://127.0.0.1:1", "auth": "none"}
		]
	}`
	if err := os.WriteFile(path, []byte(goodConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: unexpected error: %v", err)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("loadConfig: got %d routes, want 2", len(cfg.Routes))
	}
}
