package main

// Alias resolution for OCI seccomp names against the module's per-arch
// syscall tables (pure table work; runs on darwin like seccomp_cond_test).

import (
	"testing"

	"github.com/elastic/go-seccomp-bpf/arch"
)

// The docker default profile allowlists "newfstatat" (libseccomp's name for
// the generic-table syscall 79); the module's aarch64 table only knows
// "fstatat". Without the alias the syscall falls to the default deny and
// stat()-class wrappers fail EPERM container-wide.
func TestResolveSeccompNameAliasesNewfstatatOnAARCH64(t *testing.T) {
	info, err := arch.GetInfo("aarch64")
	if err != nil {
		t.Fatalf("aarch64 table: %v", err)
	}
	if got := resolveSeccompName("newfstatat", info); got != "fstatat" {
		t.Fatalf("newfstatat on aarch64: resolved %q, want fstatat", got)
	}
	if num := info.SyscallNames["fstatat"]; num != 79 {
		t.Fatalf("fstatat number on aarch64: %d, want 79", num)
	}
}

// Known names pass through unchanged.
func TestResolveSeccompNamePassesThroughKnown(t *testing.T) {
	info, err := arch.GetInfo("aarch64")
	if err != nil {
		t.Fatalf("aarch64 table: %v", err)
	}
	for _, name := range []string{"openat", "getdents64", "execve", "fstatat"} {
		if got := resolveSeccompName(name, info); got != name {
			t.Fatalf("%s: resolved %q, want unchanged", name, got)
		}
	}
}

// Names no table knows stay skipped (32-bit companions on aarch64), alias or
// not: the alias must not invent numbers foreign to the arch.
func TestResolveSeccompNameSkipsUnknown(t *testing.T) {
	info, err := arch.GetInfo("aarch64")
	if err != nil {
		t.Fatalf("aarch64 table: %v", err)
	}
	for _, name := range []string{"open", "lstat", "getdents", "dup2", "flibberty"} {
		if got := resolveSeccompName(name, info); got != "" {
			t.Fatalf("%s on aarch64: resolved %q, want empty", name, got)
		}
	}
}

// resolveSeccompNames collapses an OCI name list the way seccompFilter
// consumes it: aliasing, dedup (newfstatat + fstatat in one profile group
// would otherwise duplicate a table name and the assembler rejects that),
// skipping unknown-for-arch names, preserving order otherwise.
func TestResolveSeccompNamesList(t *testing.T) {
	info, err := arch.GetInfo("aarch64")
	if err != nil {
		t.Fatalf("aarch64 table: %v", err)
	}
	got := resolveSeccompNames([]string{"newfstatat", "openat", "bogus", "fstatat", "getdents64", "newfstatat"}, info)
	want := []string{"fstatat", "openat", "getdents64"}
	if len(got) != len(want) {
		t.Fatalf("resolved %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolved %v, want %v", got, want)
		}
	}
}
