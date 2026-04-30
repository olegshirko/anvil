package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"anvil/internal/domain"
	"anvil/internal/environment/host"
	"anvil/internal/usecase"
	"anvil/internal/util/downloader"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type composeCmd struct {
	File string   `short:"f" default:"docker-compose.yml" help:"Compose file path"`
	Args []string `arg:"" optional:"" passthrough:"" help:"Arguments passed to docker compose"`
}

func (c *composeCmd) Run(g *Globals) error {
	g.resolveProfile("")

	// Resolve compose file path
	composeFile := c.File
	if _, err := os.Stat(composeFile); err != nil {
		// Try docker-compose.yaml as fallback
		alt := filepath.Join(filepath.Dir(composeFile), "docker-compose.yaml")
		if _, err2 := os.Stat(alt); err2 == nil {
			composeFile = alt
		}
	}

	// Determine if we need to prefetch images
	cmd := ""
	if len(c.Args) > 0 {
		cmd = c.Args[0]
	}
	needsPrefetch := cmd == "up" || cmd == "run" || cmd == "create"

	if needsPrefetch {
		images, err := parseComposeImages(composeFile)
		if err != nil {
			logrus.Warnf("could not parse compose file: %v", err)
		} else if len(images) > 0 {
			if err := prefetchComposeImages(g.app, images); err != nil {
				logrus.Warnf("prefetch failed: %v", err)
			}
		}
	}

	// Pass through to docker compose
	args := append([]string{"docker", "compose", "-f", composeFile}, c.Args...)
	return host.New().RunInteractive(args...)
}

func parseComposeImages(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	var compose struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &compose); err != nil {
		return nil, err
	}

	var images []string
	seen := make(map[string]bool)
	for _, svc := range compose.Services {
		if svc.Image != "" && !seen[svc.Image] {
			images = append(images, svc.Image)
			seen[svc.Image] = true
		}
	}
	return images, nil
}

func prefetchComposeImages(app usecase.Application, images []string) error {
	ctx := context.Background()

	h := host.New()
	for _, img := range images {
		// Check if already present locally
		out, _ := h.RunOutput("docker", "images", "--format", "{{.Repository}}:{{.Tag}}", img)
		if strings.Contains(out, img) {
			logrus.Infof("Image %s already present locally", img)
			continue
		}

		// Try docker pull first
		logrus.Infof("Pulling %s ...", img)
		if err := h.Run("docker", "pull", img); err == nil {
			continue
		}

		// Fallback to docker-mirror
		logrus.Warnf("docker pull failed for %s, trying docker-mirror fallback...", img)

		imageName, tag, err := parseDockerImage(img)
		if err != nil {
			logrus.Warnf("invalid image format %s: %v", img, err)
			continue
		}

		arch := domain.Arch("").Value().GoArch()
		info, err := app.ImageDiscovery().FindDockerMirrorImage(ctx, imageName, tag, arch)
		if err != nil {
			logrus.Warnf("image %s not found in docker-mirror: %v", img, err)
			continue
		}

		cacheDir, err := os.UserCacheDir()
		if err != nil {
			cacheDir = os.TempDir()
		}
		cacheDir = filepath.Join(cacheDir, "anvil", "docker-images")
		_ = os.MkdirAll(cacheDir, 0750)

		tarZstPath := filepath.Join(cacheDir, fmt.Sprintf("%s-%s-%s.tar.zst", imageName, tag, arch))
		tarPath := filepath.Join(cacheDir, fmt.Sprintf("%s-%s-%s.tar", imageName, tag, arch))

		// Check cached
		if _, err := os.Stat(tarPath); err != nil {
			req := downloader.Request{URL: info.URL}
			cachedFile, err := downloader.Download(h, logrus.StandardLogger(), cacheDir, req)
			if err != nil {
				logrus.Errorf("download failed for %s: %v", img, err)
				continue
			}
			if err := os.Rename(cachedFile, tarZstPath); err != nil {
				logrus.Errorf("cache rename failed: %v", err)
				continue
			}
			if err := h.Run("zstd", "-d", "-f", tarZstPath, "-o", tarPath); err != nil {
				logrus.Errorf("decompress failed for %s: %v", img, err)
				continue
			}
		}

		if err := h.Run("docker", "load", "-i", tarPath); err != nil {
			logrus.Errorf("docker load failed for %s: %v", img, err)
			continue
		}
		logrus.Infof("Loaded %s from docker-mirror", img)
	}
	return nil
}
