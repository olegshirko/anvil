package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	digest "github.com/opencontainers/go-digest"
)

// dockerImageSummary matches the JSON returned by GET /images/json.
type dockerImageSummary struct {
	Id          string            `json:"Id"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	VirtualSize int64             `json:"VirtualSize"`
	Labels      map[string]string `json:"Labels"`
	ParentId    string            `json:"ParentId"`
	Containers  int               `json:"Containers"`
}

// nerdctlImage is the shape of `nerdctl images --format json` output.
type nerdctlImage struct {
	CreatedAt  string `json:"CreatedAt"`
	Digest     string `json:"Digest"`
	ID         string `json:"ID"`
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	Name       string `json:"Name"`
	Size       string `json:"Size"`
	BlobSize   string `json:"BlobSize"`
	Platform   string `json:"Platform"`
}

// parseHumanSize converts nerdctl size strings like "182.2 MiB" or "211.5 MB" to bytes.
func parseHumanSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" || s == "0B" {
		return 0
	}

	// Try to parse a plain number first.
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(v)
	}

	// Find where the numeric part ends and the unit begins.
	numEnd := 0
	for i, r := range s {
		if (r >= '0' && r <= '9') || r == '.' {
			numEnd = i + 1
		} else {
			break
		}
	}
	if numEnd == 0 {
		return 0
	}
	valStr := strings.TrimSpace(s[:numEnd])
	unit := strings.TrimSpace(s[numEnd:])

	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0
	}

	switch {
	case strings.HasPrefix(unit, "KiB"):
		return int64(val * 1024)
	case strings.HasPrefix(unit, "MiB"):
		return int64(val * 1024 * 1024)
	case strings.HasPrefix(unit, "GiB"):
		return int64(val * 1024 * 1024 * 1024)
	case strings.HasPrefix(unit, "TiB"):
		return int64(val * 1024 * 1024 * 1024 * 1024)
	case strings.HasPrefix(unit, "kB"):
		return int64(val * 1000)
	case strings.HasPrefix(unit, "MB"):
		return int64(val * 1000 * 1000)
	case strings.HasPrefix(unit, "GB"):
		return int64(val * 1000 * 1000 * 1000)
	case strings.HasPrefix(unit, "TB"):
		return int64(val * 1000 * 1000 * 1000 * 1000)
	case strings.HasPrefix(unit, "B"):
		return int64(val)
	default:
		return 0
	}
}

// canonicalizeImageRef returns a fully-qualified containerd image reference.
// Unqualified library images (e.g. "postgres:15.5") become
// "docker.io/library/postgres:15.5", and user images (e.g. "foo/bar") become
// "docker.io/foo/bar". A registry domain is present only when the first path
// component looks like a host (contains "." or ":", or is localhost) — a bare
// "name:tag" has no slash and must NOT be mistaken for a registry. When
// neither tag nor digest is present, ":latest" is appended (Docker
// semantics; the check looks at the last path component so a registry port
// is not treated as a tag).
func canonicalizeImageRef(ref string) string {
	if ref == "" {
		return ref
	}
	parts := strings.SplitN(ref, "/", 2)
	result := ref
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		result = ref
	} else if len(parts) == 1 {
		result = "docker.io/library/" + ref
	} else {
		result = "docker.io/" + ref
	}
	last := result
	if i := strings.LastIndex(result, "/"); i != -1 {
		last = result[i+1:]
	}
	if !strings.ContainsAny(last, ":@") {
		result += ":latest"
	}
	return result
}

// ensureImageInNamespace makes sure an image reference exists in the target
// namespace. If the image already exists there, it returns nil. If it exists
// in another namespace, the image is streamed into the target namespace
// (containerd's content store is namespaced, so a bare metadata copy would
// leave a dangling pointer and nerdctl would fall back to a registry pull).
// Otherwise it is pulled into the target namespace.
func ensureImageInNamespace(ref, targetNs string) error {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	canonicalRef := canonicalizeImageRef(ref)

	// Fast path: image already exists in target namespace.
	targetCtx := namespaces.WithNamespace(ctx, targetNs)
	if _, err := cl.GetImage(targetCtx, canonicalRef); err == nil {
		log.Printf("[images] %s already in namespace %s", canonicalRef, targetNs)
		return nil
	}

	// Images imported from OCI archives may be registered under the raw,
	// unnormalized ref.name (e.g. "myapp:1" instead of
	// "docker.io/library/myapp:1"). If the target namespace has the image
	// under that raw name, alias it to the canonical one (same namespace, so
	// the content store already sees the blobs).
	if ref != canonicalRef {
		if img, err := cl.GetImage(targetCtx, ref); err == nil {
			if err := putImage(cl, targetCtx, images.Image{Name: canonicalRef, Target: img.Target(), Labels: img.Labels()}); err != nil {
				return fmt.Errorf("alias image %s to %s: %w", ref, canonicalRef, err)
			}
			log.Printf("[images] aliased %s to %s in namespace %s", ref, canonicalRef, targetNs)
			return nil
		}
	}

	// Find the image in another namespace and stream it into the target one.
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	candidates := []string{canonicalRef}
	if ref != canonicalRef {
		candidates = append(candidates, ref)
	}
	for _, ns := range nss {
		if ns == targetNs {
			continue
		}
		nsCtx := namespaces.WithNamespace(ctx, ns)
		for _, name := range candidates {
			if _, err := cl.GetImage(nsCtx, name); err != nil {
				continue
			}
			if err := copyImageBetweenNamespaces(ns, name, targetNs); err != nil {
				return fmt.Errorf("copy image %s from %s to %s: %w", name, ns, targetNs, err)
			}
			log.Printf("[images] streamed %s from %s to %s", name, ns, targetNs)
			return nil
		}
	}

	// Image not found anywhere; pull it. Use the user-supplied short form so
	// nerdctl resolves aliases the same way Docker does.
	log.Printf("[images] pulling %q (canonical %q) into namespace %s", ref, canonicalRef, targetNs)
	stdout, stderr, code, err := runNerdctl(targetNs, "pull", ref)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl pull failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	log.Printf("[images] pulled %q into namespace %s", ref, targetNs)
	return nil
}

// putImage creates or updates an image record.
func putImage(cl *client.Client, nsCtx context.Context, img images.Image) error {
	if _, err := cl.ImageService().Create(nsCtx, img); err != nil {
		if _, uerr := cl.ImageService().Update(nsCtx, img, "target"); uerr != nil {
			return err
		}
	}
	return nil
}

// copyImageBetweenNamespaces streams an image from one containerd namespace
// to another via `nerdctl save | ctr images import`, so that both the image
// record and the content store entries exist in the target namespace.
// containerd's content store is namespaced: a metadata-only copy would
// reference blobs the target namespace cannot see.
func copyImageBetweenNamespaces(srcNs, ref, targetNs string) error {
	env := append([]string{}, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	save := exec.Command("/opt/containerd/bin/nerdctl", "--namespace", srcNs, "save", ref)
	save.Env = env
	imp := exec.Command("/opt/containerd/bin/ctr", "-n", targetNs, "images", "import", "--no-unpack", "-")
	imp.Env = env
	pipe, err := save.StdoutPipe()
	if err != nil {
		return err
	}
	imp.Stdin = pipe
	var saveErr, impErr strings.Builder
	save.Stderr = &saveErr
	imp.Stderr = &impErr
	if err := imp.Start(); err != nil {
		return err
	}
	if err := save.Run(); err != nil {
		_ = imp.Process.Kill()
		_ = imp.Wait()
		return fmt.Errorf("save: %v: %s", err, saveErr.String())
	}
	if err := imp.Wait(); err != nil {
		return fmt.Errorf("import: %v: %s", err, impErr.String())
	}
	return nil
}

// listDockerImages collects images from all namespaces and returns a
// Docker-compatible summary, deduplicating by image ID.
func listDockerImages() ([]dockerImageSummary, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	seen := make(map[string]struct{})
	result := make([]dockerImageSummary, 0)

	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "images", "--format", "json")
		if err != nil || code != 0 {
			log.Printf("[docker-api] list images in %s: %s%s", ns, stdout, stderr)
			continue
		}
		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var img nerdctlImage
			if err := json.Unmarshal([]byte(line), &img); err != nil {
				log.Printf("[docker-api] parse image line %q: %v", line, err)
				continue
			}
			if img.ID == "" {
				continue
			}
			if _, ok := seen[img.ID]; ok {
				continue
			}
			seen[img.ID] = struct{}{}

			created := int64(0)
			if t, err := time.Parse("2006-01-02 15:04:05 +0000 UTC", img.CreatedAt); err == nil {
				created = t.Unix()
			}

			size := parseHumanSize(img.Size)
			if size == 0 {
				size = parseHumanSize(img.BlobSize)
			}

			repoTag := ""
			if img.Repository != "" && img.Tag != "" {
				repoTag = img.Repository + ":" + img.Tag
			} else if img.Name != "" {
				repoTag = img.Name
			}

			repoDigest := ""
			if img.Digest != "" && repoTag != "" {
				// Docker digest format: repo@digest
				repo := repoTag
				if idx := strings.Index(repoTag, ":"); idx != -1 {
					repo = repoTag[:idx]
				}
				repoDigest = repo + "@" + img.Digest
			}

			dockerID := img.ID
			if !strings.HasPrefix(dockerID, "sha256:") {
				dockerID = "sha256:" + dockerID
			}

			var repoTags []string
			if repoTag != "" {
				repoTags = []string{repoTag}
			}
			var repoDigests []string
			if repoDigest != "" {
				repoDigests = []string{repoDigest}
			}

			result = append(result, dockerImageSummary{
				Id:          dockerID,
				RepoTags:    repoTags,
				RepoDigests: repoDigests,
				Created:     created,
				Size:        size,
				VirtualSize: size,
				Labels:      map[string]string{},
				ParentId:    "",
				Containers:  -1,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Id < result[j].Id
	})
	return result, nil
}

// findImageNamespace returns the containerd namespace that contains an image
// matching the given reference (name or name:tag). The empty string is returned
// if no matching image is found.
func findImageNamespace(ref string) string {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return ""
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		log.Printf("[images] list namespaces: %v", err)
		return ""
	}
	log.Printf("[images] findImageNamespace ref=%q namespaces=%v", ref, nss)

	// Normalize ref: strip tag if present.
	repo := ref
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		repo = ref[:idx]
	}

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		images, err := cl.ListImages(nsCtx)
		if err != nil {
			debugLog("list images in %s: %v", ns, err)
			continue
		}
		for _, img := range images {
			name := img.Name()
			if name == ref || name == repo {
				return ns
			}
			// Also match short names like "nginx" against "docker.io/library/nginx".
			short := name
			if strings.HasPrefix(name, "docker.io/library/") {
				short = strings.TrimPrefix(name, "docker.io/library/")
			} else if strings.HasPrefix(name, "docker.io/") {
				short = strings.TrimPrefix(name, "docker.io/")
			}
			if short == ref || short == repo {
				return ns
			}
		}
	}
	debugLog("findImageNamespace: no namespace for %s", ref)
	return ""
}

// tagDockerImage tags an image using nerdctl.
func tagDockerImage(source, target string) error {
	ns := findImageNamespace(source)
	if ns == "" {
		ns = "default"
	}
	stdout, stderr, code, err := runNerdctl(ns, "tag", source, target)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl tag failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	return nil
}

// handleImageGet implements GET /images/{name}/get (docker save) — streams a
// Docker-format tar of the image into the response.
func handleImageGet(w http.ResponseWriter, r *http.Request, name string) {
	streamImageSave(w, []string{name})
}

// handleImagesGet implements GET /images/get?names=a&names=b — same, for
// multiple images in one archive.
func handleImagesGet(w http.ResponseWriter, r *http.Request) {
	names := r.URL.Query()["names"]
	if len(names) == 0 {
		http.Error(w, `{"message":"no images specified"}`, http.StatusBadRequest)
		return
	}
	streamImageSave(w, names)
}

func streamImageSave(w http.ResponseWriter, names []string) {
	// Verify all images exist first so a missing one is a clean 404 instead
	// of a failed stream mid-response.
	ns := "default"
	for _, name := range names {
		imgNs := findImageNamespace(name)
		if imgNs == "" {
			http.Error(w, fmt.Sprintf(`{"message":"No such image: %s"}`, name), http.StatusNotFound)
			return
		}
		ns = imgNs
	}

	log.Printf("[docker-api] saving images %v from ns=%q", names, ns)
	w.Header().Set("Content-Type", "application/x-tar")
	args := append([]string{"--namespace", ns, "save"}, names...)
	cmd := exec.Command("/opt/containerd/bin/nerdctl", args...)
	cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	cmd.Stdout = w
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		// Headers are already sent; only log.
		log.Printf("[docker-api] save %v failed: %v: %s", names, err, stripANSI(errBuf.String()))
	}
}

// removeDockerImage removes an image using nerdctl.
func removeDockerImage(name string) error {
	ns := findImageNamespace(name)
	if ns == "" {
		ns = "default"
	}
	stdout, stderr, code, err := runNerdctl(ns, "rmi", name)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl rmi failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	return nil
}

// pruneDockerImages removes images and returns Docker's /images/prune
// response shape. With dangling=true only untagged images go; with
// dangling=false (`docker system prune -a`) every image not referenced by a
// container goes.
func pruneDockerImages(dangling bool) ([]map[string]string, int64, error) {
	images, err := listDockerImages()
	if err != nil {
		return nil, 0, err
	}

	used := map[string]bool{}
	if containers, err := listDockerContainers(nil); err == nil {
		for _, c := range containers {
			if c.Image != "" {
				used[c.Image] = true
			}
			if c.ImageID != "" {
				used[c.ImageID] = true
			}
		}
	}

	deleted := []map[string]string{}
	var reclaimed int64
	for _, img := range images {
		tag := ""
		if len(img.RepoTags) > 0 {
			tag = img.RepoTags[0]
		}
		isDangling := tag == "" || strings.HasPrefix(tag, "<none>")
		if dangling && !isDangling {
			continue
		}
		if !dangling && used[tag] {
			continue
		}
		ref := tag
		if isDangling {
			ref = img.Id
		}
		if err := removeDockerImage(ref); err != nil {
			log.Printf("[docker-api] prune image %s: %v", ref, err)
			continue
		}
		deleted = append(deleted, map[string]string{"Deleted": img.Id})
		reclaimed += img.Size
	}
	return deleted, reclaimed, nil
}

// inspectDockerImage returns a Docker-compatible image inspect payload,
// searching all namespaces for the image.
func inspectDockerImage(name string) (map[string]interface{}, error) {
	ns := findImageNamespace(name)
	if ns == "" {
		ns = "default"
	}
	stdout, stderr, code, err := runNerdctl(ns, "image", "inspect", "--format", "json", name)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("nerdctl image inspect failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var info struct {
			ID          string   `json:"ID"`
			RepoTags    []string `json:"RepoTags"`
			RepoDigests []string `json:"RepoDigests"`
			Comment     string   `json:"Comment"`
			Created     string   `json:"Created"`
			Author      string   `json:"Author"`
			Config      struct {
				Cmd          []string            `json:"Cmd"`
				Entrypoint   []string            `json:"Entrypoint"`
				Env          []string            `json:"Env"`
				ExposedPorts map[string]struct{} `json:"ExposedPorts"`
				Labels       map[string]string   `json:"Labels"`
				WorkingDir   string              `json:"WorkingDir"`
				User         string              `json:"User"`
				StopSignal   string              `json:"StopSignal"`
			} `json:"Config"`
			RootFS struct {
				Type   string   `json:"Type"`
				Layers []string `json:"Layers"`
			} `json:"RootFS"`
		}
		if err := json.Unmarshal([]byte(line), &info); err != nil {
			continue
		}
		if info.ID == "" {
			continue
		}
		return map[string]interface{}{
			"Id":          info.ID,
			"RepoTags":    info.RepoTags,
			"RepoDigests": info.RepoDigests,
			"Comment":     info.Comment,
			"Created":     info.Created,
			"Author":      info.Author,
			"Config":      info.Config,
			"RootFS":      info.RootFS,
			"Size":        0,
			"VirtualSize": 0,
			"GraphDriver": map[string]interface{}{"Data": map[string]interface{}{}, "Name": "overlayfs"},
		}, nil
	}
	return nil, fmt.Errorf("No such image: %s", name)
}

// pushDockerImage pushes an image and streams progress lines to w.
func pushDockerImage(name string, w io.Writer) error {
	ns := findImageNamespace(name)
	if ns == "" {
		ns = "default"
	}
	cmd := exec.Command("/opt/containerd/bin/nerdctl", "-n", ns, "push", name)
	cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()

	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return nil
}

// pullDockerImage pulls an image via nerdctl into the default namespace.
func pullDockerImage(image string) (string, string, error) {
	ns := "default"
	stdout, stderr, code, err := runNerdctl(ns, "pull", image)
	if err != nil || code != 0 {
		return stdout, stderr, fmt.Errorf("nerdctl pull failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	return stdout, stderr, nil
}

// handleImageLoad implements POST /images/load — streams a Docker/OCI tar
// archive from the request body straight into containerd via the Go client.
// No temp file is used: the guest root is a small RAM tmpfs, so buffering
// large archives on disk can fill it up. gzip/zstd streams are auto-detected.
func handleImageLoad(w http.ResponseWriter, r *http.Request) {
	ns := "default"
	if r.URL.Query().Get("namespace") != "" {
		ns = r.URL.Query().Get("namespace")
	}

	cl, err := client.New(containerdSocket)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer cl.Close()

	log.Printf("[docker-api] loading image stream (%d bytes) into ns=%q", r.ContentLength, ns)

	ds, err := compression.DecompressStream(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer ds.Close()

	// Archives without name annotations (e.g. `buildx --output type=oci`
	// without -t) import content only and register no image reference, so
	// `docker run <name>` would fall back to a registry pull. Give unnamed
	// manifests a digest-based ref so they can be run/tagged locally.
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	imgs, err := cl.Import(nsCtx, ds,
		client.WithDigestRef(func(d digest.Digest) string {
			return "docker.io/imported/anvil-image:" + d.Encoded()[:12]
		}),
		client.WithSkipDigestRef(func(name string) bool { return name != "" }),
		// Archives exported for a single platform may still reference
		// manifests/blobs of other platforms (e.g. a multi-arch index) that
		// are not included. Skip those instead of failing the whole import;
		// ctr's transfer-based import is lenient the same way.
		client.WithSkipMissing(),
	)
	if err != nil {
		log.Printf("[docker-api] import failed: %v", err)
		http.Error(w, fmt.Sprintf(`{"message":"import failed: %s"}`, stripANSI(err.Error())), http.StatusInternalServerError)
		return
	}

	// Import into the content store without unpacking (avoids whiteout
	// conversion errors with the native snapshotter). Unpack best-effort so
	// images are immediately usable; otherwise the first container start
	// unpacks via containerd's snapshotter.
	for _, img := range imgs {
		_ = client.NewImage(cl, img).Unpack(nsCtx, "native")
	}

	// Docker CLI prints every "status" line from the response stream. Report
	// the exact refs that were registered so the user knows which name to run.
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if len(imgs) == 0 {
		enc.Encode(map[string]interface{}{"status": "Loaded image"})
		return
	}
	for _, img := range imgs {
		enc.Encode(map[string]interface{}{"status": fmt.Sprintf("Loaded image: %s", img.Name)})
	}
}
