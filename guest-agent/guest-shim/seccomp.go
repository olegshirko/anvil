//go:build linux

package main

import (
	"fmt"
	"runtime"

	"github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// OCI seccomp profile -> BPF program. The agent applies containerd's default
// profile (docker's syscall allowlist, argument filters are Equal-style
// personality/socket constraints only) to every non-privileged container,
// so the shim must install it without libseccomp: go-seccomp-bpf assembles
// the classic socket-filter program and loads it via prctl/seccomp(2).
// Custom profiles are excluded from fast-shim eligibility by the agent; the
// argument operators beyond the single-value ones fail loudly here (Create
// fails, the runc shim fallback takes over).

// applySeccomp installs the filter in the current (init) process. Must run
// last, right before the user exec — the filter applies to this process.
// no-new-privileges is always set with it: the default container capability
// set has no CAP_SYS_ADMIN, and seccomp requires one or the other (runc
// does the same for its default-caps containers).
func applySeccomp(sc *specs.LinuxSeccomp) error {
	filter, err := seccompFilter(sc)
	if err != nil {
		return err
	}
	filter.NoNewPrivs = true
	if err := seccomp.LoadFilter(*filter); err != nil {
		return fmt.Errorf("load seccomp filter: %w", err)
	}
	return nil
}

// seccompFilter converts the OCI structure into a go-seccomp-bpf Filter for
// the running kernel's architecture. Foreign audit architectures (the
// profile lists the 32-bit companions) fall to the default action, like a
// runc single-arch filter.
func seccompFilter(sc *specs.LinuxSeccomp) (*seccomp.Filter, error) {
	archInfo, err := arch.GetInfo(runtime.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("seccomp arch: %w", err)
	}
	if err := validateSeccompOps(sc); err != nil {
		return nil, err
	}
	policy := seccomp.Policy{
		DefaultAction: bpfAction(sc.DefaultAction, nil),
	}

	for i := range sc.Syscalls {
		s := &sc.Syscalls[i]
		action := bpfAction(s.Action, s.ErrnoRet)
		var names []string
		for _, name := range s.Names {
			if _, ok := archInfo.SyscallNames[name]; ok {
				names = append(names, name)
			}
			// Unknown-for-arch names are skipped, matching runc/libseccomp.
		}
		if len(names) == 0 {
			continue
		}
		if len(s.Args) == 0 {
			policy.Syscalls = append(policy.Syscalls, seccomp.SyscallGroup{
				Names:  names,
				Action: action,
			})
			continue
		}
		conds := make(seccomp.ArgumentConditions, 0, len(s.Args))
		for _, arg := range s.Args {
			op, val, ok := bpfCondition(arg)
			if !ok {
				return nil, fmt.Errorf("seccomp: unsupported operator %v", arg.Op)
			}
			conds = append(conds, seccomp.Condition{
				Argument:  uint32(arg.Index),
				Operation: op,
				Value:     val,
			})
		}
		for _, name := range names {
			policy.Syscalls = append(policy.Syscalls, seccomp.SyscallGroup{
				NamesWithCondtions: []seccomp.NameWithConditions{
					{Name: name, Conditions: conds},
				},
				Action: action,
			})
		}
	}
	return &seccomp.Filter{
		Policy: policy,
	}, nil
}

// bpfAction maps OCI action strings to filter actions, carrying the
// requested errno for ActErrno: the module emits the action value raw, so
// SECCOMP_RET_ERRNO|errno reaches the kernel as-is. clone3 must answer
// ENOSYS (glibc's clone3-to-clone fallback keys on ENOSYS specifically),
// not a blanket EPERM.
func bpfAction(a specs.LinuxSeccompAction, errnoRet *uint) seccomp.Action {
	switch a {
	case specs.ActKill, specs.ActKillThread:
		return seccomp.ActionKillThread
	case specs.ActKillProcess:
		return seccomp.ActionKillProcess
	case specs.ActTrap:
		return seccomp.ActionTrap
	case specs.ActErrno:
		if errnoRet != nil {
			return seccomp.Action(uint32(seccomp.ActionErrno) | (uint32(*errnoRet) & 0xffff))
		}
		return seccomp.ActionErrno
	case specs.ActTrace:
		return seccomp.ActionTrace
	case specs.ActNotify:
		return seccomp.ActionAllow // no user notification support: allow
	case specs.ActLog:
		return seccomp.ActionLog
	default:
		return seccomp.ActionAllow
	}
}

// archForOCI resolves an OCI SCMP_ARCH_* value to filter arch info.
func archForOCI(a specs.Arch) (*arch.Info, error) {
	switch a {
	case specs.ArchX86_64, specs.ArchX32, specs.ArchX86:
		return arch.GetInfo("x86_64")
	case specs.ArchARM:
		return arch.GetInfo("arm")
	case specs.ArchAARCH64:
		return arch.GetInfo("aarch64")
	case specs.ArchMIPS:
		return arch.GetInfo("mips")
	case specs.ArchMIPS64:
		return arch.GetInfo("mips64")
	case specs.ArchMIPS64N32:
		return arch.GetInfo("mips64n32")
	case specs.ArchMIPSEL:
		return arch.GetInfo("mipsle")
	case specs.ArchMIPSEL64:
		return arch.GetInfo("mipsel64")
	case specs.ArchMIPSEL64N32:
		return arch.GetInfo("mipsel64n32")
	case specs.ArchPPC64:
		return arch.GetInfo("ppc64")
	case specs.ArchPPC64LE:
		return arch.GetInfo("ppc64le")
	case specs.ArchS390:
		return arch.GetInfo("s390")
	case specs.ArchS390X:
		return arch.GetInfo("s390x")
	}
	return nil, fmt.Errorf("unsupported seccomp arch %v", a)
}
