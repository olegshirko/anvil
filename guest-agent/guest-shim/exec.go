//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// isExecHelper reports whether this process runs as stage A of the exec
// helper. Stage B runs as "exec-apply" (isExecApply below).
func isExecHelper() bool {
	return len(os.Args) >= 2 && os.Args[1] == "exec-init"
}

// isExecApply reports whether this process runs as stage B of the exec
// helper (born inside the container's pid namespace, before the user
// attributes).
func isExecApply() bool {
	return len(os.Args) >= 2 && os.Args[1] == "exec-apply"
}

// exec helper fd layout (ExtraFiles start at 3): gate, report, then stdio.
// 60 carries the init's root fd into stage B (chroot), 61 the shim binary
// (stage B's exec ran through it; stage B closes it on entry), 62 the
// confirm pipe write end (its closure at the user exec is the start
// signal), 63 the close-on-exec report duplicate for a failed user exec.
// Below 64 on purpose: the first dup onto a descriptor >= 64 expands the fd
// table at 20-50 ms wall (NR_OPEN_DEFAULT), while the Go runtime's own low
// fds (netpoll epoll, own-binary probe) live in 8-9 and must not be hit —
// so the fixed fds sit just under the boundary.
const (
	fdExecGate      = 3
	fdExecStdin     = 5
	fdExecStdout    = 6
	fdExecStderr    = 7
	fdExecRoot      = 60
	fdExecExe       = 61
	fdExecConfirm   = 62
	fdExecReportDup = 63
)

