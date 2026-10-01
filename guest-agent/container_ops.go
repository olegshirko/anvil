package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"net/url"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Native task lifecycle: start (CNI attach + json-file logging), stop
// (signal escalation), delete (full cleanup) and a one-shot exec primitive.
// Companion to container_runtime.go.

// --- runtime state ----------------------------------------------------------

// containerNetInfo is the persisted CNI result for a running container.
type containerNetInfo struct {
	IP      string `json:"IP"`
	Mac     string `json:"Mac,omitempty"`
	Network string `json:"Network"`
	// Extra holds the secondary endpoints (eth1...), see container_networks.go.
	Extra []netEndpoint `json:"Extra,omitempty"`
}

func saveNetInfo(ns, id string, ni containerNetInfo) error {
	data, err := json.Marshal(ni)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(containerMetaDir(ns, id), "net.json"), data, 0o644)
}

func loadNetInfo(ns, id string) (containerNetInfo, bool) {
	data, err := os.ReadFile(filepath.Join(containerMetaDir(ns, id), "net.json"))
	if err != nil {
		return containerNetInfo{}, false
	}
	var ni containerNetInfo
	if json.Unmarshal(data, &ni) != nil {
		return containerNetInfo{}, false
	}
	return ni, true
}

// netInfoMu makes claimNetInfo atomic; releasing tracks claimed records
// whose detach is still running, so stop and delete can wait for it.
var (
	netInfoMu sync.Mutex
	releasing = map[string]chan struct{}{}
)

// claimNetInfo takes a container's live endpoint record for teardown:
// stop, the exit watcher and delete all race to detach a stopped
// container's network, and every CNI DEL costs a few iptables runs — with a
// dozen containers going down at once (compose down) the duplicates made
// up most of the wait. Only the caller that claims the record detaches.
func claimNetInfo(ns, id string) (containerNetInfo, bool) {
	netInfoMu.Lock()
	defer netInfoMu.Unlock()
	ni, ok := loadNetInfo(ns, id)
	if ok {
		removeNetInfo(ns, id)
		releasing[ns+"/"+id] = make(chan struct{})
	}
	return ni, ok
}

// awaitNetRelease blocks until a detach another caller claimed is done: a
// stop must not return (and a restart attach) while the veth still exists.
func awaitNetRelease(ns, id string) {
	netInfoMu.Lock()
	ch := releasing[ns+"/"+id]
	netInfoMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
	}
}

func finishNetRelease(ns, id string) {
	netInfoMu.Lock()
	defer netInfoMu.Unlock()
	if ch := releasing[ns+"/"+id]; ch != nil {
		close(ch)
		delete(releasing, ns+"/"+id)
	}
}

