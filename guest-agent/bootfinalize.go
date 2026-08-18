package main

import (
	"log"
	"os"
	"os/exec"
	"time"
)

// runBootFinalize performs the boot tail that stage2 used to run before exec'ing
// the agent: waiting for the containerd socket and removing stale container
// metadata left by an unclean shutdown. Moved here so the control channel
// (vsock:1024) comes up immediately; closes done once containerd is reachable
// and cleanup has run, which gates the Docker API server.
func runBootFinalize(done chan<- struct{}) {
	defer close(done)

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
}
