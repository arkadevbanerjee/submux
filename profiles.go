// profiles.go persists the `submux pick` create-wizard's saved setups to
// ~/.config/submux/profiles.json (spec §2.6). A profile names five model
// ids (main-loop plus the four subagent tiers, any of which may be unset --
// that tier is simply skipped at launch, matching today's submux-claude
// behaviour for an omitted flag) and a usage history used to sort screen 1
// most-used-first.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Profile is one saved setup. Uses and LastUsed are stamped at LAUNCH time
// only (§2.6) -- creating or editing a profile never touches them beyond
// the zero value a brand new profile starts with.
type Profile struct {
	Name     string `json:"name"`
	Main     string `json:"main,omitempty"`
	Fable    string `json:"fable,omitempty"`
	Opus     string `json:"opus,omitempty"`
	Sonnet   string `json:"sonnet,omitempty"`
	Haiku    string `json:"haiku,omitempty"`
	Uses     int    `json:"uses"`
	LastUsed string `json:"last_used,omitempty"` // RFC3339, empty if never launched
}

type profilesFile struct {
	Profiles []Profile `json:"profiles"`
}

// profilesPath returns ~/.config/submux/profiles.json for the current user.
func profilesPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "submux", "profiles.json"), nil
}

// loadProfiles reads path and returns its profiles, already sorted
// (sortProfiles). A missing file is NOT an error -- it returns an empty
// history. A corrupt (unparsable) file is also not fatal: it returns an
// empty history plus a human warning string the caller surfaces in the
// status bar, per spec §2.6 "must NOT be fatal".
func loadProfiles(path string) (profiles []Profile, warning string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		return nil, fmt.Sprintf("profiles.json unreadable (%v), starting with empty history", err)
	}
	var pf profilesFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Sprintf("profiles.json is corrupt (%v), starting with empty history", err)
	}
	sortProfiles(pf.Profiles)
	return pf.Profiles, ""
}

// sortProfiles sorts in place by Uses DESC, then LastUsed DESC (both parsed
// as RFC3339; an unparsable/empty LastUsed sorts last within its Uses
// bucket). sort.SliceStable so two profiles tied on both keys keep their
// on-disk order (spec §4.1 "ties are stable").
func sortProfiles(profiles []Profile) {
	sort.SliceStable(profiles, func(i, j int) bool {
		if profiles[i].Uses != profiles[j].Uses {
			return profiles[i].Uses > profiles[j].Uses
		}
		ti, erri := time.Parse(time.RFC3339, profiles[i].LastUsed)
		tj, errj := time.Parse(time.RFC3339, profiles[j].LastUsed)
		switch {
		case erri != nil && errj != nil:
			return false // neither parses: preserve on-disk order (stable)
		case erri != nil:
			return false // i has no usable timestamp: sorts after j
		case errj != nil:
			return true // j has no usable timestamp: i sorts before it
		default:
			return ti.After(tj)
		}
	})
}

// atomicWriteFile writes data to path via a temp file in the same directory
// followed by a rename, so a reader never observes a partial write and a
// failed write leaves the original file untouched (spec §4.4). The temp
// file is created with os.CreateTemp so a failure (e.g. the directory does
// not exist, or is not writable) surfaces before anything at path is
// touched.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// saveProfiles writes profiles to path atomically (unsorted on disk is
// fine; loadProfiles sorts on read).
func saveProfiles(path string, profiles []Profile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(profilesFile{Profiles: profiles}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal profiles: %w", err)
	}
	return atomicWriteFile(path, data)
}

// touchProfile increments Uses and stamps LastUsed=now for the profile
// named name (spec §2.6: only at launch, never at save). Returns the
// updated slice; a no-op if name is not found.
func touchProfile(profiles []Profile, name string, now time.Time) []Profile {
	for i := range profiles {
		if profiles[i].Name == name {
			profiles[i].Uses++
			profiles[i].LastUsed = now.Format(time.RFC3339)
			break
		}
	}
	return profiles
}

// relativeTime renders t (RFC3339, possibly empty/unparsable) as a short
// human string for screen 1 ("2h ago", "yesterday", "Sep 16"), matching the
// Devin-CLI chrome's terse labels (spec §2.2).
func relativeTime(rfc3339 string, now time.Time) string {
	if rfc3339 == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d / time.Minute)
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		return fmt.Sprintf("%dh ago", h)
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd ago", days)
	default:
		return t.Format("Jan 2")
	}
}