// runExecHelper is stage A of the exec helper. Join the namespaces that are
// legal from a multi-threaded Go process (everything but mount: setns(pid)
// is fine and only switches pid_ns_for_children, so stage A's own pid view
// stays global), then fork a raw window for the mount namespace — setns(mnt)
// needs full CAP_SYS_ADMIN and a private fs struct (fs->users == 1), and
// every Go thread shares the fs struct via CLONE_FS — which execs stage B
// through the shim binary's fd (after setns(mnt) the initramfs path no
// longer resolves). Stage B is born inside the pid namespace (CLONE_THREAD
// requires task_active_pid_ns == pid_ns_for_children, so a process that
// merely joined the pid ns cannot create Go threads) and BECOMES the user
// process with a direct execve, exactly like the init helper's init-apply.
//
// Reports (written by stage B, then stage A): R (setup complete, holding at
// the gate), S pid (the user exec landed), X detail (setup/exec failed),
// D code (exit).
func runExecHelper(initpidArg, bundle string) int {
	initpid, err := strconv.Atoi(initpidArg)
	if err != nil {
		reportLine(reportSetupPrefix+"bad init pid %q: %v", initpidArg, err)
		return 1
	}
	if os.Getenv("ANVIL_EXEC_SPEC") == "" {
		reportLine(reportSetupPrefix + "exec spec env missing")
		return 1
	}
	// The init spec must be read while the initramfs root and the VM's mount
	// view are still current: after the namespace joins below, the bundle
	// path no longer resolves (the container's /run is its own tmpfs). Pass
	// the raw JSON down through the environment instead of the path.
	initSpecJSON, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		reportLine(reportSetupPrefix+"init spec: %v", err)
		return 1
	}
	os.Setenv("ANVIL_INIT_SPEC", string(initSpecJSON))

	// The init's cgroup from its /proc entry (works from any mount view).
	joinCgroupOf(initpid)

	// Namespace fds and the init's root, opened while /proc still shows the
	// VM's process table (paths name the global pid). The namespace fds are
	// close-on-exec; the root fd survives into stage B at a fixed number.
	types := []specs.LinuxNamespaceType{
		specs.UserNamespace, specs.IPCNamespace, specs.UTSNamespace,
		specs.NetworkNamespace, specs.CgroupNamespace, specs.PIDNamespace,
		specs.MountNamespace,
	}
	nsFds := make(map[specs.LinuxNamespaceType]int, len(types))
	for _, typ := range types {
		fd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/%s", initpid, nsTypeName(typ)), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			for _, f := range nsFds {
				unix.Close(f) //nolint:errcheck
			}
			reportLine(reportSetupPrefix+"open %s ns: %v", typ, err)
			return 1
		}
		nsFds[typ] = fd
	}
	defer func() {
		for _, fd := range nsFds {
			unix.Close(fd) //nolint:errcheck — consumed during setup
		}
	}()
	rootFd, err := unix.Open(fmt.Sprintf("/proc/%d/root", initpid), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		reportLine(reportSetupPrefix+"open init root: %v", err)
		return 1
	}
	if err := unix.Dup3(rootFd, fdExecRoot, 0); err != nil {
		reportLine(reportSetupPrefix+"dup root fd: %v", err)
		return 1
	}
	unix.Close(rootFd) //nolint:errcheck — handed over at fdExecRoot

	// Join everything but the mount namespace here: multithreaded is fine
	// for these (only setns(mnt) checks fs->users). The pid namespace join
	// is pid_ns_for_children only — stage A keeps its global pid view, and
	// every forked child (the raw window and stage B) is born inside.
	for _, typ := range []specs.LinuxNamespaceType{
		specs.UserNamespace, specs.IPCNamespace, specs.UTSNamespace,
		specs.NetworkNamespace, specs.CgroupNamespace, specs.PIDNamespace,
	} {
		fd := nsFds[typ]
		if err := unix.Setns(fd, 0); err != nil {
			if typ == specs.UserNamespace {
				// Containers without a private user namespace share the
				// host's; setns into the namespace we are already in
				// returns EINVAL. Best effort, like runc's nsenter.
				continue
			}
			reportLine(reportSetupPrefix+"setns %s: %v", typ, err)
			return 1
		}
	}

	// Confirm pipe: stage B holds the write end close-on-exec, so a
	// successful user exec closes it instantly (EOF = started) and a failed
	// one reports through it. Same contract as the init helper's.
	confirm := make([]int, 2)
	if err := unix.Pipe(confirm); err != nil {
		reportLine(reportSetupPrefix+"confirm pipe: %v", err)
		return 1
	}
	unix.CloseOnExec(confirm[0])
	unix.CloseOnExec(confirm[1])
	defer unix.Close(confirm[0]) //nolint:errcheck
	if err := unix.Dup3(confirm[1], fdExecConfirm, 0); err != nil {
		reportLine(reportSetupPrefix+"dup confirm: %v", err)
		return 1
	}
	unix.Close(confirm[1]) //nolint:errcheck — handed over at fdExecConfirm

	// The raw window's inputs, all prepared before the fork: no Go runs in
	// the child (allocator and runtime locks are copies). The shim binary is
	// held open because setns(mnt) replaces the process root with the
	// container's — the initramfs path stops resolving and the stage-B exec
	// must go through execveat(AT_EMPTY_PATH) on the fd.
	self, err := os.Executable()
	if err != nil {
		reportLine(reportSetupPrefix+"resolve self: %v", err)
		return 1
	}
	argv, err := argvBytePtrs([]string{self, "exec-apply", "-bundle", bundle})
	if err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	env, err := envBytePtrs(os.Environ())
	if err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	empty, err := unix.BytePtrFromString("")
	if err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	exeFd, err := unix.Open("/proc/self/exe", unix.O_RDONLY, 0)
	if err != nil {
		reportLine(reportSetupPrefix+"open self exe: %v", err)
		return 1
	}
	if err := unix.Dup3(exeFd, fdExecExe, 0); err != nil {
		reportLine(reportSetupPrefix+"dup exe fd: %v", err)
		return 1
	}
	unix.Close(exeFd) //nolint:errcheck — handed over at fdExecExe

	mntFd := nsFds[specs.MountNamespace]
	pid, errno := forkRaw()
	if errno != 0 {
		reportLine(reportSetupPrefix+"fork ns join: %v", errno)
		return 1
	}
	if pid == 0 {
		execApplyChild(mntFd, fdExecExe, empty, argv, env)
		// unreached
	}
	unix.Close(fdExecExe)     //nolint:errcheck — the window keeps its copy
	unix.Close(fdExecConfirm) //nolint:errcheck — stage B's copy matters

	// EOF on the confirm pipe = the user exec happened; a line is the
	// failure stage B wrote instead. Then mirror stage B's exit code.
	buf := make([]byte, 128)
	n, _ := unix.Read(confirm[0], buf)
	if n > 0 {
		msg := strings.TrimSpace(string(buf[:n]))
		if msg == "" {
			msg = "exec died before exec"
		}
		var ws unix.WaitStatus
		code := 127
		if _, werr := unix.Wait4(pid, &ws, 0, nil); werr == nil {
			code = int(exitStatus(ws))
		}
		reportLine(reportStartPrefix+"%s", msg)
		reportLine(reportDonePrefix+"%d", code)
		return code
	}
	reportLine(reportStartedPrefix+"%d", pid)
	var ws unix.WaitStatus
	if _, waitErr := unix.Wait4(pid, &ws, 0, nil); waitErr != nil {
		reportLine(reportDonePrefix + "137")
		return 137
	}
	code := int(exitStatus(ws))
	reportLine(reportDonePrefix+"%d", code)
	return code
}