// releaseEndpoints detaches a claimed endpoint record (secondaries first)
// and publishes the network disconnect events.
func releaseEndpoints(ctx context.Context, ns, id string, ni containerNetInfo, ports []cniPortMapping) {
	defer finishNetRelease(ns, id)
	if usesHostNetworkName(ni.Network) {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Destroy the netns first: the kernel then tears the veths down on its
	// own, and the bridge plugin's DEL skips the synchronous in-netns link
	// removal (~240 ms each, serialized on the RTNL across a compose down).
	// Only where the bridge plugin does not masquerade per container — it
	// would skip removing that chain too. start recreates the netns.
	fast := !networkUsesPluginMasq(ni.Network)
	for _, e := range ni.Extra {
		fast = fast && !networkUsesPluginMasq(e.Network)
	}
	// Containers that joined this one's netns (--network container:) keep
	// it alive: the kernel would not remove the veth, so it must be the
	// plugin's DEL.
	fast = fast && !hasNetworkJoiners(ns, id)
	if fast {
		releaseNamedNetNS(id)
	}
	detachSecondaryNetworks(dctx, id, ni.Extra)
	start := time.Now()
	if err := detachNetwork(dctx, ni.Network, ns, id, netnsPathFor(id), ports); err != nil {
		debugLog("[cni] detach %s/%s: %v", ns, truncateID(id), err)
	}
	debugLog("[cni] detach %s from %s took %v", truncateID(id), ni.Network, time.Since(start))
	publishEndpointEvents("disconnect", ns, id, ni.Network, ni.Extra)
	// The endpoint is gone — drop the container's name/aliases from the
	// hosts files of its network peers, whichever path released it.
	refreshHostsForContainer(ns, id)
}

func removeNetInfo(ns, id string) {
	os.Remove(filepath.Join(containerMetaDir(ns, id), "net.json"))
}

// usesHostNetworkName reports whether a logical network name means
// host-networking (no CNI attachment).
func usesHostNetworkName(netName string) bool {
	return netName == "" || netName == "host" || isContainerNetworkMode(netName)
}

// --- start ------------------------------------------------------------------

// taskRuns numbers the runs of each container. startNativeTask begins a new
// run and hands its number to the exit watcher; the watcher only applies its
// teardown while its run is still the current one. Without this, the watcher
// of the previous run (docker restart, the restart policy) can fire after the
// next start and cache a stale exit code, stop the new health monitor or
// detach the new network.
var taskRuns = struct {
	sync.Mutex
	gen map[string]uint64
}{gen: make(map[string]uint64)}

func beginTaskRun(did string) uint64 {
	taskRuns.Lock()
	defer taskRuns.Unlock()
	taskRuns.gen[did]++
	return taskRuns.gen[did]
}

func isCurrentTaskRun(did string, run uint64) bool {
	taskRuns.Lock()
	defer taskRuns.Unlock()
	return taskRuns.gen[did] == run
}

func forgetTaskRuns(did string) {
	taskRuns.Lock()
	delete(taskRuns.gen, did)
	taskRuns.Unlock()
}

// startNativeTask attaches CNI networking, creates the task with json-file
// logging and starts it. A leftover stopped task from a previous run is
// deleted first.
func startNativeTask(ctx context.Context, ns, id string) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)

	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return fmt.Errorf("load container: %w", err)
	}

	// A stopped task left over from a previous run/restart must go first.
	if old, terr := c.Task(nsCtx, nil); terr == nil {
		if st, serr := old.Status(nsCtx); serr == nil && st.Status == "running" {
			return errNotModified("container is already running")
		}
		dctx, cancel := context.WithTimeout(nsCtx, 5*time.Second)
		old.Delete(dctx, client.WithProcessKill) //nolint:errcheck
		cancel()
	}

	meta, merr := loadContainerMeta(ns, id)
	netName := ""
	var ports []cniPortMapping
	if merr == nil && len(meta.Networks) > 0 {
		netName = meta.Networks[0]
		ports = meta.Ports
	}

	if !usesHostNetworkName(netName) {
		// A stop/exit detach of the previous run may still be going: its
		// CNI DEL shares this container's cache and IPAM keys with the ADD
		// below. Then recreate the netns that detach released.
		awaitNetRelease(ns, id)
		if nerr := ensureNamedNetNS(id); nerr != nil {
			return fmt.Errorf("netns: %w", nerr)
		}
		// Self-healing attach: the exit watcher's CNI detach is asynchronous,
		// so a restart (the monitor, docker restart, compose) can race it and
		// hit a leftover veth ("eth0 peer already exists") or a torn-down
		// iptables chain ("No chain/target/match by that name"). On any
		// attach failure, run the matching DEL to clear the stale endpoint
		// state and attach again.
		ip, mac, aerr := attachNetwork(ctx, netName, ns, id, netnsPathFor(id), ports)
		if aerr != nil {
			time.Sleep(200 * time.Millisecond)
			dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
			detachNetwork(dctx, netName, ns, id, netnsPathFor(id), ports) //nolint:errcheck
			dcancel()
			time.Sleep(100 * time.Millisecond)
			ip, mac, aerr = attachNetwork(ctx, netName, ns, id, netnsPathFor(id), ports)
		}
		if aerr != nil {
			return fmt.Errorf("cni attach: %w", aerr)
		}
		extra, xerr := attachSecondaryNetworks(ctx, ns, id, meta.Networks[1:])
		if xerr != nil {
			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			detachNetwork(dctx, netName, ns, id, netnsPathFor(id), ports) //nolint:errcheck
			cancel()
			return fmt.Errorf("cni attach: %w", xerr)
		}
		saveNetInfo(ns, id, containerNetInfo{IP: ip, Mac: mac, Network: netName, Extra: extra})
		publishEndpointEvents("connect", ns, id, netName, extra)
		defer func() {
			if err != nil {
				dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				detachSecondaryNetworks(dctx, id, extra)
				detachNetwork(dctx, netName, ns, id, netnsPathFor(id), ports) //nolint:errcheck
				cancel()
				removeNetInfo(ns, id)
			}
		}()
	}

	// A new run: the previous run's exit code must not answer /wait for
	// this one.
	did := dockerID(ns, id)
	run := beginTaskRun(did)
	takeContainerExitCode(did)

	rot := logRotation{maxSize: defaultLogMaxSize, maxFile: defaultLogMaxFile}
	if meta, merr := loadContainerMeta(ns, id); merr == nil && meta.HostConfig != nil {
		if r, rerr := logRotationFor(meta.HostConfig.LogConfig.Config); rerr == nil {
			rot = r
		}
	}
	uri, lerr := taskLogURI(containerLogPath(ns, id), rot)
	if lerr != nil {
		err = lerr
		return err
	}
	// TTY tasks need Terminal set in the IO config: the shim then allocates
	// the pty itself (runc refuses a terminal spec with no console socket
	// otherwise) and duplicates the console output into the log URI.
	if meta != nil {
		if verr := verifySubpathMounts(meta); verr != nil {
			err = verr
			return err
		}
	}
	if meta != nil && meta.HostConfig != nil {
		if jerr := joinContainerNamespaces(nsCtx, c, meta.HostConfig.PidMode, meta.HostConfig.IpcMode); jerr != nil {
			err = jerr
			return err
		}
	}
	tty := getContainerTTY(dockerID(ns, id))
	var taskIO cio.IO = &logOnlyIO{uri: uri, terminal: tty}
	var stopLogger func()
	var stdin *containerStdin
	if meta != nil && meta.OpenStdin {
		cs, serr := openContainerStdin(ns, id, true)
		if serr != nil {
			err = serr
			return err
		}
		stdin = cs
		cs.taskStarting()
		if tty {
			// The shim copies a stdin FIFO into the console even with the
			// binary logger.
			taskIO = &logOnlyIO{uri: uri, terminal: true, stdin: cs.path}
		} else {
			// Without a console the shim wires stdin only in FIFO mode
			// (its binary logger IO has no stdin): run the task on FIFOs
			// and the json logger on their read ends ourselves.
			fio, stop, ferr := startFifoLogger(ns, id, containerLogPath(ns, id), rot, cs.path)
			if ferr != nil {
				err = ferr
				return err
			}
			taskIO, stopLogger = fio, stop
		}
	}
	task, terr := c.NewTask(nsCtx, func(string) (cio.IO, error) {
		return taskIO, nil
	})
	if terr != nil {
		if stopLogger != nil {
			stopLogger()
		}
		err = fmt.Errorf("new task: %w", terr)
		return err
	}
	if stdin != nil {
		stdin.taskCreated() // the shim holds the FIFO now
	}

	if serr := task.Start(nsCtx); serr != nil {
		task.Delete(context.WithoutCancel(nsCtx)) //nolint:errcheck — namespaced: a bare context is rejected and leaks the record
		err = fmt.Errorf("start task: %w", serr)
		return err
	}
	updateContainerMeta(ns, id, func(m *containerMeta) { //nolint:errcheck
		m.StartedAt, m.FinishedAt = time.Now().UTC(), time.Time{}
	})

	go watchTaskExit(context.Background(), ns, id, netName, ports, run)
	// Re-attach the health monitor on every start: docker start, docker
	// restart and the restart policy all come through here.
	if hc := getHealthcheckConfig(did); hc != nil {
		startHealthCheck(did, ns, id, hc, getHealthcheckUser(did))
	}
	return nil
}

