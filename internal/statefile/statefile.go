// Package statefile persists private daemon state with atomic replacement.
package statefile

import (
	"os"
	"path/filepath"
)

// Write syncs the file before replacement and its directory afterward.
// An error may occur after replacement; callers must treat it as uncertain.
func Write(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".gauntlet-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
