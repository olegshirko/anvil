//go:build !shimserver

package main

import (
	"fmt"
	"os"
)

// runShimServer answers a non-helper invocation of the helper-only binary
// (anvil-init). Only /bin/anvil-fast-shim is a containerd runtime, so
// reaching this point means the wrong binary was selected or the caller
// passed an unknown mode.
func runShimServer() int {
	fmt.Fprintf(os.Stderr,
		"%s: helper-only build (init, init-apply, exec-init, exec-apply, secprobe); the containerd shim server is /bin/anvil-fast-shim\n",
		os.Args[0])
	if len(os.Args) >= 2 {
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", os.Args[1])
	}
	return 1
}
