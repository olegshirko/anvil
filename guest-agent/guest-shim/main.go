// The guest-side runtime is one Go package built into two binaries.
//
// anvil-fast-shim (built with -tags shimserver) is a lightweight containerd
// runtime-v2 shim. It implements the shim TaskService contract
// (Create/Start/Wait/Kill/Delete/exec/stats) without runc: at create a
// two-stage init helper joins the cgroup and the namespaces, applies the
// rootfs mounts prepared by the agent and holds at a start gate; start
// releases the gate and the init execs the user process as PID 1 of the
// container's pid namespace — one raw clone(CLONE_NEWPID), no runc in
// between.
//
// The wire protocol is the standard shim v2 one: containerd starts the
// binary with action "start" and a BootstrapParams blob on stdin; we fork
// ourselves into a long-running server that serves the ttrpc TaskService on
// a unix socket under /run/containerd/s and answer with a BootstrapResult.
// The agent selects this binary per container via containerd's WithRuntime
// (an absolute binary path is accepted as the runtime name).
//
// anvil-init is the same package built without the tag: it carries only the
// per-container helper modes below (init, init-apply, exec-init, exec-apply
// and the secprobe diagnostic) and is what the agent spawns on the container
// start hot path — once per container, so it must not carry the ttrpc server,
// the containerd API or the plugin graph. UPX was measured and rejected:
// packing the full binary to 4.8 MB costs +30 ms per exec, and the initramfs
// is already zstd-compressed.
//
// Linux-only at runtime (raw clone, namespaces, cgroup v2); the process
// scaffolding here is portable so the package still builds for darwin
// (go vet / go test on the dev Mac).
package main

import "os"

func main() {
	// The helper processes exec this binary before the user process starts;
	// they bypass the shim server flow entirely.
	if isInitHelper() {
		os.Exit(runInit(helperArg("id"), helperArg("bundle"), helperFlag("cgroup")))
	}
	if isInitApply() {
		os.Exit(runInitApply(helperArg("id"), helperArg("bundle")))
	}
	if isExecHelper() {
		os.Exit(runExecHelper(helperArg("initpid"), helperArg("bundle")))
	}
	if isExecApply() {
		os.Exit(runExecApply())
	}
	if len(os.Args) >= 2 && os.Args[1] == "secprobe" {
		os.Exit(runSecProbe(os.Args[2:]))
	}
	os.Exit(runShimServer())
}

// helperArg reads "-<name> value" from the helper invocation argv.
func helperArg(name string) string {
	args := os.Args[2:]
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "-"+name {
			return args[i+1]
		}
	}
	return ""
}

// helperFlag reports whether "-<name>" is present in the helper argv (no
// value). It is scanned independently of helperArg's pairs so flags can be
// appended after the valued arguments.
func helperFlag(name string) bool {
	for _, a := range os.Args[2:] {
		if a == "-"+name {
			return true
		}
	}
	return false
}
