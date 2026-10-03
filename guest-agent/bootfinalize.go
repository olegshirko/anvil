package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
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
	// Deletes of the previous session may predate this boot's timer.
	defer scheduleDiskTrim()

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

	ensureGuestZoneinfo()

	// host.docker.internal -> the Mac's localhost. Before the Docker API
	// opens, so every container's /etc/hosts gets the redirect address.
	setupHostLoopback()
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
	// Nothing runs yet: every CNI address reservation and cached result is
	// from the previous boot. Starts allocate afresh.
	os.RemoveAll("/var/lib/cni/networks") //nolint:errcheck
	os.RemoveAll("/var/lib/cni/results")  //nolint:errcheck

	kept := 0
	var toStart []containerRef
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			id := c.ID()
			// The task of a previous boot is gone with its processes.
			if task, terr := c.Task(nsCtx, nil); terr == nil {
				dctx, cancel := context.WithTimeout(nsCtx, 5*time.Second)
				task.Delete(dctx, client.WithProcessKill) //nolint:errcheck — best effort on a stuck shim
				cancel()
			}
			releaseNamedNetNS(id)
			removeNetInfo(ns, id)

			meta, merr := loadContainerMeta(ns, id)
			// Containers that are not anvil's (no metadata) and --rm ones go,
			// as Docker removes --rm containers when its daemon starts.
			if merr != nil || meta.AutoRemove {
				dctx, cancel := context.WithTimeout(nsCtx, 5*time.Second)
				if derr := c.Delete(dctx, client.WithSnapshotCleanup); derr != nil {
					debugLog("[boot] stale container %s/%s: %v", ns, truncateID(id), derr)
				}
				cancel()
				if merr == nil {
					// --rm takes its anonymous volumes along, as on removal.
					for _, v := range removableAnonymousVolumes(ns, meta) {
						os.RemoveAll(volumeDataDir(v.ns, v.name))
						os.Remove(volumeMetaPath(v.ns, v.name))
					}
				}
				deleteContainerMeta(ns, id)
				continue
			}
			// Everything else survives the reboot, as it survives a Docker
			// daemon restart. A container that was running is now exited.
			wasRunning := !meta.StartedAt.IsZero() && (meta.FinishedAt.IsZero() || meta.FinishedAt.Before(meta.StartedAt))
			if wasRunning {
				updateContainerMeta(ns, id, func(m *containerMeta) { //nolint:errcheck
					m.FinishedAt = time.Now().UTC()
					m.ExitCode = 255
				})
			}
			if rehydrateContainerState(ns, id, meta, wasRunning) {
				toStart = append(toStart, containerRef{ns: ns, id: id})
			}
			kept++
		}
	}
	log.Printf("[boot] stale tasks cleared, %d containers kept, %d to restart (%d namespaces)", kept, len(toStart), len(nss))
	if len(toStart) > 0 {
		goSafe("boot-restart", func() {
			<-bootFinalized // network and DHCP ready
			// Containers that join another's namespaces (--network/--pid/
			// --ipc container:) need it running: start them last, and give
			// whatever failed one more pass once the rest is up.
			sort.SliceStable(toStart, func(i, j int) bool {
				return !joinsAnotherContainer(toStart[i]) && joinsAnotherContainer(toStart[j])
			})
			var failed []containerRef
			for _, r := range toStart {
				if err := startDockerContainer(context.Background(), dockerID(r.ns, r.id)); err != nil {
					failed = append(failed, r)
				}
			}
			for _, r := range failed {
				if err := startDockerContainer(context.Background(), dockerID(r.ns, r.id)); err != nil {
					log.Printf("[boot] restart %s: %v", truncateID(dockerID(r.ns, r.id)), err)
				}
			}
		})
	}
}

// rehydrateContainerState rebuilds the agent's in-memory state of a
// container kept across a cold boot (it is set at create otherwise) and
// reports whether its restart policy starts it now: always does,
// unless-stopped unless a user stopped it, on-failure when the reboot cut
// it off while running.
func rehydrateContainerState(ns, id string, meta *containerMeta, wasRunning bool) bool {
	did := dockerID(ns, id)
	setContainerTTY(did, meta.TTY)
	setContainerEntryPointInfo(did, meta.WorkingDir, meta.Entrypoint)
	setContainerStopSignal(did, meta.StopSignal)
	links := meta.Links
	if len(links) == 0 && meta.HostConfig != nil {
		links = meta.HostConfig.Links // where create keeps them
	}
	setContainerLinks(did, links)
	if meta.Healthcheck != nil {
		setHealthcheckConfig(did, meta.Healthcheck, meta.User)
	}
	if meta.HostConfig == nil {
		return false
	}
	rp := meta.HostConfig.RestartPolicy
	p := parseRestartPolicy(rp.Name)
	if p.max < 0 && rp.MaximumRetryCount > 0 {
		p.max = rp.MaximumRetryCount
	}
	restarts.registerAt(ns, id, p.name, p.max)
	start := false
	switch p.name {
	case "always":
		start = true
	case "unless-stopped":
		start = !meta.UserStopped
	case "on-failure":
		start = wasRunning
	}
	if !start {
		restarts.clear(did) // keeps the spec for inspect; docker start re-arms
	}
	return start
}

// joinsAnotherContainer reports --network/--pid/--ipc container:<x>.
func joinsAnotherContainer(r containerRef) bool {
	meta, err := loadContainerMeta(r.ns, r.id)
	if err != nil {
		return false
	}
	if len(meta.Networks) > 0 && isContainerNetworkMode(meta.Networks[0]) {
		return true
	}
	return meta.HostConfig != nil &&
		(strings.HasPrefix(meta.HostConfig.PidMode, "container:") || strings.HasPrefix(meta.HostConfig.IpcMode, "container:"))
}
