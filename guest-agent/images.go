package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
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
// "docker.io/foo/bar".
func canonicalizeImageRef(ref string) string {
	if ref == "" {
		return ref
	}
	// Already contains a registry/domain?
	parts := strings.SplitN(ref, "/", 2)
	if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") {
		return ref
	}
	if len(parts) == 1 {
		return "docker.io/library/" + ref
	}
	return "docker.io/" + ref
}

// ensureImageInNamespace makes sure an image reference exists in the target
// namespace. If the image already exists there, it returns nil. If it exists in
// another namespace, its metadata is copied to the target namespace using the
// shared content store. Otherwise it is pulled into the target namespace.
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

	// Find the image in another namespace and copy its metadata.
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	for _, ns := range nss {
		if ns == targetNs {
			continue
		}
		nsCtx := namespaces.WithNamespace(ctx, ns)
		img, err := cl.GetImage(nsCtx, canonicalRef)
		if err != nil {
			continue
		}
		newImg := client.NewImage(cl, images.Image{
			Name:   canonicalRef,
			Target: img.Target(),
			Labels: img.Labels(),
		})
		if _, err := cl.ImageService().Create(targetCtx, newImg.Metadata()); err != nil {
			return fmt.Errorf("copy image metadata to %s: %w", targetNs, err)
		}
		log.Printf("[images] copied %s from %s to %s", canonicalRef, ns, targetNs)
		return nil
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