// watchTaskExit waits for the task to die and performs teardown Docker
// semantics expect: exit-code cache, health monitor stop and CNI detach (the
// IP is released on stop, exactly like docker stop).
func watchTaskExit(ctx context.Context, ns, id, netName string, ports []cniPortMapping, run uint64) {
	cl, err := pc.get(ctx)
	if err != nil {
		return
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		return
	}
	code, werr := waitTaskExit(nsCtx, task)
	if werr != nil {
		debugLog("[runtime] wait error for %s: %v", truncateID(id), werr)
		return
	}

	did := dockerID(ns, id)
	if !isCurrentTaskRun(did, run) {
		// The container was started again before this watcher woke up; the
		// new run owns the exit code, the health monitor and the network.
		debugLog("[runtime] task %s/%s exited code=%d (superseded run)", ns, truncateID(id), code)
		return
	}
	// docker kill caches the mapped 137 before the task dies; containerd
	// often reports 0 for signal deaths, so do not overwrite it.
	final := code
	if cached, ok := peekContainerExitCode(did); !ok || code != 0 || cached == 0 {
		cacheContainerExitCode(did, code)
	} else {
		final = cached
	}
	updateContainerMeta(ns, id, func(m *containerMeta) { //nolint:errcheck
		m.FinishedAt, m.ExitCode = time.Now().UTC(), final
	})
	stopHealthCheck(did)

	if !usesHostNetworkName(netName) {
		if ni, ok := claimNetInfo(ns, id); ok {
			releaseEndpoints(ctx, ns, id, ni, ports)
		}
	}
	debugLog("[runtime] task %s/%s exited code=%d", ns, truncateID(id), code)
}

