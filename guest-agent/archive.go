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
	"strconv"
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
		http.Error(w, `{"message":"path is required"}`, http.StatusBadRequest)
		return
	}
	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
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
		http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func handleArchiveHead(w http.ResponseWriter, r *http.Request, ns, containerdID, path string) {
	stat, err := statContainerPath(ns, containerdID, path)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
		return
	}
	data, _ := json.Marshal(stat)
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(data))
	w.WriteHeader(http.StatusOK)
}

func handleArchiveGet(w http.ResponseWriter, r *http.Request, ns, containerdID, srcPath string) {
	stat, err := statContainerPath(ns, containerdID, srcPath)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
		return
	}
	statJSON, _ := json.Marshal(stat)
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(statJSON))

	tmpFile, err := createContainerTar(ns, containerdID, srcPath)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpFile)

	f, err := os.Open(tmpFile)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
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
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	if err := extractTarIntoContainer(ns, containerdID, body, dstPath); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// statContainerPath stats the path and returns a Docker-compatible stat.
// Running containers are stat'ed via exec; stopped ones get their rootfs
// snapshot mounted temporarily (no task exists to exec in).
func statContainerPath(ns, containerdID, path string) (dockerPathStat, error) {
	if running, _, stateOK := containerTaskState(context.Background(), ns, containerdID); stateOK && !running {
		var stat dockerPathStat
		err := withRootfsMount(ns, containerdID, func(root string) error {
			fi, err := os.Lstat(filepath.Join(root, filepath.Clean("/"+path)))
			if err != nil {
				return err
			}
			stat = dockerPathStat{
				Name:  filepath.Base(fi.Name()),
				Size:  fi.Size(),
				Mode:  uint32(fi.Mode().Perm()),
				Mtime: fi.ModTime().UTC().Format(time.RFC3339),
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				if tgt, err := os.Readlink(filepath.Join(root, filepath.Clean("/"+path))); err == nil {
					stat.LinkTarget = tgt
				}
			}
			return nil
		})
		if err != nil {
			return dockerPathStat{}, fmt.Errorf("stat failed: %v", err)
		}
		return stat, nil
	}
	script := fmt.Sprintf("stat -c '%%n|%%s|%%a|%%Y|%%N' %s", shellescape(path))
	res, err := runSimpleExec(context.Background(), ns, containerdID,
		[]string{"sh", "-c", script}, "0", "", 30*time.Second)
	if err != nil || res.exitCode != 0 {
		return dockerPathStat{}, fmt.Errorf("stat failed: %v %s%s", err, outOf(res), errOf(res))
	}
	fields := strings.SplitN(strings.TrimSpace(res.stdout), "|", 5)
	if len(fields) < 4 {
		return dockerPathStat{}, fmt.Errorf("unexpected stat output: %q", res.stdout)
	}
	size, _ := strconv.ParseInt(fields[1], 10, 64)
	mode, _ := strconv.ParseUint(fields[2], 8, 32)
	mtimeSec, _ := strconv.ParseInt(fields[3], 10, 64)
	linkTarget := ""
	if len(fields) >= 5 {
		linkTarget = strings.Trim(fields[4], `"'`)
	}
	name := filepath.Base(fields[0])
	return dockerPathStat{
		Name:       name,
		Size:       size,
		Mode:       uint32(mode),
		Mtime:      time.Unix(mtimeSec, 0).UTC().Format(time.RFC3339),
		LinkTarget: linkTarget,
	}, nil
}

