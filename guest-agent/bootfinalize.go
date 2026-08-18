package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"time"
)

// bootFinalized closes when boot finalize completes: containerd reachable,
// stale-container cleanup done and a default network route present. The
// status/health control path never waits on it (daemon readiness is honest
// but does not block on it); exec'd commands and the Docker API server do,
// bounded, so early nerdctl invocations cannot race a boot still in flight.
var bootFinalized = make(chan struct{})

// runBootFinalize performs the boot tail that stage2 used to run before exec'ing
// the agent: waiting for the containerd socket, removing stale container
// metadata left by an unclean shutdown, and waiting for the DHCP lease (stage2
// obtains it in the background, off the boot critical path). Moved here so the
// control channel (vsock:1024) comes up immediately; closes bootFinalized when
// done, which gates the Docker API server and exec'd commands.
func runBootFinalize() {
	defer close(bootFinalized)

	// Kill unreachable shims/hooks from a crashed previous session first —
	// trying to delete their tasks through containerd would hang, and any
	// exec'd nerdctl that arrives later must not be reaped by this killall.
	// It runs ~immediately at agent start, before vsock:1024 listens.
	if out, err := exec.Command("/bin/sh", "-c", `
killed=0
killall -9 containerd-shim-runc-v2 2>/dev/null && killed=1
killall -9 runc 2>/dev/null && killed=1
killall -9 nerdctl 2>/dev/null && killed=1
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

	// Drop containers through the low-level ctr client so a stuck OCI hook
	// cannot block the boot, then purge nerdctl's name-store files that
	// ctr rm leaves behind (they block docker compose from reusing names).
	script := `
for ns in $(/opt/containerd/bin/ctr namespace ls -q 2>/dev/null); do
    for id in $(/opt/containerd/bin/ctr -n "$ns" c ls -q 2>/dev/null); do
        /opt/containerd/bin/ctr -n "$ns" t rm -f "$id" >/dev/null 2>&1 || true
        /opt/containerd/bin/ctr -n "$ns" c rm "$id" >/dev/null 2>&1 || true
    done
done

find /var/lib/nerdctl -mindepth 4 -maxdepth 4 -type f 2>/dev/null | while read f; do
    rm -f "$f"
done
`
	if out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput(); err != nil {
		log.Printf("[boot] stale container cleanup failed: %v: %s", err, out)
	} else {
		log.Printf("[boot] stale container cleanup done")
	}

	// Wait for the DHCP lease: pulls and DNS through the NAT gateway fail
	// with "network is unreachable" until a default route exists, and
	// nerdctl's internal pull retries turn that into minutes-long hangs.
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
