// served.go records which model ACTUALLY answered each Claude Code session, so
// a status line can show the truth instead of the name the picker printed.
// The relay writes one small JSON file; `submux statusline` reads it.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	servedKeepSessions = 40
	servedKeepFor      = 24 * time.Hour
)

// servedEntry is the latest completed request for one (session, requested model).
type servedEntry struct {
	Served    string    `json:"served"` // the model id the relay finally sent upstream
	Status    int       `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	At        time.Time `json:"at"`
	Fallback  bool      `json:"fallback,omitempty"` // served != requested
}

type servedSession struct {
	Updated time.Time              `json:"updated"`
	Models  map[string]servedEntry `json:"models"` // requested model -> latest entry
}

type servedFile struct {
	Sessions map[string]*servedSession `json:"sessions"`
}

type servedStore struct {
	mu   sync.Mutex
	path string
	data servedFile
}

func servedPath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "submux", "served.json"), nil
}

func newServedStore(path string) *servedStore {
	s := &servedStore{path: path, data: servedFile{Sessions: map[string]*servedSession{}}}
	if b, err := os.ReadFile(path); err == nil {
		var f servedFile
		if json.Unmarshal(b, &f) == nil && f.Sessions != nil {
			s.data = f
		}
	}
	return s
}

// record stores one finished request. Best effort: a write failure never
// affects the proxied response.
func (s *servedStore) record(session, requested string, e servedEntry) {
	if s == nil || s.path == "" || session == "" || requested == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.data.Sessions[session]
	if ss == nil {
		ss = &servedSession{Models: map[string]servedEntry{}}
		s.data.Sessions[session] = ss
	}
	ss.Updated = e.At
	ss.Models[requested] = e
	s.pruneLocked(e.At)
	b, err := json.Marshal(s.data)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func (s *servedStore) pruneLocked(now time.Time) {
	for id, ss := range s.data.Sessions {
		if now.Sub(ss.Updated) > servedKeepFor {
			delete(s.data.Sessions, id)
		}
	}
	if len(s.data.Sessions) <= servedKeepSessions {
		return
	}
	ids := make([]string, 0, len(s.data.Sessions))
	for id := range s.data.Sessions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return s.data.Sessions[ids[i]].Updated.Before(s.data.Sessions[ids[j]].Updated) })
	for _, id := range ids[:len(ids)-servedKeepSessions] {
		delete(s.data.Sessions, id)
	}
}

// statusLineText renders the status line for one Claude Code session.
func statusLineText(f servedFile, session, model string, now time.Time) string {
	model = strings.TrimSuffix(model, "[1m]")
	ss := f.Sessions[session]
	if ss == nil {
		return "served: waiting for first reply"
	}
	e, ok := ss.Models[model]
	if !ok {
		// Claude shows a display name; fall back to the session's newest entry.
		for _, cand := range ss.Models {
			if !ok || cand.At.After(e.At) {
				e, ok = cand, true
			}
		}
		if !ok {
			return "served: waiting for first reply"
		}
	}
	age := now.Sub(e.At).Round(time.Second)
	lat := fmt.Sprintf("%.1fs", float64(e.LatencyMS)/1000)
	switch {
	case e.Status >= 400:
		return fmt.Sprintf("✗ %s answered HTTP %d · %s ago", e.Served, e.Status, age)
	case e.Fallback:
		return fmt.Sprintf("⚠ FALLBACK served by %s (asked for %s) · %s", e.Served, model, lat)
	}
	return fmt.Sprintf("served: %s · %s · %s ago", e.Served, lat, age)
}

// cmdStatusline is the Claude Code statusLine command: stdin is Claude's JSON
// (session_id, model.id), stdout is one line.
func cmdStatusline(_ []string) {
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	var in struct {
		SessionID string `json:"session_id"`
		Model     struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	_ = json.Unmarshal(raw, &in)
	p, err := servedPath()
	if err != nil {
		fmt.Println("served: ?")
		return
	}
	fmt.Println(statusLineText(newServedStore(p).data, in.SessionID, in.Model.ID, time.Now()))
}
