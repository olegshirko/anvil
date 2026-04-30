package usecase

import "context"

// VMBaseImage represents a VM base image asset from a GitHub release.
type VMBaseImage struct {
	Arch    string
	Runtime string
	URL     string
	Size    int64
}

// DockerMirrorImage represents a Docker image mirrored to a GitHub release.
type DockerMirrorImage struct {
	Image   string
	Tag     string
	Arch    string
	Release string
	URL     string
	Size    int64
}

// DockerMirrorImageInfo holds details for a specific mirrored Docker image.
type DockerMirrorImageInfo struct {
	Image   string
	Tag     string
	Arch    string
	Release string
	URL     string
}

// ImageDiscovery provides ways to inspect what images are already available.
type ImageDiscovery interface {
	// ListVMBaseImages returns available qcow2 assets from docker-mirror releases.
	ListVMBaseImages(ctx context.Context, tag string) ([]VMBaseImage, error)
	// ListDockerMirrorImages returns Docker images mirrored by reconcile.yml.
	ListDockerMirrorImages(ctx context.Context) ([]DockerMirrorImage, error)
	// FindDockerMirrorImage returns the mirrored image info for a given image+tag+arch.
	FindDockerMirrorImage(ctx context.Context, image, tag, arch string) (*DockerMirrorImageInfo, error)
	// CheckContainerImage checks Docker Hub for a given image+tag+platform.
	CheckContainerImage(ctx context.Context, image, tag, platform string) (bool, error)
}
