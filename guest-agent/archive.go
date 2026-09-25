package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func handleArchiveGet(w http.ResponseWriter, r *http.Request, ns, containerdID, srcPath string) {
	stat, err := statContainerPath(ns, containerdID, srcPath)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	statJSON, _ := json.Marshal(stat)
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(statJSON))

	tmpFile, err := createContainerTar(ns, containerdID, srcPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmpFile)

	f, err := os.Open(tmpFile)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, f)
}

func handleArchivePut(w http.ResponseWriter, r *http.Request, ns, containerdID, dstPath string) {
	// Docker CLI sends a tar stream. Buffer it (bounded by what docker cp
	// sends in practice), then extract inside the container or into its
	// rootfs snapshot when it is not running.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := extractTarIntoContainer(ns, containerdID, body, dstPath); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// statContainerPath stats the path and returns a Docker-compatible stat.
// The path is resolved on the guest side (see withContainerFS), so it works
// for images without a shell or stat binary (distroless, scratch). As in
// Docker, a final symlink is reported, not followed.
func statContainerPath(ns, containerdID, path string) (dockerPathStat, error) {
	var stat dockerPathStat
	err := withContainerFS(context.Background(), ns, containerdID, func(root string) error {
		target, err := resolveInRoot(root, path, false)
		if err != nil {
			return err
		}
		fi, err := os.Lstat(target)
		if err != nil {
			return err
		}
		stat = dockerPathStat{
			Name:  filepath.Base(filepath.Clean("/" + path)),
			Size:  fi.Size(),
			Mode:  uint32(fi.Mode()),
			Mtime: fi.ModTime().UTC().Format(time.RFC3339),
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if tgt, err := os.Readlink(target); err == nil {
				stat.LinkTarget = tgt
			}
		}
		return nil
	})
	if err != nil {
		return dockerPathStat{}, fmt.Errorf("Could not find the file %s in container %s: %v", path, truncateID(containerdID), err)
	}
	return stat, nil
}

// createContainerTar archives the given path inside the container to a
// temporary guest file, with GNU tar on the guest side — no tar needed in
// the image.
func createContainerTar(ns, containerdID, srcPath string) (string, error) {
	var tmp string
	err := withContainerFS(context.Background(), ns, containerdID, func(root string) error {
		target, err := resolveInRoot(root, srcPath, false)
		if err != nil {
			return err
		}
		f, err := os.CreateTemp("/tmp", "anvil-cp-out-*.tar")
		if err != nil {
			return err
		}
		defer f.Close()
		tarCmd := exec.Command("/bin/tar", "--numeric-owner", "-cf", f.Name(),
			"-C", filepath.Dir(target), filepath.Base(target))
		if out, terr := tarCmd.CombinedOutput(); terr != nil {
			os.Remove(f.Name())
			return fmt.Errorf("tar create: %v: %s", terr, stripANSI(string(out)))
		}
		tmp = f.Name()
		return nil
	})
	if err != nil {
		return "", err
	}
	return tmp, nil
}

// extractTarIntoContainer extracts a tar stream into dstPath inside the
// container. An in-container `tar -xf -` via exec-with-stdin is NOT used:
// containerd's cio stdin fifo does not deliver EOF reliably for one-shot
// writers, so the extract hangs — and the image may have no tar at all.
func extractTarIntoContainer(ns, containerdID string, tarStream []byte, dstPath string) error {
	return withContainerFS(context.Background(), ns, containerdID, func(root string) error {
		target, err := resolveInRoot(root, dstPath, true)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		extract := exec.Command("/bin/tar", "--numeric-owner", "-xf", "-", "-C", target)
		extract.Stdin = strings.NewReader(string(tarStream))
		if out, err := extract.CombinedOutput(); err != nil {
			return fmt.Errorf("tar extract: %v: %s", err, stripANSI(string(out)))
		}
		return nil
	})
}

// withContainerFS calls fn with a guest path showing the container's
// filesystem. A running (or paused) container is reached through its init
// process's /proc/<pid>/root — the container's own mount view, volumes and
// bind mounts included, as docker cp sees them. A stopped one gets its
// rootfs snapshot mounted. Paths below root must go through resolveInRoot:
// the kernel would resolve absolute symlinks against the guest's root.
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

// resolveInRoot maps a container path to a guest path under root, following
// symlinks the way the container would see them: absolute targets restart
// at root and ".." never climbs above it. The final component is followed
// only with followFinal. Missing components are kept lexically, so a
// destination that does not exist yet still resolves.
func resolveInRoot(root, containerPath string, followFinal bool) (string, error) {
	pending := splitContainerPath(containerPath)
	cur := "/"
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, part)
		if len(pending) == 0 && !followFinal {
			cur = next
			break
		}
		fi, err := os.Lstat(filepath.Join(root, next))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > 40 {
			return "", fmt.Errorf("too many levels of symbolic links in %s", containerPath)
		}
		link, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(link) {
			cur = "/"
		}
		pending = append(splitContainerPath(link), pending...)
	}
	return filepath.Join(root, cur), nil
}

func splitContainerPath(p string) []string {
	var out []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
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
