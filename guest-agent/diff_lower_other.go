//go:build !linux

package main

import (
	"os"
	"path/filepath"
)

// lowerLookup: plain lookups for unit tests on the development host (see
// diff_lower_linux.go for the real, symlink-refusing one).
func lowerLookup(lowers []string) (func(p string) bool, func(), error) {
	return func(p string) bool {
		for _, l := range lowers {
			if fi, err := os.Lstat(filepath.Join(l, p)); err == nil {
				return !isOverlayWhiteout(fi)
			}
		}
		return false
	}, func() {}, nil
}