// waitTaskExit blocks until task has really exited and returns its exit
// code. A broken wait stream (containerd restarting, a dropped connection)
// is not an exit: the task is re-checked and waited on again while it runs.
// Treating it as one made a running container look exited — its network
// torn down, its exit code cached as 0, an --rm container deleted. Only
// ctx ending stops the wait early.
func waitTaskExit(ctx context.Context, task client.Task) (int, error) {
	for {
		exitCh, werr := task.Wait(ctx)
		if werr == nil {
			st := <-exitCh
			if st.Error() == nil {
				return int(st.ExitCode()), nil
			}
			werr = st.Error()
		}
		if ctx.Err() != nil {
			return 0, fmt.Errorf("wait aborted: %w", werr)
		}
		if exited, code := taskHasExited(ctx, task); exited {
			return code, nil
		}
		debugLog("[runtime] wait stream for %s broke (%v); task still running, waiting again", truncateID(task.ID()), werr)
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait aborted: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// taskHasExited reports whether task is gone or stopped, with its exit code.
// An unreachable containerd counts as not exited (the caller waits again).
func taskHasExited(ctx context.Context, task client.Task) (bool, int) {
	st, err := task.Status(ctx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return true, 0
		}
		return false, 0
	}
	switch st.Status {
	case client.Running, client.Paused, client.Pausing, client.Created:
		return false, 0
	}
	return true, int(st.ExitStatus)
}

// --- stop -------------------------------------------------------------------

// stopNativeTask sends the configured stop signal and escalates to SIGKILL
// after the grace timeout. The (stopped) task record is kept for status
// reporting until the container is started again or deleted.
// stopNativeTask stops the task with signal (docker stop -s), else the
// container's stop signal, else SIGTERM; SIGKILL after timeoutSec.
func stopNativeTask(ctx context.Context, ns, id string, timeoutSec int, signal string) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return err
	}
	task, terr := c.Task(nsCtx, nil)
	if terr != nil {
		teardownNetwork(ctx, ns, id)
		return nil // no task: nothing to stop
	}
	st, serr := task.Status(nsCtx)
	if serr != nil {
		return serr
	}
	if st.Status != "running" && st.Status != "paused" {
		teardownNetwork(ctx, ns, id)
		return nil
	}

	sig := syscall.SIGTERM
	if meta, merr := loadContainerMeta(ns, id); merr == nil && meta.StopSignal != "" {
		if s, ok := signalValue(meta.StopSignal); ok {
			sig = s
		}
	}
	if signal != "" {
		if s, ok := signalValue(signal); ok {
			sig = s
		}
	}
	publishContainerEvent("kill", ns, id, map[string]string{"signal": strconv.Itoa(int(sig))}) // as Docker's stop
	if kerr := task.Kill(nsCtx, sig); kerr != nil {
		return kerr
	}
	// Docker: -t 0 kills at once, -t -1 waits for as long as it takes.
	// (0 used to be read as "default", so `stop -t 0`, `rm -f` of a
	// signal-ignoring process and `compose down -t 0` waited 10 s.)
	var deadline time.Time
	switch {
	case timeoutSec < 0:
		deadline = time.Now().Add(100 * 365 * 24 * time.Hour)
	default:
		deadline = time.Now().Add(time.Duration(timeoutSec) * time.Second)
	}
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		cur, err := task.Status(nsCtx)
		if err != nil {
			break // task gone
		}
		if cur.Status == "stopped" {
			break
		}
	}
	if stopped, serr := task.Status(nsCtx); serr == nil && stopped.Status != "stopped" {
		kctx, cancel := context.WithTimeout(nsCtx, 5*time.Second)
		publishContainerEvent("kill", ns, id, map[string]string{"signal": strconv.Itoa(int(syscall.SIGKILL))})
		if kerr := task.Kill(kctx, syscall.SIGKILL); kerr != nil {
			cancel()
			return fmt.Errorf("force kill: %w", kerr)
		}
		cancel()
		deadline = time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			cur, err := task.Status(nsCtx)
			if err != nil || cur.Status == "stopped" {
				break
			}
		}
	}
	// Detach CNI synchronously: the exit watcher may fire later, and a
	// leftover host-side veth makes the next start's CNI attach fail with
	// "veth ... already exists" (docker restart / compose restart).
	teardownNetwork(ctx, ns, id)
	return nil
}

