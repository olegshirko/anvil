package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// bootFinalized closes when boot finalize completes: containerd reachable,
// stale-container cleanup done and a default network route present. The
// status/health control path never waits on it (daemon readiness is honest
// but does not block on it); exec'd commands and the Docker API server do,
// bounded, so early requests cannot race a boot still in flight.
var bootFinalized = make(chan struct{})

// runBootFinalize performs the boot tail that stage2 used to run before
// exec'ing the agent: waiting for the containerd socket, removing stale
// container metadata left by an unclean shutdown, and waiting for the DHCP
// lease (stage2 obtains it in the background, off the boot critical path).
// Moved here so the control channel (vsock:1024) comes up immediately;
// closes bootFinalized when done, which gates the Docker API server and
// exec'd commands.
func runBootFinalize() {
	defer close(bootFinalized)

	// Kill unreachable shims from a crashed previous session first — trying
	// to delete their tasks through containerd would hang.
	if out, err := exec.Command("/bin/sh", "-c", `
killed=0
killall -9 containerd-shim-runc-v2 2>/dev/null && killed=1
killall -9 runc 2>/dev/null && killed=1
[ "$killed" = 1 ] && sleep 1
`).CombinedOutput(); err != nil {
		log.Printf("[boot] stale process kill failed: %v: %s", err, out)
	}

	const sock = "/run/containerd/containerd.sock"
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("[boot] containerd socket did not appear, skipping stale cleanup")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	cleanupStaleContainers()
	pruneStaleContainerMeta()

	// Wait for the DHCP lease: pulls and DNS through the NAT gateway fail
	// with "network is unreachable" until a default route exists, and image
	// pulls retry until then, turning a hiccup into minutes-long hangs.
	deadline = time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command("ip", "route", "show", "default").Output()
		if err == nil && len(bytes.Fields(out)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("[boot] no default route after 10s, proceeding without network")
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cleanupStaleContainers deletes every task and container record left over
// from a previous session (the VM disk persists across cold boots). Doing it
// through the containerd Go client avoids any external tool dependency.
func cleanupStaleContainers() {
	ctx := context.Background()
	cl, err := pc.get(ctx)
	if err != nil {
		log.Printf("[boot] stale container cleanup skipped: %v", err)
		return
	}
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		log.Printf("[boot] stale container cleanup skipped: %v", err)
		return
	}
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			id := c.ID()
			if task, terr := c.Task(nsCtx, nil); terr == nil {
				dctx, cancel := context.WithTimeout(nsCtx, 5*time.Second)
				task.Delete(dctx) //nolint:errcheck — best effort on a stuck shim
				cancel()
			}
			dctx, cancel := context.WithTimeout(nsCtx, 5*time.Second)
			if derr := c.Delete(dctx); derr != nil {
				debugLog("[boot] stale container %s/%s: %v", ns, truncateID(id), derr)
			}
			cancel()
			releaseNamedNetNS(id)
			deleteContainerMeta(ns, id)
		}
	}
	log.Printf("[boot] stale container cleanup done (%d namespaces)", len(nss))
}
