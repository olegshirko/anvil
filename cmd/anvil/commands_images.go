package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"anvil/internal/domain"
	"anvil/internal/environment/host"
	"anvil/internal/usecase"
	"anvil/internal/util/downloader"

	log "github.com/sirupsen/logrus"
)

type imagesCmd struct {
	List    imagesListCmd    `cmd:"" help:"List available images from docker-mirror"`
	Check   imagesCheckCmd   `cmd:"" help:"Check if a specific image is available"`
	Request imagesRequestCmd `cmd:"" help:"Request a missing image"`
	Load    imagesLoadCmd    `cmd:"" help:"Load a mirrored Docker image into the local Docker daemon"`
}

type imagesListCmd struct {
	VMBaseImages bool `help:"List VM base images (qcow2)"`
	DockerImages bool `help:"List mirrored Docker images"`
}

func (c *imagesListCmd) Run(g *Globals) error {
	g.resolveProfile("")
	app := g.app
	ctx := context.Background()

	if !c.VMBaseImages && !c.DockerImages {
		// Default: show both
		c.VMBaseImages = true
		c.DockerImages = true
	}

	if c.VMBaseImages {
		vmImages, err := app.ImageDiscovery().ListVMBaseImages(ctx, "master")
		if err != nil {
			return fmt.Errorf("failed to list VM base images: %w", err)
		}
		fmt.Println("VM base images:")
		for _, img := range vmImages {
			fmt.Printf("  %-6s %-10s  %s (%s)\n", img.Arch, img.Runtime, img.URL, formatSize(img.Size))
		}
	}

	if c.DockerImages {
		dockerImages, err := app.ImageDiscovery().ListDockerMirrorImages(ctx)
		if err != nil {
			return fmt.Errorf("failed to list Docker images: %w", err)
		}
		if c.VMBaseImages && len(dockerImages) > 0 {
			fmt.Println()
		}
		fmt.Println("Mirrored Docker images:")
		for _, img := range dockerImages {
			fmt.Printf("  %-40s %-6s  %s (%s)\n", img.Image, img.Arch, img.Release, formatSize(img.Size))
		}
	}

	return nil
}

type imagesCheckCmd struct {
	Runtime string `short:"r" default:"" help:"Container runtime (none, docker, containerd, incus)"`
	Arch    string `short:"a" default:"" help:"Architecture (amd64, arm64)"`
	Tag     string `default:"master" help:"Release tag for VM base images"`
	Docker  string `help:"Check a Docker image (e.g. postgres:13.8)"`
	JSON    bool   `help:"Output as JSON"`
}