// teardownNetwork detaches the container's CNI endpoint if one is recorded.
// Best effort and idempotent (a late exit-watcher detach on an already
// detached endpoint is harmless).
func teardownNetwork(ctx context.Context, ns, id string) {
	ni, ok := claimNetInfo(ns, id)
	if !ok {
		awaitNetRelease(ns, id) // the exit watcher got there first
		return
	}
	if usesHostNetworkName(ni.Network) {
		finishNetRelease(ns, id)
		return
	}
	meta, _ := loadContainerMeta(ns, id)
	var ports []cniPortMapping
	if meta != nil {
		ports = meta.Ports
	}
	releaseEndpoints(ctx, ns, id, ni, ports)
}

// logOnlyIO mirrors cio.LogURI but can flag the IO config as a terminal,
// which the stock LogURI creator cannot do.
type logOnlyIO struct {
	uri      *url.URL
	terminal bool
	stdin    string // FIFO path for an OpenStdin container, else empty
}

func (l *logOnlyIO) Config() cio.Config {
	return cio.Config{
		Terminal: l.terminal,
		Stdin:    l.stdin,
		Stdout:   l.uri.String(),
		Stderr:   l.uri.String(),
	}
}

func (l *logOnlyIO) Cancel()      {}
func (l *logOnlyIO) Wait()        {}
func (l *logOnlyIO) Close() error { return nil }

// --- delete -----------------------------------------------------------------

