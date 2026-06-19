package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveLoadRoundTrip is the core contract: a Saved offset comes back from Load
// byte-for-byte, surviving a process boundary (a fresh OffsetStore over the same path).
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offset.json")
	s := NewOffsetStore(path)

	if err := s.Save(4242); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Fresh store over the same path — simulates a restart reading what the prior run wrote.
	got := NewOffsetStore(path).Load()
	if got != 4242 {
		t.Fatalf("Load after Save = %d, want 4242", got)
	}
}

// TestLoadMissingReturnsZero — a fresh install (no file yet) starts the long-poll at 0,
// never errors.
func TestLoadMissingReturnsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "offset.json")
	if got := NewOffsetStore(path).Load(); got != 0 {
		t.Fatalf("Load on missing path = %d, want 0", got)
	}
}

// TestLoadCorruptReturnsZero — a half-written or garbage file must not panic or
// propagate an error; it just resets to 0 (the loop re-syncs from Telegram).
func TestLoadCorruptReturnsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offset.json")
	if err := os.WriteFile(path, []byte("\x00not json at all{{{"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if got := NewOffsetStore(path).Load(); got != 0 {
		t.Fatalf("Load on corrupt file = %d, want 0", got)
	}
}

// TestSaveAtomicNoTempLeftover proves the atomic write leaves no scratch file behind,
// and that overwriting an existing offset works (10 then 20 -> Load is 20).
func TestSaveAtomicNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "offset.json")
	s := NewOffsetStore(path)

	if err := s.Save(10); err != nil {
		t.Fatalf("Save(10): %v", err)
	}
	if err := s.Save(20); err != nil {
		t.Fatalf("Save(20): %v", err)
	}

	if got := s.Load(); got != 20 {
		t.Fatalf("Load after overwrite = %d, want 20", got)
	}

	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatalf("glob dir: %v", err)
	}
	if len(entries) != 1 || entries[0] != path {
		t.Fatalf("dir contents = %v, want only %q (no leftover temp file)", entries, path)
	}
}

// TestSaveCreatesMissingParentDir — the store owns creating its directory so callers
// don't have to pre-make it.
func TestSaveCreatesMissingParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "offset.json")
	s := NewOffsetStore(path)

	if err := s.Save(7); err != nil {
		t.Fatalf("Save into missing parent dir: %v", err)
	}
	if got := s.Load(); got != 7 {
		t.Fatalf("Load = %d, want 7", got)
	}
}
