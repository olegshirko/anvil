package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"anvil/internal/cli"
	"anvil/internal/environment"

	"github.com/coreos/go-semver/semver"
)

const minLimaVersion = "v0.18.0"

// ConfigureBinfmt installs QEMU binfmt handlers for the specified guest architecture.
func ConfigureBinfmt(host environment.HostActions, guest environment.GuestActions, arch environment.Arch) error {
	targetArch := environment.AARCH64
	if arch.Value().GoArch() == "arm64" {
		targetArch = environment.X8664
	}

	apply := func() error {
		cmd := fmt.Sprintf("sudo QEMU_PRESERVE_ARGV0=1 /usr/bin/binfmt --install 386,%s", targetArch.GoArch())
		if err := guest.Run("sh", "-c", cmd); err != nil {
			return fmt.Errorf("binfmt installation failed: %w", err)
		}
		return nil
	}

	if err := guest.RunQuiet("command", "-v", "binfmt"); err != nil {
		return fmt.Errorf("binfmt binary not found in guest: %w", err)
	}
	return apply()
}

var (
	versionCacheErr error
	versionCacheAt  time.Time
	versionCacheTTL = 5 * time.Second
)

// ValidateLimaVersion ensures that the host's Lima installation meets the minimum requirement.
func ValidateLimaVersion() error {
	if !versionCacheAt.IsZero() && time.Since(versionCacheAt) < versionCacheTTL {
		return versionCacheErr
	}

	ver, err := fetchLimaVersion()
	if err != nil {
		versionCacheErr = err
		versionCacheAt = time.Now()
		return versionCacheErr
	}

	if strings.EqualFold(ver, "HEAD") {
		logrus.Warnf("using Lima development build; ensure it is not older than %s", minLimaVersion)
		versionCacheErr = nil
		versionCacheAt = time.Now()
		return nil
	}

	minimum := semver.New(strings.TrimPrefix(minLimaVersion, "v"))
	installed, err := semver.NewVersion(strings.TrimPrefix(ver, "v"))
	if err != nil {
		versionCacheErr = fmt.Errorf("cannot parse Lima version %q: %w", ver, err)
		versionCacheAt = time.Now()
		return versionCacheErr
	}

	if minimum.Compare(*installed) > 0 {
		versionCacheErr = fmt.Errorf("anvil requires Lima >= %s (found %s). Upgrade with: brew upgrade lima", minLimaVersion, ver)
		versionCacheAt = time.Now()
		return versionCacheErr
	}

	versionCacheErr = nil
	versionCacheAt = time.Now()
	return nil
}

// fetchLimaVersion invokes `limactl info` and extracts the version string.
func fetchLimaVersion() (string, error) {
	var info struct {
		Version string `json:"version"`
	}
	var buf bytes.Buffer
	cmd := cli.Exec("limactl", "info").Cmd()
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cannot query Lima version: %w", err)
	}
	if err := json.NewDecoder(&buf).Decode(&info); err != nil {
		return "", fmt.Errorf("cannot decode limactl info: %w", err)
	}
	// strip prerelease suffix
	if i := strings.Index(info.Version, "-"); i >= 0 {
		info.Version = info.Version[:i]
	}
	return info.Version, nil
}
