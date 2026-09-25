package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Tar archiving for docker cp / docker export, in Go instead of an external
// tar: it runs inside inChroot (chroot_linux.go), so every path — including
// symlinks a running container swaps in mid-copy — resolves within the
// container's root and can never reach the VM (whose root holds the Mac
// shares). Names are written relative, owners numeric, as Docker does.

// writeTarTree archives src (a path in the current root) into tw. Entries
// are named prefix/<relative path>; with an empty prefix the src entry
// itself is omitted (docker export: the rootfs contents only). Symlinks are
// stored, never followed; hard links inside the tree are kept as links.
func writeTarTree(tw *tar.Writer, src, prefix string) error {
	inodes := map[[2]uint64]string{}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if prefix == "" && rel == "." {
			return nil
		}
		name := filepath.ToSlash(filepath.Join(prefix, rel))
		fi, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", ""
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
			if fi.Mode().IsRegular() && st.Nlink > 1 {
				key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
				if first, seen := inodes[key]; seen {
					hdr.Typeflag = tar.TypeLink
					hdr.Linkname = first
					hdr.Size = 0
				} else {
					inodes[key] = hdr.Name
				}
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		return nil
	})
}

// extractTar unpacks r into dst (a directory in the current root). Entry
// names are confined lexically to dst; within the chroot the kernel keeps
// symlink resolution inside the container as well.
func extractTar(r io.Reader, dst string) error {
	type dirTime struct {
		path  string
		mtime time.Time
	}
	var dirs []dirTime
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.Clean("/"+hdr.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := hdr.FileInfo().Mode()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if fi, err := os.Lstat(target); err != nil || !fi.IsDir() {
				os.Remove(target)
				if err := os.Mkdir(target, mode.Perm()); err != nil && !os.IsExist(err) {
					return err
				}
			}
			dirs = append(dirs, dirTime{target, hdr.ModTime})
		case tar.TypeReg:
			os.Remove(target)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm())
			if err != nil {
				return err
			}
			_, cerr := io.Copy(f, tr)
			if err := f.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return cerr
			}
		case tar.TypeSymlink:
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			os.Remove(target)
			if err := os.Link(filepath.Join(dst, filepath.Clean("/"+hdr.Linkname)), target); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			os.Remove(target)
			kind := uint32(unix.S_IFIFO)
			switch hdr.Typeflag {
			case tar.TypeChar:
				kind = unix.S_IFCHR
			case tar.TypeBlock:
				kind = unix.S_IFBLK
			}
			dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
			if err := unix.Mknod(target, kind|uint32(mode.Perm()), int(dev)); err != nil {
				return err
			}
		default:
			continue // xattr-only and other extended records
		}
		if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil && !errors.Is(err, fs.ErrPermission) {
			return err
		}
		if hdr.Typeflag != tar.TypeSymlink {
			// Chmod after chown: chown clears setuid/setgid bits.
			if err := os.Chmod(target, mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
				return err
			}
			if hdr.Typeflag != tar.TypeDir {
				os.Chtimes(target, hdr.AccessTime, hdr.ModTime) //nolint:errcheck
			}
		}
	}
	// Directory times last: creating their children bumped them.
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chtimes(dirs[i].path, dirs[i].mtime, dirs[i].mtime) //nolint:errcheck
	}
	return nil
}

// containerPath normalizes a docker cp path to an absolute path within the
// (chrooted) container root.
func containerPath(p string) string {
	return filepath.Clean("/" + strings.TrimSpace(p))
}
