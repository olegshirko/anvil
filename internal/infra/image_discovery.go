package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"anvil/internal/usecase"
	"anvil/internal/util/github"
)

// GitHubImageDiscovery discovers images via the GitHub API.
type GitHubImageDiscovery struct {
	client *github.Client
}

// NewGitHubImageDiscovery creates a new discovery service.
func NewGitHubImageDiscovery(client *github.Client) usecase.ImageDiscovery {
	return &GitHubImageDiscovery{client: client}
}

// ListVMBaseImages queries docker-mirror releases for qcow2 assets.
func (d *GitHubImageDiscovery) ListVMBaseImages(ctx context.Context, tag string) ([]usecase.VMBaseImage, error) {
	releases, err := d.client.ListReleases(ctx, "olegshirko", "docker-mirror")
	if err != nil {
		return nil, err
	}

	var images []usecase.VMBaseImage
	for _, rel := range releases {
		if rel.TagName != tag {
			continue
		}
		for _, asset := range rel.Assets {
			name := asset.Name
			if !strings.HasSuffix(name, ".qcow2") {
				continue
			}
			// Parse filename: ubuntu-24.04-minimal-cloudimg-<arch>-<runtime>.qcow2
			parts := strings.Split(strings.TrimSuffix(name, ".qcow2"), "-")
			if len(parts) < 2 {
				continue
			}
			arch := parts[len(parts)-2]
			runtime := parts[len(parts)-1]
			images = append(images, usecase.VMBaseImage{
				Arch:    arch,
				Runtime: runtime,
				URL:     asset.BrowserDownloadURL,
				Size:    asset.Size,
			})
		}
		break
	}
	return images, nil
}

// ListDockerMirrorImages returns Docker images mirrored by reconcile.yml.
func (d *GitHubImageDiscovery) ListDockerMirrorImages(ctx context.Context) ([]usecase.DockerMirrorImage, error) {
	releases, err := d.client.ListReleases(ctx, "olegshirko", "docker-mirror")
	if err != nil {
		return nil, err
	}

	var images []usecase.DockerMirrorImage
	for _, rel := range releases {
		if rel.TagName == "master" {
			continue
		}
		for _, asset := range rel.Assets {
			if !strings.HasPrefix(asset.Name, "image-") || !strings.HasSuffix(asset.Name, ".tar.zst") {
				continue
			}
			// image-<arch>.tar.zst
			arch := strings.TrimSuffix(strings.TrimPrefix(asset.Name, "image-"), ".tar.zst")
			images = append(images, usecase.DockerMirrorImage{
				Image:   rel.Name,
				Tag:     "",
				Arch:    arch,
				Release: rel.TagName,
				URL:     asset.URL,
				Size:    asset.Size,
			})
		}
	}
	return images, nil
}

// FindDockerMirrorImage looks up a specific mirrored Docker image by release tag.
func (d *GitHubImageDiscovery) FindDockerMirrorImage(ctx context.Context, image, tag, arch string) (*usecase.DockerMirrorImageInfo, error) {
	safeName := strings.ReplaceAll(image, "/", "-")
	releaseTag := fmt.Sprintf("%s-%s-%s", safeName, tag, arch)

	release, err := d.client.GetReleaseByTag(ctx, "olegshirko", "docker-mirror", releaseTag)
	if err != nil {
		return nil, fmt.Errorf("release not found: %w", err)
	}

	assetName := fmt.Sprintf("image-%s.tar.zst", arch)
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			return &usecase.DockerMirrorImageInfo{
				Image:   image,
				Tag:     tag,
				Arch:    arch,
				Release: releaseTag,
				URL:     asset.BrowserDownloadURL,
			}, nil
		}
	}
	return nil, fmt.Errorf("asset %s not found in release %s", assetName, releaseTag)
}

// CheckContainerImage uses the Docker Hub registry API to check if a manifest exists.
func (d *GitHubImageDiscovery) CheckContainerImage(ctx context.Context, image, tag, platform string) (bool, error) {
	parts := strings.SplitN(image, "/", 2)
	var repo string
	if len(parts) == 1 {
		repo = "library/" + image
	} else {
		repo = image
	}

	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags/%s", repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, nil // treat network errors as "not available"
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, nil
	}

	if platform == "" {
		return true, nil
	}

	var result struct {
		Images []struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"images"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return true, nil
	}

	for _, img := range result.Images {
		if img.Architecture == platform || img.OS+"/"+img.Architecture == platform {
			return true, nil
		}
	}
	return false, nil
}
