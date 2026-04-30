package lima

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"anvil/internal/domain"
	"anvil/internal/environment/vm/lima/limaconfig"
	"anvil/internal/util"
)

func Test_validateMounts(t *testing.T) {
	tests := []struct {
		paths   []string
		wantErr bool
	}{
		{paths: []string{"/User", "/User/something"}, wantErr: true},
		{paths: []string{"/User/one", "/User/two"}, wantErr: false},
		{paths: []string{"/User/one", "/User/one_other"}, wantErr: false},
		{paths: []string{"/User/one_other", "/User/one"}, wantErr: false},
		{paths: []string{"/User/one", "/User/one/other"}, wantErr: true},
		{paths: []string{"/User/one/", "/User/one"}, wantErr: true},
		{paths: []string{"/User/one/", "/User/two", "User/one"}, wantErr: true},
		{paths: []string{"/home/a/b/c", "/home/b/c/a", "/home/c/a/b"}, wantErr: false},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			mounts := make([]domain.Mount, len(tt.paths))
			for j, p := range tt.paths {
				mounts[j] = domain.Mount{Location: p}
			}
			if err := validateMounts(mounts); (err != nil) != tt.wantErr {
				t.Errorf("validateMounts() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_buildLimaConfig_Mounts(t *testing.T) {
	homeDir, err := util.UserHome()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		mounts        []string
		isDefault     bool
		includesCache bool
	}{
		{mounts: []string{"/User/user", "/tmp/another"}},
		{mounts: []string{"/User/another", "/User/something", "/User/else"}},
		{isDefault: true},
		{mounts: []string{homeDir}, includesCache: true},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			mounts := make([]domain.Mount, len(tt.mounts))
			for j, m := range tt.mounts {
				mounts[j] = domain.Mount{Location: m}
			}
			cfg, err := buildLimaConfig(context.Background(), domain.Config{Mounts: mounts}, &domain.Profile{ID: "test-profile", DisplayName: "Test Profile", ShortName: "test"}, "/home/user/.cache/anvil", "/tmp/config")
			if err != nil {
				t.Error(err)
				return
			}

			var expected []string
			if tt.isDefault {
				expected = []string{"~", "/tmp/anvil"}
			} else {
				expected = append([]string{"/home/user/.cache/anvil"}, tt.mounts...)
			}

			for i, m := range cfg.Mounts {
				got := strings.TrimSuffix(m.Location, "/")
				want := strings.TrimSuffix(expected[i], "/")
				if got != want {
					t.Errorf("mounts mismatch at index %d: got %s, want %s", i, got, want)
				}
			}
		})
	}
}

func Test_ingressIsDisabled(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"--flag=f", "--another", "flag"}, want: false},
		{args: []string{"--disable=traefik", "--version=3"}, want: true},
		{args: []string{}, want: false},
		{args: []string{"--disable", "traefik", "--one=two"}, want: true},
	}
	for i, tt := range tests {
		t.Run(strconv.Itoa(i+1), func(t *testing.T) {
			if got := ingressIsDisabled(tt.args); got != tt.want {
				t.Errorf("ingressIsDisabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_isImageVersionAtLeast(t *testing.T) {
	tests := []struct {
		version string
		min     string
		want    bool
	}{
		{"24.04", "26.04", false},
		{"26.04", "26.04", true},
		{"26.10", "26.04", true},
		{"28.04", "26.04", true},
		{"", "26.04", false},
		{"26.04", "", true},
	}
	for i, tt := range tests {
		t.Run(strconv.Itoa(i+1), func(t *testing.T) {
			if got := isImageVersionAtLeast(tt.version, tt.min); got != tt.want {
				t.Errorf("isImageVersionAtLeast(%q, %q) = %v, want %v", tt.version, tt.min, got, tt.want)
			}
		})
	}
}

func Test_buildLimaConfig_CloudInitDataSource(t *testing.T) {
	for _, version := range []string{"", "24.04", "26.04", "28.04"} {
		c, err := buildLimaConfig(context.Background(), domain.Config{ImageVersion: version}, &domain.Profile{ID: "test", DisplayName: "Test", ShortName: "test"}, "/tmp/cache", "/tmp/config")
		if err != nil {
			t.Fatalf("buildLimaConfig failed: %v", err)
		}
		found := false
		for _, p := range c.Provision {
			if p.Path == "/etc/cloud/cloud.cfg.d/98-anvil-datasource.cfg" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected datasource restriction for version %q", version)
		}
	}
}

func Test_buildLimaConfig_HostsEntries(t *testing.T) {
	c, err := buildLimaConfig(context.Background(), domain.Config{}, &domain.Profile{ID: "my-profile", DisplayName: "My Profile", ShortName: "my"}, "/tmp/cache", "/tmp/config")
	if err != nil {
		t.Fatalf("buildLimaConfig failed: %v", err)
	}

	var bootHosts, systemHosts string
	for _, p := range c.Provision {
		if p.Mode == limaconfig.ProvisionModeBoot && strings.Contains(p.Script, "/etc/hosts") {
			bootHosts = p.Script
		}
		if p.Mode == limaconfig.ProvisionModeSystem && strings.Contains(p.Script, "/etc/hosts") {
			systemHosts = p.Script
		}
	}

	for _, h := range []string{"my-profile", "lima-my-profile"} {
		if !strings.Contains(bootHosts, h) {
			t.Errorf("expected boot provision to contain hostname %q, got: %s", h, bootHosts)
		}
		if !strings.Contains(systemHosts, h) {
			t.Errorf("expected system provision to contain hostname %q, got: %s", h, systemHosts)
		}
	}
}

func Test_buildLimaConfig_NetworkdWaitOnline(t *testing.T) {
	c, err := buildLimaConfig(context.Background(), domain.Config{}, &domain.Profile{ID: "test", DisplayName: "Test", ShortName: "test"}, "/tmp/cache", "/tmp/config")
	if err != nil {
		t.Fatalf("buildLimaConfig failed: %v", err)
	}

	found := false
	for _, p := range c.Provision {
		if p.Path == "/etc/systemd/system/systemd-networkd-wait-online.service.d/override.conf" {
			found = true
			if !strings.Contains(p.Content, "--any") {
				t.Errorf("expected networkd wait-online override to contain '--any', got: %s", p.Content)
			}
			break
		}
	}
	if !found {
		t.Error("expected networkd wait-online override provision")
	}
}

func Test_buildLimaConfig_DockerServiceDisabled(t *testing.T) {
	c, err := buildLimaConfig(context.Background(), domain.Config{Runtime: "docker"}, &domain.Profile{ID: "test", DisplayName: "Test", ShortName: "test"}, "/tmp/cache", "/tmp/config")
	if err != nil {
		t.Fatalf("buildLimaConfig failed: %v", err)
	}

	found := false
	for _, p := range c.Provision {
		if p.Mode == limaconfig.ProvisionModeSystem && strings.Contains(p.Script, "systemctl disable docker.service") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected docker.service disable provision for docker runtime")
	}

	// Non-docker runtimes should not contain the disable provision.
	for _, rt := range []string{"", "containerd", "incus"} {
		c, err := buildLimaConfig(context.Background(), domain.Config{Runtime: rt}, &domain.Profile{ID: "test", DisplayName: "Test", ShortName: "test"}, "/tmp/cache", "/tmp/config")
		if err != nil {
			t.Fatalf("buildLimaConfig failed: %v", err)
		}
		for _, p := range c.Provision {
			if strings.Contains(p.Script, "systemctl disable docker.service") {
				t.Errorf("unexpected docker.service disable provision for runtime %q", rt)
			}
		}
	}
}
