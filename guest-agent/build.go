package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/archive/compression"
)

// handleBuild implements POST /build (classic Docker build API). The client
// uploads the build context as a tar stream; it is extracted onto the
// persistent disk and built with `nerdctl build` (buildkitd), streaming
// Docker-style JSON progress lines back.
func handleBuild(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dockerfile := q.Get("dockerfile")
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if remote := q.Get("remote"); remote != "" {
		http.Error(w, fmt.Sprintf(`{"message":"remote build contexts are not supported (%s)"}`, remote), http.StatusNotImplemented)
		return
	}

	ctxDir := filepath.Join("/var/lib/anvil-build", fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(ctxDir)

	// Extract the context tar (gzip/zstd are auto-detected).
	ds, err := compression.DecompressStream(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer ds.Close()

	untar := exec.Command("/bin/tar", "-xf", "-", "-C", ctxDir)
	untar.Stdin = ds
	untarOut, err := untar.CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"failed to extract build context: %s: %s"}`, err, stripANSI(string(untarOut))), http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(ctxDir, dockerfile)); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"Cannot locate specified Dockerfile: %s"}`, dockerfile), http.StatusBadRequest)
		return
	}

	args := []string{"--namespace", "default", "build", "-f", dockerfile}
	for _, tag := range strings.Split(q.Get("t"), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			args = append(args, "-t", tag)
		}
	}
	if v := q.Get("buildargs"); v != "" {
		var buildArgs map[string]string
		if json.Unmarshal([]byte(v), &buildArgs) == nil {
			for k, val := range buildArgs {
				args = append(args, "--build-arg", k+"="+val)
			}
		}
	}
	if v := q.Get("labels"); v != "" {
		var labels map[string]string
		if json.Unmarshal([]byte(v), &labels) == nil {
			for k, val := range labels {
				args = append(args, "--label", k+"="+val)
			}
		}
	}
	if q.Get("nocache") == "1" || q.Get("nocache") == "true" {
		args = append(args, "--no-cache")
	}
	if target := q.Get("target"); target != "" {
		args = append(args, "--target", target)
	}
	if platform := q.Get("platform"); platform != "" {
		args = append(args, "--platform", platform)
	}
	args = append(args, ".")
	quiet := q.Get("q") == "1" || q.Get("q") == "true"

	log.Printf("[docker-api] build ctx=%s tags=%q", ctxDir, q.Get("t"))

	// nerdctl build shells out to buildkitd, which is started lazily.
	if err := ensureBuildkitd(); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	cmd := exec.Command("/opt/containerd/bin/nerdctl", args...)
	cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	cmd.Dir = ctxDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeStream := func(line string) {
		if quiet {
			return
		}
		payload, _ := json.Marshal(map[string]string{"stream": line + "\r\n"})
		w.Write(payload)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Forward builder output line by line as Docker progress stream frames.
	lineBuf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, readErr := stdout.Read(tmp)
		for _, b := range tmp[:n] {
			if b == '\n' {
				writeStream(string(lineBuf))
				lineBuf = lineBuf[:0]
			} else if b != '\r' {
				lineBuf = append(lineBuf, b)
			}
		}
		if readErr != nil {
			break
		}
	}
	if len(lineBuf) > 0 {
		writeStream(string(lineBuf))
	}

	if err := cmd.Wait(); err != nil {
		msg := fmt.Sprintf("build failed: %v", err)
		payload, _ := json.Marshal(map[string]interface{}{
			"error":       msg,
			"errorDetail": map[string]string{"message": msg},
		})
		w.Write(payload)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	final, _ := json.Marshal(map[string]string{"stream": "Successfully built\r\n"})
	w.Write(final)
	w.Write([]byte("\n"))
	if flusher != nil {
		flusher.Flush()
	}
}
