package main

import (
	"context"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestSeccompSpecOptSelection(t *testing.T) {
	cases := []struct {
		name       string
		opts       []string
		privileged bool
		wantOpt    bool
	}{
		{"default profile", nil, false, true},
		{"builtin", []string{"seccomp=builtin"}, false, true},
		{"unconfined", []string{"seccomp=unconfined"}, false, false},
		{"legacy false alias", []string{"seccomp=false"}, false, false},
		{"legacy colon separator", []string{"seccomp:unconfined"}, false, false},
		{"privileged drops the default", nil, true, false},
		{"other opts keep the default", []string{"no-new-privileges"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt, err := seccompSpecOpt(tc.opts, tc.privileged)
			if err != nil {
				t.Fatal(err)
			}
			if (opt != nil) != tc.wantOpt {
				t.Fatalf("opt present = %v, want %v", opt != nil, tc.wantOpt)
			}
		})
	}
	if _, err := seccompSpecOpt([]string{"seccomp={not json"}, false); err == nil {
		t.Fatal("malformed profile accepted")
	}
}

// A Docker-format profile: archMap, includes/excludes on caps and arches.
const testDockerProfile = `{
  "defaultAction": "SCMP_ACT_ERRNO",
  "archMap": [
    {"architecture": "SCMP_ARCH_X86_64", "subArchitectures": ["SCMP_ARCH_X86"]},
    {"architecture": "SCMP_ARCH_AARCH64", "subArchitectures": ["SCMP_ARCH_ARM"]}
  ],
  "syscalls": [
    {"names": ["read", "write"], "action": "SCMP_ACT_ALLOW"},
    {"name": "legacy", "action": "SCMP_ACT_ALLOW"},
    {"names": ["mount"], "action": "SCMP_ACT_ALLOW", "includes": {"caps": ["CAP_SYS_ADMIN"]}},
    {"names": ["clone"], "action": "SCMP_ACT_ALLOW", "excludes": {"caps": ["CAP_SYS_ADMIN"]}},
    {"names": ["arch_prctl"], "action": "SCMP_ACT_ALLOW", "includes": {"arches": ["amd64"]}},
    {"names": ["arm_only"], "action": "SCMP_ACT_ALLOW", "includes": {"arches": ["arm64"]}},
    {"names": ["not_on_arm"], "action": "SCMP_ACT_ALLOW", "excludes": {"arches": ["arm64"]}}
  ]
}`

func TestDockerSeccompProfileTranslation(t *testing.T) {
	run := func(caps []string) *specs.LinuxSeccomp {
		t.Helper()
		opt, err := seccompSpecOpt([]string{"seccomp=" + testDockerProfile}, false)
		if err != nil {
			t.Fatal(err)
		}
		s := &specs.Spec{Process: &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: caps}}, Linux: &specs.Linux{}}
		if err := opt(context.Background(), nil, nil, s); err != nil {
			t.Fatal(err)
		}
		return s.Linux.Seccomp
	}
	names := func(sc *specs.LinuxSeccomp) map[string]bool {
		out := map[string]bool{}
		for _, r := range sc.Syscalls {
			for _, n := range r.Names {
				out[n] = true
			}
		}
		return out
	}

	sc := run([]string{"CAP_CHOWN"})
	if sc.DefaultAction != specs.ActErrno {
		t.Errorf("defaultAction = %q", sc.DefaultAction)
	}
	if len(sc.Architectures) != 2 || sc.Architectures[0] != specs.ArchAARCH64 || sc.Architectures[1] != specs.ArchARM {
		t.Errorf("architectures = %v, want aarch64+arm", sc.Architectures)
	}
	got := names(sc)
	for _, want := range []string{"read", "write", "legacy", "clone", "arm_only"} {
		if !got[want] {
			t.Errorf("unprivileged: %q missing", want)
		}
	}
	for _, bad := range []string{"mount", "arch_prctl", "not_on_arm"} {
		if got[bad] {
			t.Errorf("unprivileged: %q should be filtered", bad)
		}
	}

	got = names(run([]string{"CAP_SYS_ADMIN"}))
	if !got["mount"] || got["clone"] {
		t.Errorf("CAP_SYS_ADMIN: mount=%v clone=%v, want true/false", got["mount"], got["clone"])
	}
}