// createContainerTar archives the given path inside the container to a
// temporary host file. Running containers stream the in-container tar out
// through exec; stopped ones get their rootfs snapshot mounted and are
// archived directly on the guest.
func createContainerTar(ns, containerdID, srcPath string) (string, error) {
	if running, _, stateOK := containerTaskState(context.Background(), ns, containerdID); stateOK && !running {
		var tmp string
		err := withRootfsMount(ns, containerdID, func(root string) error {
			base := filepath.Base(srcPath)
			dir := filepath.Dir(filepath.Clean("/" + srcPath))
			f, ferr := os.CreateTemp("/tmp", "anvil-cp-out-*.tar")
			if ferr != nil {
				return ferr
			}
			defer f.Close()
			tarCmd := exec.Command("/bin/tar", "-cf", f.Name(), "-C", filepath.Join(root, dir), base)
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
	base := filepath.Base(srcPath)
	dir := filepath.Dir(srcPath)
	script := fmt.Sprintf("tar -cf - -C %s %s", shellescape(dir), shellescape(base))
	res, err := runSimpleExec(context.Background(), ns, containerdID,
		[]string{"sh", "-c", script}, "0", "", 5*time.Minute)
	if err != nil || res.exitCode != 0 {
		return "", fmt.Errorf("tar create failed (%d): %s%s", codeOf(res), stripANSI(outOf(res)), stripANSI(errOf(res)))
	}

	tmpHost, err := os.CreateTemp("/tmp", "anvil-cp-out-*.tar")
	if err != nil {
		return "", err
	}
	defer tmpHost.Close()
	if _, werr := tmpHost.WriteString(res.stdout); werr != nil {
		os.Remove(tmpHost.Name())
		return "", werr
	}
	return tmpHost.Name(), nil
}

// extractTarIntoContainer extracts a host-side tar stream into the container.
// An in-container `tar -xf -` via exec-with-stdin is NOT used: containerd's
// cio stdin fifo does not deliver EOF reliably for one-shot writers, so the
// extract hangs. Running containers get the tar extracted straight into the
// task's live rootfs mount; stopped containers get their rootfs snapshot
// mounted (the buildx docker-container driver stages files this way).
func extractTarIntoContainer(ns, containerdID string, tarStream []byte, dstPath string) error {
	running, _, stateOK := containerTaskState(context.Background(), ns, containerdID)
	if stateOK && running {
		rootfs := filepath.Join("/run/containerd/io.containerd.runtime.v2.task", ns, containerdID, "rootfs")
		if _, err := os.Stat(rootfs); err == nil {
			target := filepath.Join(rootfs, filepath.Clean("/"+dstPath))
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			extract := exec.Command("/bin/tar", "-xf", "-", "-C", target)
			extract.Stdin = strings.NewReader(string(tarStream))
			if out, err := extract.CombinedOutput(); err != nil {
				return fmt.Errorf("tar extract: %v: %s", err, stripANSI(string(out)))
			}
			return nil
		}
		// Fall through to the snapshot mount if the runtime dir is gone.
	}
	return extractTarIntoSnapshot(ns, containerdID, tarStream, dstPath)
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

// extractTarIntoSnapshot mounts a stopped container's rootfs snapshot on the
// guest, extracts the tar stream into dstPath inside it, and unmounts.
func extractTarIntoSnapshot(ns, containerdID string, tarStream []byte, dstPath string) error {
	return withRootfsMount(ns, containerdID, func(root string) error {
		target := filepath.Join(root, filepath.Clean("/"+dstPath))
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		extract := exec.Command("/bin/tar", "-xf", "-", "-C", target)
		extract.Stdin = strings.NewReader(string(tarStream))
		if out, err := extract.CombinedOutput(); err != nil {
			return fmt.Errorf("tar extract: %v: %s", err, stripANSI(string(out)))
		}
		return nil
	})
}

// shellescape escapes a path for use in shell arguments.
func shellescape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// Small helpers keeping the exec result handling terse at call sites above.
func codeOf(r *simpleExecResult) int {
	if r == nil {
		return 126
	}
	return r.exitCode
}
func outOf(r *simpleExecResult) string {
	if r == nil {
		return ""
	}
	return r.stdout
}
func errOf(r *simpleExecResult) string {
	if r == nil {
		return ""
	}
	return r.stderr
}
