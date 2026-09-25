package main

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// dockerPathStat is the JSON returned in the X-Docker-Container-Path-Stat header.
type dockerPathStat struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	Mtime      string `json:"mtime"`
	LinkTarget string `json:"linkTarget,omitempty"`
}

// handleContainerArchive routes HEAD/GET/PUT /containers/{id}/archive.
func handleContainerArchive(w http.ResponseWriter, r *http.Request, id string) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSONError(w, http.StatusBadRequest, "path is required")
		return
	}
	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}

	switch r.Method {
	case http.MethodHead:
		handleArchiveHead(w, r, ns, containerdID, path)
	case http.MethodGet:
		handleArchiveGet(w, r, ns, containerdID, path)
	case http.MethodPut:
		handleArchivePut(w, r, ns, containerdID, path)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleArchiveHead(w http.ResponseWriter, r *http.Request, ns, containerdID, path string) {
	stat, err := statContainerPath(ns, containerdID, path)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	data, _ := json.Marshal(stat)
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(data))
	w.WriteHeader(http.StatusOK)
}

// handleArchiveGet streams a tar of srcPath straight from the container's
// filesystem to the client (no staging copy in the guest's RAM-backed /tmp).
func handleArchiveGet(w http.ResponseWriter, r *http.Request, ns, containerdID, srcPath string) {
	stat, err := statContainerPath(ns, containerdID, srcPath)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	statJSON, _ := json.Marshal(stat)
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(statJSON))
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)

	src := containerPath(srcPath)
	prefix := filepath.Base(src)
	err = withContainerFS(r.Context(), ns, containerdID, func(root string) error {
		return inChroot(root, func() error {
			tw := tar.NewWriter(w)
			if err := writeTarTree(tw, src, prefix); err != nil {
				return err
			}
			return tw.Close()
		})
	})
	if err != nil {
		// Headers are out; the client sees a truncated stream.
		log.Printf("[docker-api] archive %s:%s: %v", truncateID(containerdID), srcPath, err)
	}
}

// handleArchivePut extracts the request body (a tar stream from docker cp)
// into dstPath, streaming: nothing is buffered in the agent's memory.
func handleArchivePut(w http.ResponseWriter, r *http.Request, ns, containerdID, dstPath string) {
	dst := containerPath(dstPath)
	err := withContainerFS(r.Context(), ns, containerdID, func(root string) error {
		return inChroot(root, func() error {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			return extractTar(r.Body, dst)
		})
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("extract into %s: %v", dstPath, err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// statContainerPath stats the path inside the container (a final symlink is
// reported, not followed, as in Docker).
func statContainerPath(ns, containerdID, path string) (dockerPathStat, error) {
	var stat dockerPathStat
	p := containerPath(path)
	err := withContainerFS(context.Background(), ns, containerdID, func(root string) error {
		return inChroot(root, func() error {
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			stat = dockerPathStat{
				Name:  filepath.Base(p),
				Size:  fi.Size(),
				Mode:  uint32(fi.Mode()),
				Mtime: fi.ModTime().UTC().Format(time.RFC3339),
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				if tgt, err := os.Readlink(p); err == nil {
					stat.LinkTarget = tgt
				}
			}
			return nil
		})
	})
	if err != nil {
		return dockerPathStat{}, fmt.Errorf("Could not find the file %s in container %s: %v", path, truncateID(containerdID), err)
	}
	return stat, nil
}

// withContainerFS calls fn with a guest path showing the container's
// filesystem. A running (or paused) container is reached through its init
// process's /proc/<pid>/root — the container's own mount view, volumes and
// bind mounts included, as docker cp sees them. A stopped one gets its
// rootfs snapshot mounted. Never open paths below root directly: the
// kernel resolves absolute symlinks against the guest's root, and a running
// container can swap them in at any time — use inChroot(root, ...).
func withContainerFS(ctx context.Context, ns, containerdID string, fn func(root string) error) error {
	if pid, ok := containerTaskRootPid(ctx, ns, containerdID); ok {
		return fn(fmt.Sprintf("/proc/%d/root", pid))
	}
	return withRootfsMount(ns, containerdID, fn)
}

// containerTaskRootPid returns the init pid of a running or paused task.
func containerTaskRootPid(ctx context.Context, ns, containerdID string) (int, bool) {
	cl, err := pc.get(ctx)
	if err != nil {
		return 0, false
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, containerdID)
	if err != nil {
		return 0, false
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		return 0, false
	}
	st, err := task.Status(nsCtx)
	if err != nil || (st.Status != "running" && st.Status != "paused") || task.Pid() == 0 {
		return 0, false
	}
	return int(task.Pid()), true
}

// withRootfsMount mounts a stopped container's rootfs snapshot at a temporary
// directory and calls fn with the mount root. Snapshotter mounts require the
// container to have no live task.
func withRootfsMount(ns, containerdID string, fn func(root string) error) error {
	cl, err := pc.get(context.Background())
	if err != nil {
		return err
	}
	ctx := namespaces.WithNamespace(context.Background(), ns)
	c, err := cl.LoadContainer(ctx, containerdID)
	if err != nil {
		return fmt.Errorf("load container: %w", err)
	}
	info, err := c.Info(ctx)
	if err != nil || info.SnapshotKey == "" {
		return fmt.Errorf("container has no rootfs snapshot")
	}
	sn := cl.SnapshotService(info.Snapshotter)
	if sn == nil {
		return fmt.Errorf("snapshotter %q unavailable", info.Snapshotter)
	}
	mounts, err := sn.Mounts(ctx, info.SnapshotKey)
	if err != nil {
		return fmt.Errorf("snapshot mounts: %w", err)
	}
	root, err := os.MkdirTemp("/tmp", "anvil-rootfs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	if err := mountAll(mounts, root); err != nil {
		return fmt.Errorf("mount rootfs: %w", err)
	}
	defer unmountAll(root)
	return fn(root)
}
