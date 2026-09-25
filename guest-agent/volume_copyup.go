package main

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"maps"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Docker volume semantics beyond the mount itself: the image's VOLUME paths
// become anonymous volumes, and a volume that is empty when first mounted is
// seeded with the image's content at that path (copy-up), ownership and
// mode of the directory included. -v name:/path:nocopy and
// --mount ...,volume-nocopy opt out.

func newAnonVolumeName() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck — crypto/rand never fails in practice
	return hex.EncodeToString(b)
}

// imageVolumeMounts creates an anonymous volume for every image VOLUME path
// no other mount covers.
func imageVolumeMounts(ns string, imageVolumes map[string]struct{}, mounts []specs.Mount) ([]string, []volumeMount, error) {
	taken := map[string]bool{}
	for _, m := range mounts {
		taken[filepath.Clean(m.Destination)] = true
	}
	var names []string
	var vms []volumeMount
	for _, dst := range slices.Sorted(maps.Keys(imageVolumes)) {
		dst = filepath.Clean(dst)
		if taken[dst] {
			continue
		}
		taken[dst] = true
		name := newAnonVolumeName()
		dir := volumeDataDir(ns, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
		markAnonymousVolume(ns, name)
		names = append(names, name)
		vms = append(vms, volumeMount{dir: dir, dst: dst})
	}
	return names, vms, nil
}

// copyUpVolumes seeds the empty volumes among vms from the container's
// (not yet started) rootfs snapshot.
func copyUpVolumes(ns, id string, vms []volumeMount) error {
	var todo []volumeMount
	for _, vm := range vms {
		if !vm.nocopy && dirEmpty(vm.dir) {
			todo = append(todo, vm)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	return withRootfsMount(ns, id, func(root string) error {
		var errs []error
		for _, vm := range todo {
			if err := copyTreeBetweenRoots(root, vm.dst, vm.dir); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}

func dirEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

// copyTreeBetweenRoots copies srcPath (inside srcRoot) into dstDir. Both
// sides run chrooted — the reader in the image rootfs, the writer in the
// volume — so symlinks shipped by an image resolve inside the image on read
// and inside the volume on write, never into the VM or its Mac shares.
func copyTreeBetweenRoots(srcRoot, srcPath, dstDir string) error {
	pr, pw := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		produced <- inChroot(srcRoot, func() error {
			resolved, err := filepath.EvalSymlinks(containerPath(srcPath))
			if err != nil {
				pw.Close() // nothing in the image at that path: nothing to copy
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if fi, err := os.Stat(resolved); err != nil || !fi.IsDir() {
				pw.Close()
				return nil
			}
			tw := tar.NewWriter(pw)
			err = writeTarTree(tw, resolved, ".")
			if err == nil {
				err = tw.Close()
			}
			pw.CloseWithError(err)
			return err
		})
	}()
	extractErr := inChroot(dstDir, func() error { return extractTar(pr, "/") })
	pr.CloseWithError(extractErr) // unblock the reader if the writer failed
	if err := <-produced; err != nil {
		return err
	}
	return extractErr
}
