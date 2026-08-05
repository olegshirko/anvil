package main

import (
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
	ns, containerdID, _, err := resolveDockerID(id)
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
	// Docker CLI sends a tar stream. Save it locally, copy into the container,
	// then extract with tar.
	tmpHost, err := os.CreateTemp("/tmp", "anvil-cp-in-*.tar")
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpHost.Name())
	defer tmpHost.Close()

	if _, err := io.Copy(tmpHost, r.Body); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if err := tmpHost.Close(); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	if err := extractTarIntoContainer(ns, containerdID, tmpHost.Name(), dstPath); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// statContainerPath runs stat inside the container and returns a Docker-compatible stat.
func statContainerPath(ns, containerdID, path string) (dockerPathStat, error) {
	script := fmt.Sprintf("stat -c '%%n|%%s|%%a|%%Y|%%N' %s", shellescape(path))
	stdout, stderr, code, err := runNerdctl(ns, "exec", "--user", "0", containerdID, "sh", "-c", script)
	if err != nil || code != 0 {
		return dockerPathStat{}, fmt.Errorf("stat failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	fields := strings.SplitN(strings.TrimSpace(stdout), "|", 5)
	if len(fields) < 4 {
		return dockerPathStat{}, fmt.Errorf("unexpected stat output: %q", stdout)
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

// createContainerTar archives the given path inside the container to a temporary host file.
func createContainerTar(ns, containerdID, srcPath string) (string, error) {
	tmpGuest := "/tmp/anvil-cp-out-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".tar"
	base := filepath.Base(srcPath)
	dir := filepath.Dir(srcPath)
	script := fmt.Sprintf("tar -cf %s -C %s %s", shellescape(tmpGuest), shellescape(dir), shellescape(base))
	stdout, stderr, code, err := runNerdctl(ns, "exec", "--user", "0", containerdID, "sh", "-c", script)
	if err != nil || code != 0 {
		return "", fmt.Errorf("tar create failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}

	tmpHost, err := os.CreateTemp("/tmp", "anvil-cp-out-*.tar")
	if err != nil {
		return "", err
	}
	tmpHost.Close()

	stdout, stderr, code, err = runNerdctl(ns, "cp", containerdID+":"+tmpGuest, tmpHost.Name())
	if err != nil || code != 0 {
		os.Remove(tmpHost.Name())
		return "", fmt.Errorf("nerdctl cp failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	// Best-effort cleanup inside the container.
	runNerdctl(ns, "exec", "--user", "0", containerdID, "rm", "-f", tmpGuest)
	return tmpHost.Name(), nil
}

// extractTarIntoContainer copies the host tar into the container and extracts it.
func extractTarIntoContainer(ns, containerdID, hostTar, dstPath string) error {
	if !isNerdctlContainerRunning(ns, containerdID) {
		// nerdctl exec needs a running task, while nerdctl cp also works on
		// stopped/created containers: extract the tar on the guest and copy
		// the payload into the container rootfs. The buildx docker-container
		// driver relies on this when it stages files into its buildkit
		// container before starting it.
		tmpDir := "/tmp/anvil-cp-in-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			return err
		}
		defer os.RemoveAll(tmpDir)
		if out, err := exec.Command("/bin/tar", "-xf", hostTar, "-C", tmpDir).CombinedOutput(); err != nil {
			return fmt.Errorf("guest tar extract failed: %s", stripANSI(string(out)))
		}
		stdout, stderr, code, err := runNerdctl(ns, "cp", tmpDir+"/.", containerdID+":"+dstPath)
		if err != nil || code != 0 {
			return fmt.Errorf("nerdctl cp failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
		}
		return nil
	}

	tmpGuest := "/tmp/anvil-cp-in-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".tar"
	stdout, stderr, code, err := runNerdctl(ns, "cp", hostTar, containerdID+":"+tmpGuest)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl cp failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}

	script := fmt.Sprintf("mkdir -p %s && tar -xf %s -C %s", shellescape(dstPath), shellescape(tmpGuest), shellescape(dstPath))
	stdout, stderr, code, err = runNerdctl(ns, "exec", "--user", "0", containerdID, "sh", "-c", script)
	if err != nil || code != 0 {
		return fmt.Errorf("tar extract failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	runNerdctl(ns, "exec", "--user", "0", containerdID, "rm", "-f", tmpGuest)
	return nil
}

// shellescape escapes a path for use in shell arguments.
func shellescape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
