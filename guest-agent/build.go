package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/config/configfile"
	configtypes "github.com/docker/cli/cli/config/types"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/tonistiigi/fsutil"
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
		writeJSONError(w, http.StatusNotImplemented, fmt.Sprintf("remote build contexts are not supported (%s)", remote))
		return
	}

	ctxDir := filepath.Join("/var/lib/anvil-build", fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(ctxDir)

	// Extract the context tar (gzip/zstd are auto-detected).
	ds, err := compression.DecompressStream(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer ds.Close()

	// Go tar inside a chroot of the context dir: symlinks in the uploaded
	// context resolve within it (same mechanism as docker cp).
	if err := inChroot(ctxDir, func() error { return extractTar(ds, "/") }); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("failed to extract build context: %v", err))
		return
	}
	if _, err := os.Stat(filepath.Join(ctxDir, dockerfile)); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Cannot locate specified Dockerfile: %s", dockerfile))
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
	if queryBool(q, "pull") {
		frontendAttrs["image-resolve-mode"] = "pull" // docker build --pull
	}
	switch mode := q.Get("networkmode"); mode {
	case "host", "none":
		frontendAttrs["force-network-mode"] = mode
	}
	if hosts := buildExtraHosts(q.Get("extrahosts")); hosts != "" {
		frontendAttrs["add-hosts"] = hosts
	}
	if shm := q.Get("shmsize"); shm != "" && shm != "0" {
		frontendAttrs["shm-size"] = shm
	}
	var cacheImports []bkclient.CacheOptionsEntry
	var cacheFrom []string
	if json.Unmarshal([]byte(q.Get("cachefrom")), &cacheFrom) == nil {
		for _, ref := range cacheFrom {
			cacheImports = append(cacheImports, bkclient.CacheOptionsEntry{
				Type: "registry", Attrs: map[string]string{"ref": ref}})
		}
	}
	quiet := queryBool(q, "q")

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
		writeJSONError(w, http.StatusInternalServerError, err.Error())
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

	var builtDigest string
	// Unique per build: concurrent untagged builds must not take each
	// other's record.
	untaggedName := fmt.Sprintf("docker.io/anvil/untagged-build:%d", time.Now().UnixNano())
	runBuild := func() (berr error, digestMissing bool) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		c, cerr := bkclient.New(ctx, "unix://"+buildkitSocket)
		if cerr != nil {
			return cerr, false
		}
		defer c.Close()

		// type=image with the containerd worker writes straight into the
		// containerd image store (namespace "default"), so `FROM` sees
		// locally built images and no separate import step is needed. An
		// untagged build is exported under a temporary name and renamed to
		// its digest-only (<none>) record afterwards: Docker keeps untagged
		// builds as dangling images, and clients use the returned ID.
		exportNames := tags
		if len(exportNames) == 0 {
			exportNames = []string{untaggedName}
		}
		exports := []bkclient.ExportEntry{{
			Type:  bkclient.ExporterImage,
			Attrs: map[string]string{"name": strings.Join(exportNames, ",")},
		}}

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

		ctxFS, ferr := fsutil.NewFS(ctxDir)
		if ferr != nil {
			return ferr, false
		}
		dfFS, ferr := fsutil.NewFS(filepath.Dir(filepath.Join(ctxDir, dockerfile)))
		if ferr != nil {
			return ferr, false
		}
		solveOpts := bkclient.SolveOpt{
			Frontend:      "dockerfile.v0",
			FrontendAttrs: frontendAttrs,
			LocalMounts: map[string]fsutil.FS{
				"context":    ctxFS,
				"dockerfile": dfFS,
			},
			Exports:      exports,
			CacheImports: cacheImports,
			Session:      sessionAttachables,
		}
		resp, serr := c.Solve(ctx, nil, solveOpts, statusCh)
		berr = serr
		<-done
		if berr == nil && resp != nil {
			builtDigest = resp.ExporterResponse["containerimage.digest"]
		}

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

	if len(tags) == 0 && builtDigest != "" {
		renameUntaggedBuild(untaggedName, builtDigest)
	}
	// docker-py and docker-java read the image ID from the aux message or
	// the "Successfully built <id>" line; without it their builds failed.
	writeJSONLine := func(v interface{}) {
		payload, _ := json.Marshal(v)
		w.Write(payload)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	if builtDigest != "" {
		writeJSONLine(map[string]interface{}{"aux": map[string]string{"ID": builtDigest}})
		short := strings.TrimPrefix(builtDigest, "sha256:")
		if len(short) > 12 {
			short = short[:12]
		}
		writeJSONLine(map[string]string{"stream": "Successfully built " + short + "\n"})
		for _, t := range tags {
			writeJSONLine(map[string]string{"stream": "Successfully tagged " + t + "\n"})
		}
		return
	}
	writeJSONLine(map[string]string{"stream": "Successfully built\r\n"})
}

// renameUntaggedBuild turns the temporary record of an untagged build into
// the digest-only <none> record (a dangling image, as Docker keeps it).
func renameUntaggedBuild(tmpName, dgst string) {
	ctx := namespaces.WithNamespace(context.Background(), "default")
	cl, err := pc.get(ctx)
	if err != nil {
		return
	}
	is := cl.ImageService()
	tmp, err := is.Get(ctx, tmpName)
	if err != nil {
		return
	}
	defer is.Delete(ctx, tmpName) //nolint:errcheck
	if tmp.Target.Digest.String() != dgst {
		return // not this build's image
	}
	rec := tmp
	rec.Name = "<none>@" + dgst
	if _, err := is.Create(ctx, rec); err != nil && !errdefs.IsAlreadyExists(err) {
		log.Printf("[build] record untagged build %s: %v", dgst, err)
	}
}

// buildExtraHosts converts the API's extrahosts ("name:ip,…", host-gateway
// allowed) to buildkit's add-hosts ("name=ip,…").
func buildExtraHosts(raw string) string {
	var out []string
	for _, h := range strings.Split(raw, ",") {
		h = strings.TrimSpace(h)
		name, ip, ok := strings.Cut(h, ":")
		if !ok || name == "" {
			continue
		}
		if ip == "host-gateway" {
			ip = desktopHostIP()
		}
		out = append(out, name+"="+ip)
	}
	return strings.Join(out, ",")
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
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{AuthConfigProvider: authprovider.LoadAuthConfig(cf)})
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
