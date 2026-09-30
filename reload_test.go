package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCfg(t *testing.T, path, extra string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(sprintfCfg(extra)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sprintfCfg(extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return "{\"listen\":\"127.0.0.1:0\",\"routes\":[{\"match\":\"*\",\"upstream\":\"http://127.0.0.1:1\",\"auth\":\"none\"" + extra + "}]}"
}

func TestReloadConfigSwapsAndKeepsOldOnBadEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeCfg(t, path, "")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("seed config does not load: %v", err)
	}
	s := newServer(cfg, false)
	old := s.conf()

	writeCfg(t, path, `"model_rewrite":{"a":"b"}`)
	if err := s.reloadConfig(path); err != nil {
		t.Fatalf("reload of a valid edit: %v", err)
	}
	if s.conf() == old {
		t.Fatalf("config pointer was not swapped")
	}
	if s.conf().Routes[0].modelRewrite["a"] != "b" {
		t.Fatalf("reloaded config lacks the edit: %+v", s.conf().Routes[0])
	}

	good := s.conf()
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.reloadConfig(path); err == nil {
		t.Fatalf("bad edit reloaded without error")
	}
	if s.conf() != good {
		t.Fatalf("a failed reload replaced the running config")
	}
}

func TestWatchConfigPicksUpEditWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeCfg(t, path, "")
	cfg, _ := loadConfig(path)
	s := newServer(cfg, false)
	go s.watchConfig(path, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	writeCfg(t, path, `"model_rewrite":{"x":"y"}`)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.conf().Routes[0].modelRewrite["x"] == "y" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watcher did not reload the edited config within 2s")
}
