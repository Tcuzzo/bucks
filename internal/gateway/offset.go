// Package gateway is BUCKS's always-on Telegram gateway. This file is the durable
// offset store: the long-poll loop calls Telegram's getUpdates with an `offset`
// (the id of the next update to receive), and that offset must survive a restart so
// no update is missed or reprocessed. The store persists a single int64 to a JSON
// file, atomically, so a crash mid-write can never corrupt the live value.
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// dirPerm/filePerm keep the offset (and its directory) private to the owner — the
// gateway runs unattended next to the broker keys, so nothing here is world-readable.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// OffsetStore persists a single Telegram getUpdates offset to a JSON file at Path.
// It holds no state beyond the path, so it is safe to construct fresh on each access
// (as the long-poll loop does on resume).
type OffsetStore struct {
	path string
}

// offsetFile is the on-disk shape. A struct (not a bare number) leaves room to add
// fields later without breaking older files, and reads as self-describing JSON.
type offsetFile struct {
	Offset int64 `json:"offset"`
}

// NewOffsetStore returns a store backed by the JSON file at path.
func NewOffsetStore(path string) *OffsetStore {
	return &OffsetStore{path: path}
}

// Load returns the saved offset. A missing, unreadable, or corrupt file is treated as
// "no offset yet" and returns 0 — never an error, never a panic. A fresh install and a
// damaged file both simply restart the poll from 0, which Telegram tolerates (the loop
// re-syncs on the next getUpdates).
func (s *OffsetStore) Load() int64 {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return 0
	}
	var f offsetFile
	if err := json.Unmarshal(data, &f); err != nil {
		return 0
	}
	return f.Offset
}

// Save persists offset atomically: it writes a temp file in the same directory, then
// renames it over the target. A crash before the rename leaves the live file untouched;
// after the rename the new value is fully on disk. The parent directory is created if
// missing.
//
// Atomicity rests on os.Rename being atomic within one filesystem (the temp file is in
// the same dir, so same filesystem). On Windows, os.Rename fails if the target already
// exists, so we remove the target and retry — the small window between remove and rename
// is the cost of a portable overwrite; the temp file already holds the complete new
// value, so no data is lost.
func (s *OffsetStore) Save(offset int64) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create offset dir: %w", err)
	}

	data, err := json.Marshal(offsetFile{Offset: offset})
	if err != nil {
		return fmt.Errorf("marshal offset: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".offset-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp offset file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before a successful rename; after a successful
	// rename tmpName no longer exists, so the Remove is a harmless no-op.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp offset file: %w", err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp offset file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp offset file: %w", err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		// Windows refuses Rename onto an existing file. Remove the stale target and
		// retry; on POSIX this branch is normally not taken because Rename overwrites.
		if errors.Is(err, os.ErrExist) || fileExists(s.path) {
			if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("remove stale offset file: %w", rmErr)
			}
			if err := os.Rename(tmpName, s.path); err != nil {
				return fmt.Errorf("rename offset file (after remove): %w", err)
			}
			return nil
		}
		return fmt.Errorf("rename offset file: %w", err)
	}
	return nil
}

// fileExists reports whether path names an existing file, used to decide the Windows
// remove-then-rename fallback portably (os.ErrExist is not surfaced uniformly).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
