package main

import (
	"fmt"

	"github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// OCI-to-BPF seccomp condition mapping. Portable (pure table work), so the
// tests run on the dev Mac; the linux-tagged seccomp.go assembles the filter.

// defaultPolicyAction maps the OCI default action to the module's bare
// action value. ActErrno maps to the bare ActionErrno (no errno bits): the
// module's Validate whitelists action values, and applySeccomp rewrites the
// assembled bare returns to EPERM — which is the OCI default errno anyway
// (the default profile carries no per-default errno of its own).
func defaultPolicyAction(sc *specs.LinuxSeccomp) seccomp.Action {
	switch sc.DefaultAction {
	case specs.ActKill, specs.ActKillThread:
		return seccomp.ActionKillThread
	case specs.ActKillProcess:
		return seccomp.ActionKillProcess
	case specs.ActTrap:
		return seccomp.ActionTrap
	case specs.ActErrno:
		return seccomp.ActionErrno
	case specs.ActTrace:
		return seccomp.ActionTrace
	case specs.ActLog:
		return seccomp.ActionLog
	default:
		return seccomp.ActionAllow
	}
}

// validateSeccompOps rejects profiles with argument operators the condition
// converter cannot express faithfully (SCMP_CMP_MASKED_EQ outside its two
// expressible shapes); the agent's eligibility excludes custom profiles, so
// this only ever fires on a misconfiguration — and Create failing beats
// silently allowing syscalls the profile intended to restrict.
func validateSeccompOps(sc *specs.LinuxSeccomp) error {
	for i := range sc.Syscalls {
		for _, arg := range sc.Syscalls[i].Args {
			if _, _, ok := bpfCondition(arg); !ok {
				return fmt.Errorf("seccomp: unsupported operator %v for %s", arg.Op, sc.Syscalls[i].Names)
			}
		}
	}
	return nil
}

// seccompNameAliases maps OCI syscall names to the alias the module's
// syscall table actually knows, for names libseccomp resolves but
// go-seccomp-bpf names differently on the same architecture. The one
// collision that matters: the kernel's generic aarch64 table calls syscall
// 79 "fstatat", libseccomp (and the OCI profiles, and every docker default
// profile) call it "newfstatat" — dropped as unknown-for-arch, the syscall
// then falls to the default deny and stat()/lstat() wrappers (musl: ls,
// shell PATH lookup) fail EPERM across the whole container. The alias only
// applies when the target name exists for the arch, so foreign arch tables
// are untouched.
var seccompNameAliases = map[string]string{
	"newfstatat": "fstatat",
}

// resolveSeccompName maps one OCI syscall name to the module's table name
// for the architecture. Known names pass through; unknown ones try the
// alias table (and must then exist for the arch too). Returns the resolved
// name (or "" when the name has no number for this arch and must be
// skipped, matching runc/libseccomp's foreign-arch behavior).
func resolveSeccompName(name string, archInfo *arch.Info) string {
	if _, ok := archInfo.SyscallNames[name]; ok {
		return name
	}
	if alias, ok := seccompNameAliases[name]; ok {
		if _, ok := archInfo.SyscallNames[alias]; ok {
			return alias
		}
	}
	return ""
}

// resolveSeccompNames resolves a whole OCI name list: aliasing, duplicate
// collapse (aliasing can land two OCI names on one table name and the
// assembler rejects duplicates), order preserved.
func resolveSeccompNames(names []string, archInfo *arch.Info) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range names {
		resolved := resolveSeccompName(name, archInfo)
		if resolved == "" || seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	return out
}

// bpfCondition maps one OCI argument comparison to the filter's condition.
// SCMP_CMP_MASKED_EQ — (arg & mask) == valuetwo — is the operator the
// containerd default profile carries on clone (allow only when none of the
// namespace-creation bits are set), and its two common shapes are expressible
// with the module's bit conditions: valuetwo == 0 becomes BitsNotSet(mask),
// and valuetwo == mask collapses to BitsSet(mask) when the mask is a single
// bit. Any other masked shape is not expressible without an ALU-AND in the
// assembler and is rejected (the runc shim takes over through the agent's
// fallback).
func bpfCondition(arg specs.LinuxSeccompArg) (seccomp.Operation, uint64, bool) {
	switch arg.Op {
	case specs.OpEqualTo:
		return seccomp.Equal, arg.Value, true
	case specs.OpNotEqual:
		return seccomp.NotEqual, arg.Value, true
	case specs.OpLessThan:
		return seccomp.LessThan, arg.Value, true
	case specs.OpLessEqual:
		return seccomp.LessOrEqual, arg.Value, true
	case specs.OpGreaterThan:
		return seccomp.GreaterThan, arg.Value, true
	case specs.OpGreaterEqual:
		return seccomp.GreaterOrEqual, arg.Value, true
	case specs.OpMaskedEqual:
		if arg.ValueTwo == 0 {
			return seccomp.BitsNotSet, arg.Value, true
		}
		if arg.ValueTwo == arg.Value && arg.Value != 0 && arg.Value&(arg.Value-1) == 0 {
			return seccomp.BitsSet, arg.Value, true
		}
	}
	return "", 0, false
}
