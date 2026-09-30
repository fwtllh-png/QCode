// Package atomicfile provides durable same-directory file replacement.
package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Replace writes data to a temporary file, syncs it, atomically replaces path,
// and syncs the parent directory.
func Replace(path string, data []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(
		directory,
		"."+filepath.Base(path)+"-*.tmp",
	)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
