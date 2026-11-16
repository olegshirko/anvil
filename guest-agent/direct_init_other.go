//go:build !linux

package main

import (
	"os"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// The native init helper is a Linux ELF binary; the darwin build of the agent
// exists only for unit tests and vet, so it always keeps the Go helper.
func newDirectInitCommand(id, bundle string, spec *specs.Spec) (argv []string, cgroupFile *os.File, native bool, err error) {
	return []string{fastShimHelperName, "init", "-id", id, "-bundle", bundle, "-cgroup"}, nil, false, nil
}