// runExecApply is stage B of the exec helper, and the user process itself:
// born inside the container's pid namespace (stage A joined it), it moves
// its root into the container, finalizes stdio, resolves the exec path
// while the caps/seccomp filter are not applied yet, applies the user
// attributes, holds at the gate, and finally execve's the user command in
// place — the same shape as the init helper's init-apply.
func runExecApply() int {
	specJSON := os.Getenv("ANVIL_EXEC_SPEC")
	var pspec specs.Process
	if err := json.Unmarshal([]byte(specJSON), &pspec); err != nil {
		reportLine(reportSetupPrefix+"exec spec: %v", err)
		return 1
	}
	// The init spec arrives as raw JSON through the environment (stage A
	// read it while the bundle path still resolved — the container's mount
	// namespace has no /run/containerd). Parse it into the spec.
	initSpec := &specs.Spec{}
	if err := json.Unmarshal([]byte(os.Getenv("ANVIL_INIT_SPEC")), initSpec); err != nil {
		reportLine(reportSetupPrefix+"init spec: %v", err)
		return 1
	}
	// The shim binary fd (stage B was exec'd through it) is dead weight from
	// here on: close it before anything it could leak into.
	unix.Close(fdExecExe) //nolint:errcheck

	// Move this process's root into the container: setns(mnt) switches the
	// mount namespace, not the process root — without the chroot every path
	// still resolves against the initramfs (runc's nsenter does the same
	// via /proc/<pid>/root).
	if err := unix.Fchdir(fdExecRoot); err != nil {
		reportLine(reportSetupPrefix+"fchdir init root: %v", err)
		return 1
	}
	unix.Close(fdExecRoot) //nolint:errcheck
	if err := unix.Chroot("."); err != nil {
		reportLine(reportSetupPrefix+"chroot init root: %v", err)
		return 1
	}

	// The exec's working directory, resolved inside the container's rootfs
	// (docker exec -w, and the container's own Cwd when the exec spec
	// carries none). runc's exec chdir's before applying the user and the
	// filter, and so must this one: without it every exec started in "/".
	if pspec.Cwd != "" {
		if err := unix.Chdir(pspec.Cwd); err != nil {
			reportLine(reportSetupPrefix+"chdir %s: %v", pspec.Cwd, err)
			return 1
		}
	}

	// The confirm fd must die at the user exec (its closure is the start
	// confirmation stage A waits for).
	unix.CloseOnExec(fdExecConfirm)

	// Final stdio (fds 5/6/7 were handed over by the shim).
	tty := pspec.Terminal
	if tty {
		if _, err := unix.Setsid(); err != nil {
			reportLine(reportSetupPrefix+"setsid: %v", err)
			return 1
		}
		if err := unix.IoctlSetInt(fdExecStdin, unix.TIOCSCTTY, 0); err != nil {
			reportLine(reportSetupPrefix+"TIOCSCTTY: %v", err)
			return 1
		}
	}
	for fd, target := range map[int]int{fdExecStdin: 0, fdExecStdout: 1, fdExecStderr: 2} {
		if err := unix.Dup3(fd, target, 0); err != nil {
			reportLine(reportSetupPrefix+"dup %d: %v", fd, err)
			return 1
		}
	}
	unix.Close(fdExecStdin)  //nolint:errcheck
	unix.Close(fdExecStdout) //nolint:errcheck
	unix.Close(fdExecStderr) //nolint:errcheck
	// dup copies the fd status flags, and the shim's io fifos are poller
	// registered on its side (O_NONBLOCK). The user process must get plain
	// blocking stdio — reads would EAGAIN past the first pipe buffer.
	for fd := 0; fd <= 2; fd++ {
		unix.SetNonblock(fd, false) //nolint:errcheck
	}

	// Resolve the exec path before the process attributes are applied — the
	// seccomp filter is part of those attributes and is active on this
	// process from its install until the exec, and the lookup's stat calls
	// are not on the profile's allowlist (runc's ordering is the same for a
	// reason). The root was chrooted into the container's rootfs above, so
	// the lookup sees the container's filesystem.
	argv0, err := resolveExecPath(&specs.Spec{Process: &pspec})
	if err != nil {
		reportLine(reportStartPrefix+"%v", err)
		return 127
	}

	// Attributes: the merged exec process (falls back to the init's caps,
	// rlimits and oom score) plus the container's seccomp filter — the
	// filter is inherited only from the parent, and this process is a fresh
	// child of the shim, so it must install it explicitly.
	if err := applyProcessAttrs("", &specs.Spec{Process: &pspec, Linux: initSpec.Linux}); err != nil {
		reportLine(reportStartPrefix+"%v", err)
		return 126
	}
	reportLine(reportReady)

	if !waitGate() {
		return 0
	}
	unix.Close(fdExecGate) //nolint:errcheck — consumed

	// The report fd closes before the exec too (a live write end in the
	// user process would hold stage A's confirm read and the shim's report
	// fallback open until the container exits). A close-on-exec duplicate
	// carries the failure report if the execve fails.
	if err := unix.Dup3(fdReport, fdExecReportDup, unix.O_CLOEXEC); err != nil {
		reportLine(reportStartPrefix+"dup report: %v", err)
		return 126
	}
	unix.Close(fdReport) //nolint:errcheck — dup'ed above
	if err := unix.Exec(argv0, pspec.Args, pspec.Env); err != nil {
		reportLineTo(fdExecReportDup, reportStartPrefix+"exec %s: %v", argv0, err)
		return 127
	}
	return 0 // unreached
}