// deleteNativeContainer removes the task (if any), the container record, its
// rootfs snapshot, CNI attachment, named netns and anvil metadata. Anonymous
// volumes are removed with the container (--rm semantics).
func deleteNativeContainer(ctx context.Context, ns, id string, force, removeVolumes bool) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)

	meta, _ := loadContainerMeta(ns, id)

	if c, cerr := cl.LoadContainer(nsCtx, id); cerr == nil {
		if task, terr := c.Task(nsCtx, nil); terr == nil {
			if st, serr := task.Status(nsCtx); serr == nil && st.Status == "running" {
				if !force {
					return errConflict("cannot remove container %q: container is running: stop the container before removing or force remove", truncateID(dockerID(ns, id)))
				}
				kctx, kcancel := context.WithTimeout(nsCtx, 5*time.Second)
				task.Kill(kctx, syscall.SIGKILL) //nolint:errcheck
				kcancel()
				waitDeadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(waitDeadline) {
					time.Sleep(50 * time.Millisecond)
					if cur, err := task.Status(nsCtx); err != nil || cur.Status == "stopped" {
						break
					}
				}
			}
			dctx, dcancel := context.WithTimeout(nsCtx, 5*time.Second)
			task.Delete(dctx) //nolint:errcheck — best effort; record removal proceeds
			dcancel()
		}
		rmctx, rmcancel := context.WithTimeout(nsCtx, 10*time.Second)
		if derr := c.Delete(rmctx, client.WithSnapshotCleanup); derr != nil {
			rmcancel()
			return fmt.Errorf("delete container: %w", derr)
		}
		rmcancel()
	}

	// CNI teardown if the exit watcher lost the race (or never ran).
	if ni, ok := claimNetInfo(ns, id); ok {
		var ports []cniPortMapping
		if meta != nil {
			ports = meta.Ports
		}
		releaseEndpoints(ctx, ns, id, ni, ports)
	} else {
		awaitNetRelease(ns, id) // before the netns goes away under it
	}

	releaseNamedNetNS(id)
	deleteContainerMeta(ns, id)
	// Refresh the hosts files of the deleted container's network peers
	// (its own metadata is gone, so iterate its former networks directly).
	if meta != nil {
		for _, net := range meta.Networks {
			refreshNetworkHosts(net)
		}
	}
	// Anonymous volumes go only with docker rm -v / --rm, as in Docker,
	// and never while another container still mounts one (--volumes-from):
	// that would delete data out from under it.
	if meta != nil && removeVolumes {
		for _, v := range removableAnonymousVolumes(ns, meta) {
			dir := volumeDataDir(v.ns, v.name)
			if volumeDirInUse(ctx, dir) {
				log.Printf("[docker-api] keeping volume %s: still mounted by another container", truncateID(v.name))
				continue
			}
			os.RemoveAll(dir)
			os.Remove(volumeMetaPath(v.ns, v.name))
		}
	}
	return nil
}

// volumeDirInUse reports whether any remaining container mounts dir.
func volumeDirInUse(ctx context.Context, dir string) bool {
	mounted, err := mountedBindSources(ctx)
	if err != nil {
		return true // unsure: keep the data
	}
	return mounted[filepath.Clean(dir)]
}

// mountedBindSources is the set of bind-mount sources of every container
// (running or not) across namespaces — volume directories included.
func mountedBindSources(ctx context.Context) (map[string]bool, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, err
	}
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		cs, err := cl.Containers(nsCtx)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			spec, err := c.Spec(nsCtx)
			if err != nil {
				continue
			}
			for _, m := range spec.Mounts {
				if m.Type == "bind" {
					out[filepath.Clean(m.Source)] = true
				}
			}
		}
	}
	return out, nil
}

// --- exec primitive -----------------------------------------------------------

// simpleExecResult carries a one-shot exec outcome.
type simpleExecResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// runSimpleExec runs argv inside a running container and captures output.
// It is the shared foundation for healthchecks and cp/archive operations.
func runSimpleExec(ctx context.Context, ns, id string, argv []string, user, cwd string, timeout time.Duration) (*simpleExecResult, error) {
	return runSimpleExecStdin(ctx, ns, id, argv, user, cwd, nil, timeout)
}

// runSimpleExecStdin additionally feeds the given bytes to the exec's stdin
// (nil = closed stdin). Mirrors the lifecycle of handleExecStart (exec.go),
// the battle-tested path: create with attached streams, Start, Wait, drain
// the cio copiers via IO().Wait, then close the parent write ends so the
// pipe readers see EOF.
func runSimpleExecStdin(ctx context.Context, ns, id string, argv []string, user, cwd string, stdin []byte, timeout time.Duration) (*simpleExecResult, error) {
	return runSimpleExecEnv(ctx, ns, id, argv, user, cwd, nil, stdin, timeout)
}