func (c *imagesCheckCmd) Run(g *Globals) error {
	g.resolveProfile("")
	app := g.app
	ctx := context.Background()

	if c.Runtime == "" && c.Arch == "" && c.Docker == "" {
		// Default: check current profile's VM base image
		conf, err := app.LoadConfig(ctx)
		if err == nil {
			c.Runtime = conf.Runtime
		}
		if c.Arch == "" {
			c.Arch = runtime.GOARCH
		}
	}

	if c.Runtime == "" {
		c.Runtime = "docker"
	}

	archValue := domain.Arch(c.Arch).Value()
	goArch := archValue.GoArch()

	conf, _ := app.LoadConfig(ctx)
	version := conf.ImageVersion
	if version == "" {
		version = "24.04"
	}

	var results []checkResult

	if c.Docker != "" {
		image, tag, err := parseDockerImage(c.Docker)
		if err != nil {
			return err
		}
		// Check Docker Hub
		hubAvailable, _ := app.ImageDiscovery().CheckContainerImage(ctx, image, tag, goArch)
		hubStatus := "✗ not found"
		if hubAvailable {
			hubStatus = "✓ available"
		}
		// Check docker-mirror releases
		mirrorImages, _ := app.ImageDiscovery().ListDockerMirrorImages(ctx)
		mirrorStatus := "✗ not mirrored"
		for _, mi := range mirrorImages {
			if strings.Contains(mi.Image, image) && strings.Contains(mi.Image, tag) && mi.Arch == goArch {
				mirrorStatus = "✓ mirrored"
				break
			}
		}
		results = append(results, checkResult{
			Type:   "docker-hub",
			Name:   fmt.Sprintf("%s:%s", image, tag),
			Arch:   goArch,
			Status: hubStatus,
		})
		results = append(results, checkResult{
			Type:   "docker-mirror",
			Name:   fmt.Sprintf("%s:%s", image, tag),
			Arch:   goArch,
			Status: mirrorStatus,
		})
	} else {
		images, err := app.ImageDiscovery().ListVMBaseImages(ctx, c.Tag)
		if err != nil {
			return fmt.Errorf("failed to list images: %w", err)
		}

		found := false
		for _, img := range images {
			if img.Arch == goArch && img.Runtime == c.Runtime {
				found = true
				results = append(results, checkResult{
					Type:   "vm-base",
					Name:   fmt.Sprintf("ubuntu-%s-minimal-cloudimg-%s-%s.qcow2", version, goArch, c.Runtime),
					Arch:   goArch,
					Status: "✓ available",
					URL:    img.URL,
				})
				break
			}
		}
		if !found {
			results = append(results, checkResult{
				Type:   "vm-base",
				Name:   fmt.Sprintf("ubuntu-%s-minimal-cloudimg-%s-%s.qcow2", version, goArch, c.Runtime),
				Arch:   goArch,
				Status: "✗ not found",
			})
		}
	}

	if c.JSON {
		return json.NewEncoder(os.Stdout).Encode(results)
	}

	for _, r := range results {
		fmt.Printf("%s: %s (%s)\n  Status: %s\n", r.Type, r.Name, r.Arch, r.Status)
		if r.URL != "" {
			fmt.Printf("  URL: %s\n", r.URL)
		}
	}
	return nil
}

type checkResult struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Arch   string `json:"arch"`
	Status string `json:"status"`
	URL    string `json:"url,omitempty"`
}

type imagesRequestCmd struct {
	Runtime string `short:"r" default:"" help:"Container runtime (for VM base image)"`
	Arch    string `short:"a" default:"" help:"Architecture (amd64, arm64)"`
	Tag     string `default:"master" help:"Release tag to build (for VM base)"`
	Wait    bool   `short:"w" help:"Poll until the image appears in releases"`
	Docker  string `help:"Request a Docker image (e.g. postgres:13.8)"`
}

func (c *imagesRequestCmd) Run(g *Globals) error {
	g.resolveProfile("")
	app := g.app
	ctx := context.Background()

	if c.Runtime == "" && c.Arch == "" && c.Docker == "" {
		conf, err := app.LoadConfig(ctx)
		if err == nil {
			if c.Runtime == "" {
				c.Runtime = conf.Runtime
			}
		}
		if c.Arch == "" {
			c.Arch = runtime.GOARCH
		}
	}

	if c.Runtime == "" {
		c.Runtime = "docker"
	}

	var reqType usecase.ImageRequestType
	var imageRuntime, tag, version string

	conf, _ := app.LoadConfig(ctx)
	version = conf.ImageVersion
	if version == "" {
		version = "24.04"
	}

	if c.Docker != "" {
		reqType = usecase.RequestDockerImage
		image, t, err := parseDockerImage(c.Docker)
		if err != nil {
			return err
		}
		imageRuntime = image
		tag = t
	} else {
		reqType = usecase.RequestVMBaseImage
		imageRuntime = c.Runtime
		tag = c.Tag
	}

	archValue := domain.Arch(c.Arch).Value()
	goArch := archValue.GoArch()

	result, err := app.ImageRequester().Request(ctx, reqType, imageRuntime, goArch, tag, version, c.Wait)
	if err != nil {
		return fmt.Errorf("failed to request image: %w", err)
	}

	if result != nil {
		log.Infof("Issue created: #%d %s", result.IssueNumber, result.IssueURL)
	}
	return nil
}

