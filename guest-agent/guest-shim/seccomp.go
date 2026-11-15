//go:build linux

package main

import (
	"fmt"
	"runtime"
	"unsafe"

	"github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"github.com/opencontainers/runtime-spec/specs-go"
	bpf "golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
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
	insts, err := filter.Policy.Assemble()
	if err != nil {
		return fmt.Errorf("assemble seccomp policy: %w", err)
	}
	// The module's Validate only whitelists the bare ActionErrno for the
	// default action, but SECCOMP_RET_ERRNO with zero errno bits makes the
	// kernel return 0 — a faked syscall success with an uninitialized
	// result. The default deny is EPERM: rewrite the bare returns after
	// assembly (per-syscall rules carry their own errno bits already, so
	// only the bare form matches).
	for i, inst := range insts {
		if ret, ok := inst.(bpf.RetConstant); ok && ret.Val == uint32(seccomp.ActionErrno) {
			insts[i] = bpf.RetConstant{Val: uint32(seccomp.ActionErrno) | uint32(unix.EPERM)}
		}
	}
	raw, err := bpf.Assemble(insts)
	if err != nil {
		return fmt.Errorf("assemble seccomp bpf: %w", err)
	}
	sockFilter := make([]unix.SockFilter, 0, len(raw))
	for _, instruction := range raw {
		sockFilter = append(sockFilter, unix.SockFilter{
			Code: instruction.Op,
			Jt:   instruction.Jt,
			Jf:   instruction.Jf,
			K:    instruction.K,
		})
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	// prctl(PR_SET_SECCOMP, SECCOMP_SET_MODE_FILTER, &fprog, flags, 0) —
	// the same call the module's LoadFilter makes; here with the patched
	// program. TSYNC keeps every helper thread inside the same filter.
	fprog := &unix.SockFprog{
		Len:    uint16(len(sockFilter)),
		Filter: &sockFilter[0],
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		uintptr(unsafe.Pointer(fprog)), uintptr(unix.SECCOMP_FILTER_FLAG_TSYNC), 0); err != nil {
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
		DefaultAction: bpfAction(sc.DefaultAction, sc.DefaultErrnoRet),
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

// bpfAction maps OCI action strings to filter actions, carrying the errno
// for ActErrno: the module emits the action value raw, and SECCOMP_RET_ERRNO
// with zero errno bits makes the kernel return 0 — a faked success with an
// uninitialized result — so a bare ActionErrno must never be emitted. The
// OCI errno wins (clone3 must answer ENOSYS: glibc's clone3-to-clone
// fallback keys on ENOSYS specifically); without one the default is EPERM,
// matching libseccomp/runc's SCMP_ACT_ERRNO default.
func bpfAction(a specs.LinuxSeccompAction, errnoRet *uint) seccomp.Action {
	switch a {
	case specs.ActKill, specs.ActKillThread:
		return seccomp.ActionKillThread
	case specs.ActKillProcess:
		return seccomp.ActionKillProcess
	case specs.ActTrap:
		return seccomp.ActionTrap
	case specs.ActErrno:
		errno := uint32(unix.EPERM)
		if errnoRet != nil {
			errno = uint32(*errnoRet) & 0xffff
		}
		return seccomp.Action(uint32(seccomp.ActionErrno) | errno)
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
