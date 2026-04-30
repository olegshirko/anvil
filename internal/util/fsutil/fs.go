package fsutil

import (
	"fmt"
	"os"
)

// EnsureDir creates the directory path and all its parents if they do not exist.
// It is a convenience wrapper around os.MkdirAll with fixed permissions.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0750); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", path, err)
	}
	return nil
}
