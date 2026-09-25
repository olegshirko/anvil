package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// Whole-filesystem views of a container: `docker export` (tar of the
// rootfs) and `docker diff` (changes against the image, read straight from
// the overlay upper directory).

// liveRootfsPath is where containerd mounts a task's rootfs in the guest.
func liveRootfsPath(ns, containerdID string) string {
	return filepath.Join("/run/containerd/io.containerd.runtime.v2.task", ns, containerdID, "rootfs")
}

// withContainerRootfs calls fn with the container's merged rootfs: the live
// task mount when a task holds it, otherwise a temporary snapshot mount.
func withContainerRootfs(ctx context.Context, ns, containerdID string, fn func(root string) error) error {
	if running, status, ok := containerTaskState(ctx, ns, containerdID); ok && (running || status == "paused") {
		if _, err := os.Stat(liveRootfsPath(ns, containerdID)); err == nil {
			return fn(liveRootfsPath(ns, containerdID))
		}
	}
	return withRootfsMount(ns, containerdID, fn)
}

// writeTracker records whether anything reached the client, so a failure
// before the first byte can still become a JSON error.
type writeTracker struct {
	w       http.ResponseWriter
	started bool
}

func (t *writeTracker) Write(p []byte) (int, error) {
	if !t.started {
		t.w.Header().Set("Content-Type", "application/x-tar")
		t.started = true
	}
	return t.w.Write(p)
}

func handleContainerExport(w http.ResponseWriter, r *http.Request, p routeParams) {
	ns, cid, _, err := resolveDockerID(r.Context(), p["id"])
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	out := &writeTracker{w: w}
	err = withContainerRootfs(r.Context(), ns, cid, func(root string) error {
		var stderr bytes.Buffer
		// Numeric owners: the guest's /etc/passwd would map the container's
		// uids to the wrong names.
		cmd := exec.CommandContext(r.Context(), "/bin/tar", "--numeric-owner", "-C", root, "-cf", "-", ".")
		cmd.Stdout = out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("tar: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	})
	if err != nil && !out.started {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	}
}

// --- docker diff -------------------------------------------------------------

// Docker change kinds.
const (
	changeModified = 0
	changeAdded    = 1
	changeDeleted  = 2
)

type containerChange struct {
	Path string `json:"Path"`
	Kind int    `json:"Kind"`
}

func handleContainerChanges(w http.ResponseWriter, r *http.Request, p routeParams) {
	changes, err := containerChanges(r.Context(), p["id"])
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(changes)
}

func containerChanges(ctx context.Context, ref string) ([]containerChange, error) {
	ns, cid, _, err := resolveDockerID(ctx, ref)
	if err != nil {
		return nil, err
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, cid)
	if err != nil {
		return nil, fmt.Errorf("load container: %w", err)
	}
	info, err := c.Info(nsCtx)
	if err != nil || info.SnapshotKey == "" {
		return nil, fmt.Errorf("container has no rootfs snapshot")
	}
	mounts, err := cl.SnapshotService(info.Snapshotter).Mounts(nsCtx, info.SnapshotKey)
	if err != nil {
		return nil, fmt.Errorf("snapshot mounts: %w", err)
	}
	upper, lowers, err := overlayLayers(mounts)
	if err != nil {
		return nil, err
	}
	// Mountpoints runc creates in the upper dir (/etc/hosts, volume
	// targets, ...) are not user changes; Docker hides them in its init
	// layer.
	ignore := map[string]bool{}
	if spec, serr := c.Spec(nsCtx); serr == nil {
		for _, m := range spec.Mounts {
			ignore[path.Clean("/"+m.Destination)] = true
		}
	}
	return overlayChanges(upper, lowers, ignore)
}

// overlayLayers extracts the upper and lower directories (top first) from a
// snapshot's mount list. A snapshot without parents is a plain bind of its
// upper directory.
func overlayLayers(mounts []mount.Mount) (upper string, lowers []string, err error) {
	if len(mounts) != 1 {
		return "", nil, fmt.Errorf("unexpected snapshot mount count %d", len(mounts))
	}
	m := mounts[0]
	switch m.Type {
	case "bind":
		return m.Source, nil, nil
	case "overlay":
		for _, o := range m.Options {
			if v, ok := strings.CutPrefix(o, "upperdir="); ok {
				upper = v
			} else if v, ok := strings.CutPrefix(o, "lowerdir="); ok {
				lowers = strings.Split(v, ":")
			}
		}
		if upper == "" {
			return "", nil, fmt.Errorf("snapshot is read-only (no upperdir)")
		}
		return upper, lowers, nil
	}
	return "", nil, fmt.Errorf("unsupported snapshot mount type %q", m.Type)
}

// isOverlayWhiteout reports whether fi is an overlayfs whiteout: a 0/0
// character device.
func isOverlayWhiteout(fi fs.FileInfo) bool {
	if fi.Mode()&fs.ModeCharDevice == 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Rdev == 0
}

// overlayChanges lists the upper directory's entries as Docker changes:
// whiteouts are deletions, entries shadowing a lower path are
// modifications, the rest additions. Ignored paths (mountpoints) are
// dropped, and so is a directory that only shows up because of them — a
// directory is listed as modified when something reported lives under it
// or it has no upper children at all (its own metadata changed).
//
// Simplification: opaque directories are not special-cased; lower entries
// they hide are not reported as deleted.
func overlayChanges(upper string, lowers []string, ignore map[string]bool) ([]containerChange, error) {
	type entry struct {
		kind  int
		isDir bool
	}
	entries := map[string]entry{}
	hasChildren := map[string]bool{}
	inLower := func(p string) bool {
		for _, l := range lowers {
			if fi, err := os.Lstat(filepath.Join(l, p)); err == nil {
				return !isOverlayWhiteout(fi)
			}
		}
		return false
	}
	err := filepath.WalkDir(upper, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(upper, full)
		if rel == "." {
			return nil
		}
		p := "/" + filepath.ToSlash(rel)
		hasChildren[path.Dir(p)] = true
		if ignore[p] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case isOverlayWhiteout(fi):
			entries[p] = entry{kind: changeDeleted}
		case inLower(p):
			entries[p] = entry{kind: changeModified, isDir: d.IsDir()}
		default:
			entries[p] = entry{kind: changeAdded, isDir: d.IsDir()}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Deepest first, so a directory's verdict sees its reported children.
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di > dj
		}
		return paths[i] < paths[j]
	})
	reportedUnder := map[string]bool{}
	var out []containerChange
	for _, p := range paths {
		e := entries[p]
		if e.isDir && e.kind == changeModified && !reportedUnder[p] && hasChildren[p] {
			continue
		}
		out = append(out, containerChange{Path: p, Kind: e.kind})
		for dir := path.Dir(p); dir != "/"; dir = path.Dir(dir) {
			reportedUnder[dir] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if out == nil {
		out = []containerChange{}
	}
	return out, nil
}
