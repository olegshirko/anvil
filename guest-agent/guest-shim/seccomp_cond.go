package main

import (
	"fmt"

	"github.com/elastic/go-seccomp-bpf"
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
