package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ensureMountpoint(target string, dir bool) error {
	if _, err := os.Lstat(target); err == nil {
		return nil
	}
	if dir {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// resolveInRoot resolves path inside root the way the container would see
// it: symlinks are followed relative to root (absolute targets restart at
// root), ".." never climbs above it. Missing trailing components are
// joined as they are. A rootfs from an image can hold any symlink; mounting
// onto a path resolved by the kernel would follow it out of the rootfs.
func resolveInRoot(root, path string) (string, error) {
	parts := strings.Split(filepath.Clean("/"+path), "/")
	cur := "/"
	for links := 0; len(parts) > 0; {
		p := parts[0]
		parts = parts[1:]
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, p)
		fi, err := os.Lstat(filepath.Join(root, next))
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.Join(root, next, filepath.Join(append([]string{"/"}, parts...)...)), nil
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 255 {
			return "", fmt.Errorf("too many symlinks in %s", path)
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			cur = "/"
		}
		parts = append(strings.Split(target, "/"), parts...)
	}
	return filepath.Join(root, cur), nil
}