// runSimpleExecEnv is runSimpleExecStdin with extra environment entries on
// top of the container's; a negative timeout means none (docker exec -d).
func runSimpleExecEnv(ctx context.Context, ns, id string, argv []string, user, cwd string, extraEnv []string, stdin []byte, timeout time.Duration) (*simpleExecResult, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return nil, err
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("task: %w", err)
	}

	u := execUserFor(nsCtx, c, user)
	env, ctrCwd := containerProcessDefaults(nsCtx, c)
	env = mergeEnv(env, extraEnv)
	pspec := &specs.Process{
		Args: argv,
		Env:  env,
		Cwd:  cwd,
		User: u,
	}
	if pspec.Cwd == "" {
		pspec.Cwd = ctrCwd
	}

	execID := newContainerID()[:32]
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutW.Close()
		stdoutR.Close()
		return nil, err
	}

	var stdinR io.Reader
	if len(stdin) > 0 {
		stdinR = bytes.NewReader(stdin)
	}
	process, err := task.Exec(nsCtx, execID, pspec, cio.NewCreator(cio.WithStreams(stdinR, stdoutW, stderrW)))
	if err != nil {
		stdoutW.Close()
		stderrW.Close()
		stdoutR.Close()
		stderrR.Close()
		return nil, err
	}
	if timeout < 0 {
		timeout = 100 * 365 * 24 * time.Hour // detached: runs until it exits
	} else if timeout == 0 {
		timeout = 30 * time.Second
	}

	detached := timeout > 30*24*time.Hour
	var outBuf, errBuf strings.Builder
	readAll := func(r *os.File, sb *strings.Builder, done chan struct{}) {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := r.Read(buf)
			// A detached exec's output has no reader: buffering it grew
			// the agent without bound (docker exec -d c yes).
			if n > 0 && !detached {
				sb.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		close(done)
	}
	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go readAll(stdoutR, &outBuf, done1)
	go readAll(stderrR, &errBuf, done2)

	if serr := process.Start(nsCtx); serr != nil {
		stdoutW.Close()
		stderrW.Close()
		<-done1
		<-done2
		process.Delete(context.WithoutCancel(nsCtx)) //nolint:errcheck — namespaced: a bare context is rejected and leaks the record
		stdoutR.Close()
		stderrR.Close()
		return nil, fmt.Errorf("exec start: %w", serr)
	}

	exitCh, werr := process.Wait(nsCtx)
	if werr != nil {
		stdoutW.Close()
		stderrW.Close()
		<-done1
		<-done2
		process.Delete(context.WithoutCancel(nsCtx)) //nolint:errcheck — namespaced: a bare context is rejected and leaks the record
		stdoutR.Close()
		stderrR.Close()
		return nil, werr
	}

	var st client.ExitStatus
	select {
	case s, ok := <-exitCh:
		if !ok {
			return nil, fmt.Errorf("exec wait cancelled")
		}
		st = s
	case <-time.After(timeout):
		kctx, kcancel := context.WithTimeout(nsCtx, 3*time.Second)
		process.Kill(kctx, syscall.SIGKILL) //nolint:errcheck
		kcancel()
		stdoutW.Close()
		stderrW.Close()
		<-done1
		<-done2
		process.Delete(context.WithoutCancel(nsCtx)) //nolint:errcheck — namespaced: a bare context is rejected and leaks the record
		stdoutR.Close()
		stderrR.Close()
		return &simpleExecResult{stdout: outBuf.String(), stderr: errBuf.String(), exitCode: 124},
			fmt.Errorf("exec timed out after %s", timeout)
	}

	// Drain the client-side fifo copiers before closing our write ends,
	// exactly like handleExecStart.
	process.IO().Wait()
	stdoutW.Close()
	stderrW.Close()
	<-done1
	<-done2

	exitCode := 0
	if serr := st.Error(); serr != nil {
		debugLog("[exec] wait error: %v", serr)
	} else {
		exitCode = int(st.ExitCode())
	}
	process.Delete(context.WithoutCancel(nsCtx)) //nolint:errcheck — namespaced: a bare context is rejected and leaks the record
	stdoutR.Close()
	stderrR.Close()
	return &simpleExecResult{stdout: outBuf.String(), stderr: errBuf.String(), exitCode: exitCode}, nil
}

// execUserFor maps an optional "uid[:gid]" user spec onto the OCI User of the
// container's own spec. Non-numeric names fall back to the container user.
func execUserFor(ctx context.Context, c client.Container, userstr string) specs.User {
	out := specs.User{}
	if spec, err := c.Spec(ctx); err == nil && spec.Process != nil {
		out = spec.Process.User
	}
	if userstr == "" {
		return out
	}
	parts := strings.SplitN(userstr, ":", 2)
	if uid, err := parseUint32(parts[0]); err == nil {
		out.UID = uid
	}
	if len(parts) == 2 {
		if gid, err := parseUint32(parts[1]); err == nil {
			out.GID = gid
		}
	}
	return out
}

