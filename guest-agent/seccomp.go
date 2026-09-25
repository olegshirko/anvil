package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Seccomp confinement, Docker semantics: every unprivileged container gets
// the default profile (containerd's port of moby's, capability-aware) unless
// it asks for seccomp=unconfined or supplies its own profile. The docker CLI
// reads `--security-opt seccomp=<file>` on the client and sends the file
// CONTENTS, in Docker's profile format (archMap, includes/excludes), which
// is translated to the OCI shape here.

// guestSeccompArch is the only architecture the guest runs; Docker profile
// filters name it the Go way.
const guestSeccompArch = "arm64"

// seccompSpecOpt returns the option applying the requested seccomp policy,
// or nil for no filter. It must run after every capability option: both the
// default profile and Docker-profile filters depend on the bounding set.
func seccompSpecOpt(securityOpts []string, privileged bool) (oci.SpecOpts, error) {
	profile := "" // "" = default profile
	for _, opt := range securityOpts {
		v, ok := strings.CutPrefix(opt, "seccomp=")
		if !ok {
			v, ok = strings.CutPrefix(opt, "seccomp:") // legacy separator
		}
		if ok {
			profile = v
		}
	}
	switch profile {
	case "unconfined", "false":
		return nil, nil
	case "", "builtin":
		if privileged {
			return nil, nil // docker: --privileged disables the default profile
		}
		return seccomp.WithDefaultProfile(), nil
	}
	var p dockerSeccompProfile
	if err := json.Unmarshal([]byte(profile), &p); err != nil {
		return nil, fmt.Errorf("security-opt seccomp: decoding profile: %v", err)
	}
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		var caps []string
		if s.Process != nil && s.Process.Capabilities != nil {
			caps = s.Process.Capabilities.Bounding
		}
		sc, err := p.toOCI(caps)
		if err != nil {
			return err
		}
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		s.Linux.Seccomp = sc
		return nil
	}, nil
}

// dockerSeccompProfile is moby's profile format (profiles/seccomp.Seccomp).
type dockerSeccompProfile struct {
	DefaultAction    specs.LinuxSeccompAction `json:"defaultAction"`
	DefaultErrnoRet  *uint                    `json:"defaultErrnoRet,omitempty"`
	Architectures    []specs.Arch             `json:"architectures,omitempty"`
	ArchMap          []dockerSeccompArch      `json:"archMap,omitempty"`
	Flags            []specs.LinuxSeccompFlag `json:"flags,omitempty"`
	ListenerPath     string                   `json:"listenerPath,omitempty"`
	ListenerMetadata string                   `json:"listenerMetadata,omitempty"`
	Syscalls         []dockerSeccompSyscall   `json:"syscalls"`
}

type dockerSeccompArch struct {
	Arch      specs.Arch   `json:"architecture"`
	SubArches []specs.Arch `json:"subArchitectures"`
}

type dockerSeccompSyscall struct {
	Name     string                   `json:"name,omitempty"` // pre-1.13 single-name form
	Names    []string                 `json:"names,omitempty"`
	Action   specs.LinuxSeccompAction `json:"action"`
	ErrnoRet *uint                    `json:"errnoRet,omitempty"`
	Args     []specs.LinuxSeccompArg  `json:"args"`
	Includes *dockerSeccompFilter     `json:"includes,omitempty"`
	Excludes *dockerSeccompFilter     `json:"excludes,omitempty"`
}

// dockerSeccompFilter gates a rule on capabilities and architectures.
// minKernel is not evaluated: the guest kernel (6.6) is newer than any
// minimum the stock profiles use.
type dockerSeccompFilter struct {
	Caps   []string `json:"caps,omitempty"`
	Arches []string `json:"arches,omitempty"`
}

// toOCI resolves the profile for a container with the given bounding set.
func (p dockerSeccompProfile) toOCI(caps []string) (*specs.LinuxSeccomp, error) {
	if p.DefaultAction == "" {
		return nil, fmt.Errorf("security-opt seccomp: profile has no defaultAction")
	}
	out := &specs.LinuxSeccomp{
		DefaultAction:    p.DefaultAction,
		DefaultErrnoRet:  p.DefaultErrnoRet,
		Architectures:    p.Architectures,
		Flags:            p.Flags,
		ListenerPath:     p.ListenerPath,
		ListenerMetadata: p.ListenerMetadata,
	}
	if len(p.ArchMap) > 0 {
		out.Architectures = nil
		for _, a := range p.ArchMap {
			if a.Arch == specs.ArchAARCH64 {
				out.Architectures = append([]specs.Arch{a.Arch}, a.SubArches...)
			}
		}
	}
	for _, sc := range p.Syscalls {
		if !sc.Includes.matches(caps, true) || (sc.Excludes != nil && sc.Excludes.matches(caps, false)) {
			continue
		}
		names := sc.Names
		if sc.Name != "" {
			names = append([]string{sc.Name}, names...)
		}
		if len(names) == 0 {
			continue
		}
		out.Syscalls = append(out.Syscalls, specs.LinuxSyscall{
			Names: names, Action: sc.Action, ErrnoRet: sc.ErrnoRet, Args: sc.Args,
		})
	}
	return out, nil
}

// matches reports whether the filter applies. For includes every listed
// cap and the guest arch must be present (all=true); for excludes a single
// hit on either list is enough (all=false). A nil includes filter always
// matches.
func (f *dockerSeccompFilter) matches(caps []string, all bool) bool {
	if f == nil {
		return all
	}
	capHits := 0
	for _, c := range f.Caps {
		if slices.Contains(caps, c) {
			capHits++
		}
	}
	archHit := slices.Contains(f.Arches, guestSeccompArch)
	if all {
		return capHits == len(f.Caps) && (len(f.Arches) == 0 || archHit)
	}
	return capHits > 0 || archHit
}
