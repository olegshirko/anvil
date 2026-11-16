package main

import (
	"testing"
)

// TestFastShimEligibility pins the stage-2 gate (builder-check resolution):
// the fast shim is the default for plain containers and the runc shim stays
// the fallback for everything the fast shim does not implement yet.
func TestFastShimEligibility(t *testing.T) {
	t.Setenv("ANVIL_FAST_SHIM", "")
	plain := func() (dockerCreateRequest, *containerMeta) {
		return dockerCreateRequest{}, &containerMeta{Networks: []string{"bridge"}}
	}

	t.Run("plain container is eligible", func(t *testing.T) {
		req, meta := plain()
		if !fastShimEligible(req, meta) {
			t.Fatal("plain container must be eligible")
		}
	})

	t.Run("privileged is not", func(t *testing.T) {
		req, meta := plain()
		req.HostConfig.Privileged = true
		if fastShimEligible(req, meta) {
			t.Fatal("privileged container must not be eligible")
		}
	})

	t.Run("custom seccomp profile is not", func(t *testing.T) {
		req, meta := plain()
		req.HostConfig.SecurityOpt = []string{`seccomp={"defaultAction":"SCMP_ACT_ALLOW"}`}
		if fastShimEligible(req, meta) {
			t.Fatal("custom seccomp profile must not be eligible")
		}
	})

	t.Run("unconfined seccomp is", func(t *testing.T) {
		req, meta := plain()
		req.HostConfig.SecurityOpt = []string{"seccomp=unconfined"}
		if !fastShimEligible(req, meta) {
			t.Fatal("seccomp=unconfined must be eligible")
		}
	})

	t.Run("default (no security-opt) is", func(t *testing.T) {
		req, meta := plain()
		req.HostConfig.SecurityOpt = []string{"no-new-privileges"}
		if !fastShimEligible(req, meta) {
			t.Fatal("non-seccomp security-opts must stay eligible")
		}
	})

	t.Run("pid container: is not", func(t *testing.T) {
		req, meta := plain()
		req.HostConfig.PidMode = "container:abc123"
		if fastShimEligible(req, meta) {
			t.Fatal("--pid container: must not be eligible")
		}
	})

	t.Run("pid host is not", func(t *testing.T) {
		req, meta := plain()
		req.HostConfig.PidMode = "host"
		if fastShimEligible(req, meta) {
			t.Fatal("--pid host must not be eligible: the init is always born in a new pid namespace")
		}
	})

	t.Run("subpath mounts are not", func(t *testing.T) {
		req, meta := plain()
		meta.SubpathMounts = []subpathMount{{VolumeDir: "/v", Subpath: "data"}}
		if fastShimEligible(req, meta) {
			t.Fatal("subpath mounts must not be eligible")
		}
	})

	t.Run("secondary networks are not", func(t *testing.T) {
		req, meta := plain()
		meta.Networks = append(meta.Networks, "extra")
		if fastShimEligible(req, meta) {
			t.Fatal("secondary networks must not be eligible")
		}
	})

	t.Run("custom runtime is not", func(t *testing.T) {
		req, meta := plain()
		req.HostConfig.Runtime = "youki"
		if fastShimEligible(req, meta) {
			t.Fatal("custom --runtime must not be eligible")
		}
	})

	t.Run("explicit runc runtime is", func(t *testing.T) {
		req, meta := plain()
		req.HostConfig.Runtime = "runc"
		if !fastShimEligible(req, meta) {
			t.Fatal("--runtime runc is the default shim and must be eligible")
		}
	})

	t.Run("nil meta is not", func(t *testing.T) {
		if fastShimEligible(dockerCreateRequest{}, nil) {
			t.Fatal("nil meta must not be eligible")
		}
	})

	t.Run("kill switch disables wholesale", func(t *testing.T) {
		t.Setenv("ANVIL_FAST_SHIM", "0")
		req, meta := plain()
		if fastShimEligible(req, meta) {
			t.Fatal("ANVIL_FAST_SHIM=0 must make everything ineligible")
		}
	})
}
