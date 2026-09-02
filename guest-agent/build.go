package main

import (
	"context"
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
	"github.com/docker/cli/cli/config/configfile"
	configtypes "github.com/docker/cli/cli/config/types"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
)

// handleBuild implements POST /build (classic Docker build API). The client
// uploads the build context as a tar stream; it is extracted onto the
// persistent disk and built through buildkitd via its gRPC API, streaming
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

	var tags []string
	for _, tag := range strings.Split(q.Get("t"), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	frontendAttrs := map[string]string{
		"filename": dockerfile,
	}
	for k, v := range parseKVParam(q.Get("buildargs")) {
		frontendAttrs["build-arg:"+k] = v
	}
	for k, v := range parseKVParam(q.Get("labels")) {
		frontendAttrs["label:"+k] = v
	}
	if q.Get("nocache") == "1" || q.Get("nocache") == "true" {
		frontendAttrs["no-cache"] = ""
	}
	if target := q.Get("target"); target != "" {
		frontendAttrs["target"] = target
	}
	if platform := q.Get("platform"); platform != "" {
		frontendAttrs["platform"] = platform
	}
	quiet := q.Get("q") == "1" || q.Get("q") == "true"

	// Private registries: attach the request's X-Registry-Auth credentials to
	// the buildkit session so FROM pulls (and --push exports in the future)
	// authenticate like the CLI's own driver does.
	var sessionAttachables []session.Attachable
	if a := parseRegistryAuth(r); !a.empty() {
		sessionAttachables = append(sessionAttachables, buildAuthAttachable(a))
	}

	log.Printf("[docker-api] build ctx=%s tags=%q", ctxDir, tags)

	// buildkitd is started lazily on first use.
	if err := ensureBuildkitd(); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeStream := func(line string) {
		if quiet || line == "" {
			return
		}
		payload, _ := json.Marshal(map[string]string{"stream": line + "\r\n"})
		w.Write(payload)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	runBuild := func() (berr error, digestMissing bool) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		c, cerr := bkclient.New(ctx, "unix://"+buildkitSocket)
		if cerr != nil {
			return cerr, false
		}
		defer c.Close()

		exports := []bkclient.ExportEntry{}
		if len(tags) > 0 {
			// type=image with the containerd worker writes straight into the
			// containerd image store (namespace "default"), so `FROM` sees
			// locally built images and no separate import step is needed.
			exports = append(exports, bkclient.ExportEntry{
				Type: bkclient.ExporterImage,
				Attrs: map[string]string{
					"name": strings.Join(tags, ","),
				},
			})
		}

		statusCh := make(chan *bkclient.SolveStatus)
		done := make(chan error, 1)
		go func() {
			var lastVertex string
			for st := range statusCh {
				for _, v := range st.Vertexes {
					if v.Name != "" && v.Name != lastVertex {
						lastVertex = v.Name
						writeStream("[+] " + v.Name)
					}
					if v.Error != "" {
						writeStream("# " + v.Name + " ERROR: " + v.Error)
					}
				}
				for _, l := range st.Logs {
					writeStream(strings.TrimRight(string(l.Data), "\n"))
				}
			}
			done <- nil
		}()

		solveOpts := bkclient.SolveOpt{
			Frontend:      "dockerfile.v0",
			FrontendAttrs: frontendAttrs,
			LocalDirs: map[string]string{
				"context":    ctxDir,
				"dockerfile": filepath.Dir(filepath.Join(ctxDir, dockerfile)),
			},
			Exports: exports,
			Session: sessionAttachables,
		}
		_, berr = c.Solve(ctx, nil, solveOpts, statusCh)
		<-done

		// Stale buildkit cache records referencing blobs removed by
		// `docker rmi` fail with a missing-digest error; the caller prunes
		// and retries once.
		if berr != nil && (strings.Contains(berr.Error(), "not found") || strings.Contains(berr.Error(), "does not exist")) {
			digestMissing = true
		}
		return berr, digestMissing
	}

	buildErr, digestMissing := runBuild()
	if buildErr != nil && digestMissing {
		writeStream("[anvil] stale buildkit cache detected, pruning and retrying")
		pruneBuildkitCache(context.Background())
		buildErr, _ = runBuild()
	}

	if buildErr != nil {
		msg := fmt.Sprintf("build failed: %v", buildErr)
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

// buildAuthAttachable wraps request credentials in buildkit's auth session
// provider. The same AuthConfig is registered under every key form the
// authprovider may look up (raw server address, bare host, Docker Hub v1
// canonical URL) so host normalization never misses the entry.
func buildAuthAttachable(a *registryAuth) session.Attachable {
	ac := configtypes.AuthConfig{
		Username:      a.Username,
		Password:      a.Password,
		IdentityToken: a.IdentityToken,
	}
	cf := configfile.New("")
	host := registryHostOf(a.ServerAddress)
	keys := map[string]bool{a.ServerAddress: true, host: true}
	if a.ServerAddress == "" || host == "docker.io" {
		keys["https://index.docker.io/v1/"] = true
		keys["index.docker.io"] = true
		keys["registry-1.docker.io"] = true
		keys["docker.io"] = true
	}
	for k := range keys {
		if k != "" {
			cf.AuthConfigs[k] = ac
		}
	}
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{ConfigFile: cf})
}

// parseKVParam decodes a JSON object of string→string parameters sent by the
// docker CLI in query strings (buildargs, labels).
func parseKVParam(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" {
		return out
	}
	json.Unmarshal([]byte(raw), &out) //nolint:errcheck — best effort
	return out
}

// pruneBuildkitCache clears all build records via the buildkit API.
func pruneBuildkitCache(ctx context.Context) {
	c, err := bkclient.New(ctx, "unix://"+buildkitSocket)
	if err != nil {
		log.Printf("[docker-api] buildkit prune connect: %v", err)
		return
	}
	defer c.Close()
	ch := make(chan bkclient.UsageInfo)
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	err = c.Prune(ctx, ch, bkclient.PruneAll)
	<-done
	if err != nil {
		log.Printf("[docker-api] buildkit prune: %v", err)
	}
}
