package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	bkclient "github.com/moby/buildkit/client"
)

// handleSystemDF implements GET /system/df (docker system df [-v]): image,
// container, volume and build-cache usage with the sizes the CLI shows.
func handleSystemDF(ctx context.Context, w http.ResponseWriter) {
	images, err := listDockerImages(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	containers, _ := listDockerContainers(ctx, nil)
	volumes, _ := listDockerVolumes(ctx, nil)

	// Containers per image, and per volume directory.
	imageRefs := map[string]int{}
	for _, c := range containers {
		imageRefs[c.ImageID]++
		if c.Image != c.ImageID {
			imageRefs[c.Image]++
		}
	}
	volumeRefs := map[string]int{}
	for _, c := range containers {
		for _, m := range c.Mounts {
			if m.Type == "volume" {
				volumeRefs[filepath.Clean(m.Source)]++
			}
		}
	}

	var imgTotal int64
	imagesOut := []map[string]any{}
	for _, img := range images {
		imgTotal += img.Size
		refs := imageRefs[img.Id]
		for _, t := range img.RepoTags {
			refs += imageRefs[t]
		}
		imagesOut = append(imagesOut, map[string]any{
			"Id": img.Id, "RepoTags": img.RepoTags, "RepoDigests": img.RepoDigests,
			"Created": img.Created, "Size": img.Size, "SharedSize": 0, "VirtualSize": img.Size,
			"Labels": img.Labels, "Containers": refs,
		})
	}

	contOut := []map[string]any{}
	for _, c := range containers {
		rw := containerRWSize(ctx, c.Id)
		contOut = append(contOut, map[string]any{
			"Id": c.Id, "Names": c.Names, "Image": c.Image, "ImageID": c.ImageID,
			"Command": c.Command, "Created": c.Created, "Ports": c.Ports, "Labels": c.Labels,
			"State": c.State, "Status": c.Status, "Mounts": c.Mounts,
			"SizeRw": rw, "SizeRootFs": rw + imageSizeOf(images, c.ImageID),
		})
	}

	volOut := []map[string]any{}
	for _, v := range volumes {
		volOut = append(volOut, map[string]any{
			"Name": v.Name, "Driver": "local", "Mountpoint": v.Mountpoint, "Labels": v.Labels,
			"Scope": "local", "Options": v.Options,
			"UsageData": map[string]any{"Size": dirSize(v.Mountpoint), "RefCount": volumeRefs[filepath.Clean(v.Mountpoint)]},
		})
	}

	cache, cacheSize := buildCacheUsage(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"LayersSize":  imgTotal,
		"Images":      imagesOut,
		"Containers":  contOut,
		"Volumes":     volOut,
		"BuildCache":  cache,
		"BuilderSize": cacheSize,
	})
}

func imageSizeOf(images []dockerImageSummary, id string) int64 {
	for _, img := range images {
		if img.Id == id {
			return img.Size
		}
	}
	return 0
}

// containerRWSize is the size of the container's writable layer.
func containerRWSize(ctx context.Context, did string) int64 {
	ns, cid, _, err := resolveDockerID(ctx, did)
	if err != nil {
		return 0
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return 0
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, cid)
	if err != nil {
		return 0
	}
	info, err := c.Info(nsCtx)
	if err != nil || info.SnapshotKey == "" {
		return 0
	}
	u, err := cl.SnapshotService(info.Snapshotter).Usage(nsCtx, info.SnapshotKey)
	if err != nil {
		return 0
	}
	return u.Size
}

// buildCacheUsage lists buildkit's cache records — only when buildkitd is
// already running (it starts lazily; df must not start it).
func buildCacheUsage(ctx context.Context) ([]map[string]any, int64) {
	out := []map[string]any{}
	if _, err := os.Stat(buildkitSocket); err != nil {
		return out, 0
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := bkclient.New(cctx, "unix://"+buildkitSocket)
	if err != nil {
		return out, 0
	}
	defer c.Close()
	usage, err := c.DiskUsage(cctx)
	if err != nil {
		return out, 0
	}
	var total int64
	for _, u := range usage {
		total += u.Size
		rec := map[string]any{
			"ID": u.ID, "Parents": u.Parents, "Type": string(u.RecordType), "Description": u.Description,
			"InUse": u.InUse, "Shared": u.Shared, "Size": u.Size,
			"CreatedAt": u.CreatedAt, "UsageCount": u.UsageCount,
		}
		if u.LastUsedAt != nil {
			rec["LastUsedAt"] = *u.LastUsedAt
		}
		out = append(out, rec)
	}
	return out, total
}
