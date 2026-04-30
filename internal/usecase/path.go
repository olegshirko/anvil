package usecase

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PathValidator defines the interface for path validation.
type PathValidator interface {
	IsMounted(path string) (bool, error)
}

// CleanPath returns the absolute path to the mount location.
// If location is an empty string, nothing is done.
func CleanPath(location string, homeDir string) (string, error) {
	if location == "" {
		return "", nil
	}

	str := os.ExpandEnv(location)

	if strings.HasPrefix(str, "~") {
		str = strings.Replace(str, "~", homeDir, 1)
	}

	str = filepath.Clean(str)
	if !filepath.IsAbs(str) {
		return "", fmt.Errorf("relative paths not supported for mount '%s'", location)
	}

	return strings.TrimSuffix(str, "/") + "/", nil
}
