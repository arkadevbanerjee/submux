package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Test spec §4.1: history sorts by uses DESC then last_used DESC; ties are
// stable (on-disk order preserved for equal uses+last_used).
func TestSortProfilesUsesDescThenLastUsedDescStable(t *testing.T) {
	t3 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	t1 := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	profiles := []Profile{
		{Name: "low-uses-recent", Uses: 1, LastUsed: t3},
		{Name: "high-uses-old", Uses: 5, LastUsed: t1},
		{Name: "high-uses-new", Uses: 5, LastUsed: t3},
		{Name: "tie-a", Uses: 2, LastUsed: t1},
		{Name: "tie-b", Uses: 2, LastUsed: t1}, // exact tie with tie-a: must stay after it
	}
	sortProfiles(profiles)

	got := make([]string, len(profiles))
	for i, p := range profiles {
		got[i] = p.Name
	}
	want := []string{"high-uses-new", "high-uses-old", "tie-a", "tie-b", "low-uses-recent"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortProfiles order = %v, want %v", got, want)
		}
	}
}

// Test spec §4.2: uses increments and last_used stamps only on LAUNCH
// (touchProfile), never merely by saving a freshly created profile.
func TestUsesAndLastUsedOnlyStampAtLaunch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")

	fresh := Profile{Name: "new-setup", Main: "claude-opus-5"}
	if err := saveProfiles(path, []Profile{fresh}); err != nil {
		t.Fatalf("saveProfiles: %v", err)
	}
	loaded, warn := loadProfiles(path)
	if warn != "" {
		t.Fatalf("unexpected warning: %s", warn)
	}
	if len(loaded) != 1 || loaded[0].Uses != 0 || loaded[0].LastUsed != "" {
		t.Fatalf("save alone must not stamp uses/last_used, got %+v", loaded)
	}

	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	touched := touchProfile(loaded, "new-setup", now)
	if touched[0].Uses != 1 {
		t.Fatalf("touchProfile: Uses = %d, want 1", touched[0].Uses)
	}
	if touched[0].LastUsed != now.Format(time.RFC3339) {
		t.Fatalf("touchProfile: LastUsed = %q, want %q", touched[0].LastUsed, now.Format(time.RFC3339))
	}
}

// Test spec §4.3: a corrupt profiles.json is not fatal -- empty history,
// no panic, and a warning surfaced to the caller.
// RED CONTROL: this fails if loadProfiles panics or returns a non-empty
// warning-less result for garbage JSON -- e.g. drop the `if err := ...
// Unmarshal` corruption check in loadProfiles and this goes red.
func TestLoadProfilesCorruptFileDegradesGracefully(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("loadProfiles panicked on corrupt file: %v", r)
		}
	}()
	profiles, warning := loadProfiles(path)
	if len(profiles) != 0 {
		t.Fatalf("corrupt file: profiles = %v, want empty", profiles)
	}
	if warning == "" {
		t.Fatalf("corrupt file: expected a non-empty warning")
	}
}

// Test spec §4.4: atomic write leaves no partial file when the temp write
// fails. We force the failure by making the target directory unwritable
// (CreateTemp fails), then assert the original file's content is untouched.
func TestAtomicWriteFileLeavesNoPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	original := []byte(`{"profiles":[{"name":"keep-me","uses":3}]}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed original file: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := atomicWriteFile(path, []byte(`{"profiles":[{"name":"new-content"}]}`))
	if err == nil {
		t.Fatalf("atomicWriteFile: expected an error writing into a read-only dir")
	}

	os.Chmod(dir, 0o700)
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back original file: %v", readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("original file was modified on a failed write: got %q, want %q", got, original)
	}
}