func parseUint32(s string) (uint32, error) {
	var v uint64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		v = v*10 + uint64(r-'0')
		if v > 0xFFFFFFFF {
			return 0, fmt.Errorf("overflow")
		}
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return uint32(v), nil
}

type volumeRef struct{ ns, name string }

// removableAnonymousVolumes lists the anonymous volumes a container mounts:
// the ones it created, plus anonymous volumes it was handed by name (compose
// passes the old container's anonymous volumes to the recreated one).
func removableAnonymousVolumes(ns string, meta *containerMeta) []volumeRef {
	seen := map[volumeRef]bool{}
	var out []volumeRef
	add := func(r volumeRef) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, v := range meta.AnonymousVolumes {
		add(volumeRef{ns, v})
	}
	for _, mp := range meta.MountPoints {
		if mp.Type != "volume" {
			continue
		}
		if vns, name, ok := volumeRefFromDir(mp.Source); ok && isAnonymousVolume(vns, name) {
			add(volumeRef{vns, name})
		}
	}
	return out
}

// defaultExecPath is the PATH of a container whose spec has none.
const defaultExecPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// containerProcessDefaults returns the environment and working directory
// an exec in the container inherits, as with Docker: the container's own
// (image ENV, -e, WORKDIR, -w). Execs and healthcheck probes used to run
// with PATH only in "/", so `docker exec db psql -U $POSTGRES_USER` and
// healthchecks reading the container's variables failed.
func containerProcessDefaults(nsCtx context.Context, c client.Container) ([]string, string) {
	env, cwd := []string{defaultExecPath}, "/"
	spec, err := c.Spec(nsCtx)
	if err != nil || spec == nil || spec.Process == nil {
		return env, cwd
	}
	env = mergeEnv(env, spec.Process.Env)
	if spec.Process.Cwd != "" {
		cwd = spec.Process.Cwd
	}
	return env, cwd
}

// hasNetworkJoiners reports whether any container runs in this container's
// network namespace (--network container:<it>).
func hasNetworkJoiners(ns, id string) bool {
	metas, err := containerMetas()
	if err != nil {
		return true // unknown: take the safe, slower path
	}
	mode := "container:" + dockerID(ns, id)
	for _, m := range metas {
		if len(m.Networks) > 0 && m.Networks[0] == mode {
			return true
		}
	}
	return false
}

// joinContainerNamespaces points the spec's PID/IPC namespaces at the
// running task of the container named by --pid / --ipc container:<x>. It
// runs at every start: the target's namespaces belong to its current run.
func joinContainerNamespaces(ctx context.Context, c client.Container, pidMode, ipcMode string) error {
	paths := map[specs.LinuxNamespaceType]string{}
	for typ, mode := range map[specs.LinuxNamespaceType]string{specs.PIDNamespace: pidMode, specs.IPCNamespace: ipcMode} {
		ref, ok := strings.CutPrefix(mode, "container:")
		if !ok {
			continue
		}
		tns, tid, _, err := resolveDockerID(ctx, ref)
		if err != nil {
			return errConflict("cannot join %s namespace of %s: %v", typ, ref, err)
		}
		pid, running := containerTaskRootPid(ctx, tns, tid)
		if !running {
			return errConflict("cannot join %s namespace of a non running container: %s", typ, ref)
		}
		name := map[specs.LinuxNamespaceType]string{specs.PIDNamespace: "pid", specs.IPCNamespace: "ipc"}[typ]
		paths[typ] = fmt.Sprintf("/proc/%d/ns/%s", pid, name)
	}
	if len(paths) == 0 {
		return nil
	}
	spec, err := c.Spec(ctx)
	if err != nil {
		return err
	}
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	for typ, path := range paths {
		found := false
		for i := range spec.Linux.Namespaces {
			if spec.Linux.Namespaces[i].Type == typ {
				spec.Linux.Namespaces[i].Path = path
				found = true
			}
		}
		if !found {
			spec.Linux.Namespaces = append(spec.Linux.Namespaces, specs.LinuxNamespace{Type: typ, Path: path})
		}
	}
	return c.Update(ctx, client.UpdateContainerOpts(client.WithSpec(spec)))
}
