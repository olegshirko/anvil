package limautil

import (
	"strings"
	"testing"

	"anvil/internal/environment"
)

func TestImage(t *testing.T) {
	tests := []struct {
		name      string
		arch      environment.Arch
		runtime   string
		version   string
		wantURL   string
		wantError bool
	}{
		{
			name:    "arm64 docker 24.04",
			arch:    environment.AARCH64,
			runtime: "docker",
			version: "24.04",
			wantURL: "ubuntu-24.04-minimal-cloudimg-arm64-docker.qcow2",
		},
		{
			name:    "amd64 containerd 24.04",
			arch:    environment.X8664,
			runtime: "containerd",
			version: "24.04",
			wantURL: "ubuntu-24.04-minimal-cloudimg-amd64-containerd.qcow2",
		},
		{
			name:    "default version empty string",
			arch:    environment.AARCH64,
			runtime: "none",
			version: "",
			wantURL: "ubuntu-24.04-minimal-cloudimg-arm64-none.qcow2",
		},
		{
			name:      "unknown version",
			arch:      environment.AARCH64,
			runtime:   "docker",
			version:   "99.99",
			wantError: true,
		},
		{
			name:      "unknown runtime",
			arch:      environment.AARCH64,
			runtime:   "unknown",
			version:   "24.04",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img, err := Image(tt.arch, tt.runtime, tt.version)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(img.Location, tt.wantURL) {
				t.Fatalf("expected URL to contain %q, got %q", tt.wantURL, img.Location)
			}
			if img.Digest == "" {
				t.Fatalf("expected digest to be set")
			}
			if !strings.HasPrefix(img.Digest, "sha512:") {
				t.Fatalf("expected sha512 digest, got %q", img.Digest)
			}
		})
	}
}

func TestImage_AllRuntimesAndArches(t *testing.T) {
	versions := []string{"24.04"}
	arches := []environment.Arch{environment.AARCH64, environment.X8664}
	runtimes := []string{"none", "docker", "containerd", "incus"}

	for _, version := range versions {
		for _, arch := range arches {
			for _, runtime := range runtimes {
				_, err := Image(arch, runtime, version)
				if err != nil {
					t.Errorf("Image(%q, %q, %q) failed: %v", arch, runtime, version, err)
				}
			}
		}
	}
}