// execApplyChild is the raw mount-join window: setns into the mount
// namespace of the running init (single-threaded and with full caps —
// setns(mnt) needs CAP_SYS_ADMIN and is EINVAL when fs_struct users > 1,
// which every Go process has from its runtime threads' CLONE_FS), then exec
// stage B through the shim binary's fd — after setns(mnt) the initramfs
// path no longer resolves, so the exec is execveat(exeFd, "",
// AT_EMPTY_PATH). All pointers are pre-built in stage A (no Go runs after
// the fork); any failure reports through the report fd and exits 126.
func execApplyChild(mntFd, exeFd int, emptyPath *byte, argv, env []*byte) {
	_, _, errno := unix.RawSyscall(unix.SYS_SETNS, uintptr(mntFd), 0, 0)
	if errno != 0 {
		failApply(failSetnsMntMessage)
	}
	unix.RawSyscall6(unix.SYS_EXECVEAT,
		uintptr(exeFd),
		uintptr(unsafe.Pointer(emptyPath)),
		uintptr(unsafe.Pointer(&argv[0])),
		uintptr(unsafe.Pointer(&env[0])),
		unix.AT_EMPTY_PATH, 0)
	// Stage B exec failed (fd bad, OOM): report and die like the init's
	// raw window does.
	failApply(failStageExecMessage)
}

// failApply writes one pre-built failure line to the report fd and exits
// 126 — the only channel still open in the raw window (no Go, so no
// formatting).
func failApply(msg []byte) {
	unix.RawSyscall(unix.SYS_WRITE, uintptr(fdReport),
		uintptr(unsafe.Pointer(&msg[0])), uintptr(len(msg)))
	unix.RawSyscall(unix.SYS_EXIT_GROUP, 126, 0, 0) //nolint:errcheck
}

// Pre-built raw-window failure reports (the errno itself is not formattable
// without allocations).
var (
	failSetnsMntMessage  = []byte("X exec failed (setns mount namespace)\n")
	failStageExecMessage = []byte("X exec failed (stage exec)\n")
)

// nsTypeName maps a namespace type to its /proc/<pid>/ns directory name.
func nsTypeName(t specs.LinuxNamespaceType) string {
	switch t {
	case specs.PIDNamespace:
		return "pid"
	case specs.UserNamespace:
		return "user"
	case specs.IPCNamespace:
		return "ipc"
	case specs.UTSNamespace:
		return "uts"
	case specs.MountNamespace:
		return "mnt"
	case specs.CgroupNamespace:
		return "cgroup"
	case specs.NetworkNamespace:
		return "net"
	case specs.TimeNamespace:
		return "time"
	}
	return string(t)
}

// joinCgroupOf moves the caller into the init process's cgroup (v2: the
// "0::/path" line of its /proc/<pid>/cgroup).
func joinCgroupOf(initpid int) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", initpid))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		rel, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		rel = strings.TrimSpace(rel)
		if rel == "" || rel == "/" {
			return // root cgroup: nothing to join
		}
		os.WriteFile(filepath.Join(cgroupRoot, strings.TrimPrefix(rel, "/"), "cgroup.procs"), //nolint:errcheck
			[]byte(strconv.Itoa(os.Getpid())), 0o600)
		return
	}
}
