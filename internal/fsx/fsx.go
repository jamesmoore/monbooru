// Package fsx holds the filesystem helpers several packages share.
package fsx

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic is atomic, not durable: a caller that needs the content to
// survive a crash calls Sync at the end of write and SyncDir after.
func WriteAtomic(path, pattern string, write func(*os.File) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// SyncDir is best effort: a refused open or fsync still leaves the renamed
// file.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