type imagesLoadCmd struct {
	Docker string `short:"d" help:"Docker image to load from docker-mirror (e.g. postgres:15.8)"`
	Arch   string `short:"a" default:"" help:"Architecture (amd64, arm64)"`
	Force  bool   `short:"f" help:"Force re-download even if image exists locally"`
}

func (c *imagesLoadCmd) Run(g *Globals) error {
	g.resolveProfile("")
	app := g.app
	ctx := context.Background()

	if c.Docker == "" {
		return fmt.Errorf("--docker flag is required")
	}

	image, tag, err := parseDockerImage(c.Docker)
	if err != nil {
		return err
	}

	if c.Arch == "" {
		c.Arch = runtime.GOARCH
	}
	archValue := domain.Arch(c.Arch).Value()
	goArch := archValue.GoArch()

	// Check if already loaded in Docker
	if !c.Force {
		out, err := host.New().RunOutput("docker", "images", "--format", "{{.Repository}}:{{.Tag}}", image+":"+tag)
		if err == nil && strings.Contains(out, image+":"+tag) {
			log.Infof("Image %s:%s is already present locally", image, tag)
			return nil
		}
	}

	log.Infof("Looking up mirrored image %s:%s (%s)...", image, tag, goArch)
	info, err := app.ImageDiscovery().FindDockerMirrorImage(ctx, image, tag, goArch)
	if err != nil {
		return fmt.Errorf("image not found in docker-mirror: %w\n\nHint: run 'anvil images request --docker %s:%s' to request it", err, image, tag)
	}

	log.Infof("Found mirrored release: %s", info.Release)

	// Download to cache
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	cacheDir = filepath.Join(cacheDir, "anvil", "docker-images")
	if err := os.MkdirAll(cacheDir, 0750); err != nil {
		return fmt.Errorf("error creating cache dir: %w", err)
	}

	tarZstPath := filepath.Join(cacheDir, fmt.Sprintf("%s-%s-%s.tar.zst", image, tag, goArch))
	tarPath := filepath.Join(cacheDir, fmt.Sprintf("%s-%s-%s.tar", image, tag, goArch))

	// Check if already cached
	if _, err := os.Stat(tarPath); err == nil && !c.Force {
		log.Infof("Using cached archive: %s", tarPath)
	} else {
		log.Infof("Downloading %s ...", info.URL)
		req := downloader.Request{URL: info.URL}
		_, err := downloader.Download(host.New(), log.StandardLogger(), cacheDir, req)
		if err != nil {
			return fmt.Errorf("error downloading image: %w", err)
		}

		// downloader returns the SHA256-named cache file; we need to find it or use the direct path
		// The downloader API is file-oriented; let's use a simple direct download for the tar.zst
		// Actually, downloader.Download uses SHA256(url) as filename. We can get it from CacheFilename.
		cachedFile := downloader.CacheFilename(cacheDir, info.URL)
		if err := os.Rename(cachedFile, tarZstPath); err != nil {
			return fmt.Errorf("error moving cached file: %w", err)
		}

		log.Infof("Decompressing %s ...", tarZstPath)
		if err := host.New().Run("zstd", "-d", "-f", tarZstPath, "-o", tarPath); err != nil {
			return fmt.Errorf("error decompressing archive: %w", err)
		}
	}

	log.Infof("Loading image into Docker ...")
	if err := host.New().Run("docker", "load", "-i", tarPath); err != nil {
		return fmt.Errorf("error loading image: %w", err)
	}

	log.Infof("Done. Image %s:%s is ready.", image, tag)
	return nil
}

func parseDockerImage(s string) (image, tag string, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid docker image format: %s (expected image:tag)", s)
	}
	return parts[0], parts[1], nil
}

func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}
