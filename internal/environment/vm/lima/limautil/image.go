package limautil

import (
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"anvil/internal/embedded/images"
	"anvil/internal/environment"
	"anvil/internal/environment/host"
	"anvil/internal/environment/vm/lima/limaconfig"
	"anvil/internal/util"
	"anvil/internal/util/downloader"
)

const defaultImageVersion = "24.04"

// ResolveImage looks up the Lima disk image metadata for the given arch, runtime, and version.
func ResolveImage(arch environment.Arch, runtime string, version string) (limaconfig.File, error) {
	if version == "" {
		version = defaultImageVersion
	}

	ver, ok := images.Manifest[version]
	if !ok {
		return limaconfig.File{}, fmt.Errorf("image version %q not found", version)
	}

	archMap, ok := ver.Artifacts[arch.GoArch()]
	if !ok {
		return limaconfig.File{}, fmt.Errorf("no image for %s / %s (version %s)", arch, runtime, version)
	}

	art, ok := archMap[runtime]
	if !ok {
		return limaconfig.File{}, fmt.Errorf("no image for %s / %s (version %s)", arch, runtime, version)
	}

	return limaconfig.File{
		Location: art.URL,
		Arch:     arch,
		Digest:   art.Digest,
	}, nil
}

// Image returns the details of the disk image to download for the arch, runtime and version.
func Image(arch environment.Arch, runtime string, version string) (limaconfig.File, error) {
	return ResolveImage(arch, runtime, version)
}

// ImageCached returns if the image for architecture, runtime and version
// has been previously downloaded and cached.
func ImageCached(arch environment.Arch, runtime string, version string, cacheDir string) (limaconfig.File, bool) {
	img, err := ResolveImage(arch, runtime, version)
	if err != nil {
		return img, false
	}

	ci := cachedImage(downloader.CacheFilename(cacheDir, img.Location))
	img.Location = ci.preferredPath()
	img.Digest = ""
	return img, ci.exists()
}

// DownloadImage downloads the image for arch, runtime and version.
func DownloadImage(arch environment.Arch, runtime string, version string, cacheDir string) (limaconfig.File, error) {
	img, err := ResolveImage(arch, runtime, version)
	if err != nil {
		return img, err
	}

	h := host.New()
	path, err := fetchImage(h, logrus.StandardLogger(), cacheDir, img)
	if err != nil {
		return limaconfig.File{}, err
	}

	ci := cachedImage(path)

	// If qemu-img is unavailable, return the qcow2 as-is.
	if err := util.RequireQemuImg(); err != nil {
		img.Location = ci.String()
		img.Digest = ""
		return img, nil
	}

	raw, err := convertToRaw(h, ci)
	if err != nil {
		return limaconfig.File{}, err
	}

	img.Location = raw
	img.Digest = ""
	return img, nil
}

// DownloadKernel downloads the kernel for the given arch, runtime and version.
// It derives the URL from the qcow2 image URL by replacing the .qcow2 suffix.
// Returns empty path if the download fails (kernel is optional for direct boot).
func DownloadKernel(arch environment.Arch, runtime string, version string, cacheDir string) (string, error) {
	img, err := ResolveImage(arch, runtime, version)
	if err != nil {
		return "", err
	}

	h := host.New()
	kernelURL := strings.Replace(img.Location, ".qcow2", "-vmlinuz", 1)
	path, _ := downloader.Download(h, logrus.StandardLogger(), cacheDir, downloader.Request{URL: kernelURL})
	return path, nil
}

func fetchImage(h environment.HostActions, log *logrus.Logger, cacheDir string, file limaconfig.File) (string, error) {
	req := downloader.Request{URL: file.Location}
	if file.Digest != "" {
		req.SHA = &downloader.SHA{Size: 512, Digest: file.Digest}
	}
	loc, err := downloader.Download(h, log, cacheDir, req)
	if err != nil {
		return "", fmt.Errorf("image download failed: %w", err)
	}
	return loc, nil
}

func convertToRaw(h environment.HostActions, ci cachedImage) (string, error) {
	if _, err := os.Stat(ci.rawPath()); err == nil {
		return ci.rawPath(), nil
	}
	if err := h.Run("qemu-img", "convert", "-f", "qcow2", "-O", "raw", ci.String(), ci.rawPath()); err != nil {
		_ = h.RunQuiet("rm", "-f", ci.rawPath())
		return "", err
	}
	return ci.rawPath(), nil
}

// cachedImage tracks the on-disk paths for a downloaded VM image.
type cachedImage string

func (c cachedImage) String() string { return strings.TrimSuffix(string(c), ".raw") }

func (c cachedImage) rawPath() string { return c.String() + ".raw" }

func (c cachedImage) exists() bool {
	stat, err := os.Stat(c.preferredPath())
	return err == nil && !stat.IsDir()
}

func (c cachedImage) preferredPath() string {
	if util.RequireQemuImg() == nil {
		return c.rawPath()
	}
	return c.String()
}
