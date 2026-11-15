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

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// The init helper, in two stages (the same shape as runc's nsexec + init):
//
//	stage A ("init", started by the shim via exec.Cmd): joins the
//	     namespaces (setns for the pathed ones, unshare for the fresh
//	     ones), moves into the cgroup, then forks stage B and supervises
//	     it: it confirms the start upstream (the user exec closed the
//	     confirm pipe), reports the exit code and exits with it.
//
//	stage B ("init-apply", the raw-forked child of A, re-exec'd as a
//	     fresh Go process so the new PID namespace is its ACTIVE one):
//	     applies the spec mounts onto the prepared rootfs, creates
//	     devices, pivot_roots, applies sysctls and hostname, holds at the
//	     start gate, applies the exec-time attributes (oom score,
//	     rlimits, no-new-privileges, user, capabilities, seccomp) and
//	     finally execve's the user process in place — PID 1 of the
//	     container.
//
// Why the second stage: unshare(CLONE_NEWPID) does not switch the caller's
// active namespace (only its children enter it), so a procfs mounted by
// stage A would expose the VM's process table inside the container, and no
// pid seen by stage A translates across the boundary. Stage B, born inside
// the new namespace, mounts the container's own procfs and execs the user
// process as PID 1. Stage A's fork return is the child's GLOBAL pid (the
// creator stays in the old namespace), which is what containerd needs.
//
// Report protocol over fd 4 (one line each, prefixes defined in reports.go):
// "R" setup complete; "P <pid>" stage B born (its global pid, reported at
// create); "S <pid>" user process started (global pid); "X <detail>" start
// failed; "D <code>" init exited; "E <detail>" setup failed at create.

// Fixed fd numbers the shim hands over via exec.Cmd ExtraFiles (fds start
// at 3).
const (
	fdGate   = 3 // read end: "s" releases the created init, EOF exits it
	fdReport = 4 // write end: progress back to the shim
	fdStdin  = 5 // final stdin (fifo read end or console slave)
	fdStdout = 6 // final stdout (logger pipe or fifo write end)
	fdStderr = 7 // final stderr

	// fdConfirm carries the stage-B confirm pipe write end (cleared
	// close-on-exec so it survives stage B's exec; stage B marks it
	// close-on-exec again so the user exec closes it as the start signal).
	fdConfirm = 200
)

// gateStartLine is the gate release message (written by the shim at start).
const gateStartLine = "s\n"

// isInitHelper reports whether this process runs as stage A of the
// container init helper.
func isInitHelper() bool {
	return len(os.Args) >= 2 && os.Args[1] == "init"
}

// isInitApply reports whether this process runs as stage B (the container
// init itself).
func isInitApply() bool {
	return len(os.Args) >= 2 && os.Args[1] == "init-apply"
}

// isExecHelper reports whether this process runs as an exec helper
// (second argument "exec-init").
func isExecHelper() bool {
	return len(os.Args) >= 2 && os.Args[1] == "exec-init"
}

// reportLine writes one line to the shim through the report fd.
func reportLine(format string, args ...any) {
	reportLineTo(fdReport, format, args...)
}

// reportLineTo writes one report line to an arbitrary descriptor (the
// exec path reports through a duplicate after closing the report fd
// itself, so nothing leaks into the exec'd process).
func reportLineTo(fd int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	f := os.NewFile(uintptr(fd), "report")
	fmt.Fprint(f, msg)
}

// waitGate blocks on the release pipe. It returns false only when the pipe
// closed (deleted-while-created / shim death).
func waitGate() bool {
	f := os.NewFile(fdGate, "gate")
	buf := make([]byte, 8)
	for {
		n, err := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == 's' {
				return true
			}
		}
		if err != nil {
			return false
		}
	}
}

// runInit is stage A. Returns the process exit code (stage B's exit code —
// the container init's).
func runInit(id, bundle string) int {
	spec, err := readSpecFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	if spec.Process == nil {
		reportLine(reportSetupPrefix + "spec has no process")
		return 1
	}
	if err := joinCgroup(spec); err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}

	// The confirm pipe: stage B holds the write end close-on-exec, so a
	// successful execve of the user process closes it instantly (EOF =
	// started) and a failed one reports through it.
	confirm := make([]int, 2)
	if err := unix.Pipe(confirm); err != nil {
		reportLine(reportSetupPrefix+"confirm pipe: %v", err)
		return 1
	}
	// The original write end must not leak into stage B (or the exec'd
	// user process) — its LAST closure is what produces the start
	// confirmation EOF. The fixed-fd dup below clears the flag on the
	// handed-over copy only. The read end is close-on-exec too: it is
	// stage A's alone and must never reach stage B or the user process.
	unix.CloseOnExec(confirm[1])
	unix.CloseOnExec(confirm[0])
	defer unix.Close(confirm[0]) //nolint:errcheck
	defer unix.Close(confirm[1]) //nolint:errcheck

	// Stage B must be born INSIDE the new pid namespace: unshare(NEWPID)
	// does not switch the caller's active namespace, and a Go fork after
	// unshare(CLONE_NEWPID) fails outright (ENOMEM from the runtime's
	// vfork-style clone). So stage B is created by a raw
	// clone(CLONE_NEWPID|SIGCHLD) whose child execve's this binary as
	// "init-apply" before any Go code runs. The creator stays in the old
	// pid namespace, so the clone return IS the child's global pid.
	self, err := os.Executable()
	if err != nil {
		reportLine(reportSetupPrefix+"resolve self: %v", err)
		return 1
	}
	argv := []string{self, "init-apply", "-id", id, "-bundle", bundle}
	argvPtrs, err := argvBytePtrs(argv)
	if err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	pathPtr, err := unix.BytePtrFromString(self)
	if err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	envPtrs, err := envBytePtrs(os.Environ())
	if err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	// The confirm write end must survive the child's execve (its closure
	// is the start signal), so hand it over at a fixed fd with the
	// close-on-exec bit cleared.
	if err := unix.Dup3(confirm[1], fdConfirm, 0); err != nil {
		reportLine(reportSetupPrefix+"dup confirm: %v", err)
		return 1
	}
	stageBGlobal, errno := cloneRawPid()
	if errno != 0 {
		unix.Close(fdConfirm) //nolint:errcheck
		reportLine(reportSetupPrefix+"clone stage B: %v", errno)
		return 1
	}
	if stageBGlobal == 0 {
		// Raw window (born into the new pid namespace as its PID 1): no
		// Go runtime calls, pre-built pointers only.
		unix.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(pathPtr)),
			uintptr(unsafe.Pointer(&argvPtrs[0])), uintptr(unsafe.Pointer(&envPtrs[0])))
		unix.RawSyscall(unix.SYS_EXIT_GROUP, 127, 0, 0) //nolint:errcheck
	}
	// Parent: drop BOTH write ends (the fixed-fd dup and the original);
	// only then does stage B's death turn into EOF on the confirm pipe.
	unix.Close(fdConfirm)  //nolint:errcheck
	unix.Close(confirm[1]) //nolint:errcheck

	// Stage B exists from this moment (it holds the gate through its
	// mount setup), so its global pid is already reportable: create
	// answers with it — the same contract the runc shim's Create has —
	// and kill/delete/state key off it before any start happens.
	reportLine(reportInitPrefix+"%d", stageBGlobal)

	// EOF = the user exec happened; a line is the failure the child wrote.
	buf := make([]byte, 128)
	readN := readConfirm(confirm[0], buf)
	if readN > 0 {
		// The child wrote the failure message (or died mid-report).
		msg := strings.TrimSpace(string(buf[:readN]))
		if msg == "" {
			msg = "init died before exec"
		}
		var ws unix.WaitStatus
		wp, werr := unix.Wait4(stageBGlobal, &ws, 0, nil)
		code := 127
		if werr == nil && wp == stageBGlobal {
			code = int(exitStatus(ws))
		}
		reportLine(reportStartPrefix+"%s", msg)
		reportLine(reportDonePrefix+"%d", code)
		return code
	}

	// The container init is alive and is PID 1 of the namespace; report
	// its global pid for containerd's state.
	reportLine(reportStartedPrefix+"%d", stageBGlobal)

	// Supervise until the init exits, then report and mirror its exit code.
	var ws unix.WaitStatus
	_, waitErr := unix.Wait4(stageBGlobal, &ws, 0, nil)
	code := 137 // lost the child (should not happen); kill-style code
	if waitErr == nil {
		code = int(exitStatus(ws))
	}
	reportLine(reportDonePrefix+"%d", code)
	return code
}

// readConfirm reads one confirmation from the pipe: EOF (no bytes) means
// the user exec succeeded, anything else is the failure message. Returns
// the byte count read (0 = EOF).
func readConfirm(fd int, buf []byte) int {
	n, _ := unix.Read(fd, buf)
	return n
}

// runInitApply is stage B: the container init itself. Runs with the new
// PID namespace as its active one, so the procfs it mounts shows the
// container's own processes and the exec'd user process is PID 1.
func runInitApply(id, bundle string) int {
	spec, err := readSpecFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	if spec.Process == nil {
		reportLine(reportSetupPrefix + "spec has no process")
		return 1
	}

	// Final stdio: dup the prepared fds onto 0/1/2. A TTY init makes the
	// console slave its controlling terminal first.
	if spec.Process.Terminal {
		if _, err := unix.Setsid(); err != nil {
			reportLine(reportSetupPrefix+"setsid: %v", err)
			return 1
		}
		if err := unix.IoctlSetInt(fdStdin, unix.TIOCSCTTY, 0); err != nil {
			reportLine(reportSetupPrefix+"TIOCSCTTY: %v", err)
			return 1
		}
	}
	for fd, target := range map[int]int{fdStdin: 0, fdStdout: 1, fdStderr: 2} {
		if err := unix.Dup3(fd, target, 0); err != nil {
			reportLine(reportSetupPrefix+"dup %d: %v", fd, err)
			return 1
		}
	}
	unix.Close(fdStdin)  //nolint:errcheck
	unix.Close(fdStdout) //nolint:errcheck
	unix.Close(fdStderr) //nolint:errcheck
	// The confirm fd must die at the user exec (its closure is the start
	// confirmation stage A waits for).
	unix.CloseOnExec(fdConfirm)

	rootfs := filepath.Join(bundle, "rootfs")

	// The pid namespace came with the birth (stage A's clone carried
	// CLONE_NEWPID); everything else joins here — unshare cannot be done
	// in stage A for the pid namespace, and the rest is safe in this
	// fresh process (it is single-threaded at this point, so even the
	// mount namespace unshare behaves).
	if err := joinNamespacesApply(spec); err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	if err := applySpecMounts(rootfs, spec.Mounts); err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	if err := setupDevices(rootfs, spec); err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	if err := applyMaskedAndReadonly(rootfs, spec); err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	if err := pivotRoot(rootfs); err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	if spec.Root != nil && spec.Root.Readonly {
		if err := unix.Mount("", "/", "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			reportLine(reportSetupPrefix+"readonly root: %v", err)
			return 1
		}
	}
	if spec.Linux != nil && len(spec.Linux.Sysctl) > 0 {
		if err := applySysctls(spec.Linux.Sysctl); err != nil {
			reportLine(reportSetupPrefix+"%v", err)
			return 1
		}
	}
	if spec.Hostname != "" && !utsNamespaceShared(spec) {
		if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
			reportLine(reportSetupPrefix+"sethostname: %v", err)
			return 1
		}
	}
	if spec.Process.Cwd != "" {
		if err := unix.Chdir(spec.Process.Cwd); err != nil {
			reportLine(reportSetupPrefix+"chdir %s: %v", spec.Process.Cwd, err)
			return 1
		}
	}
	reportLine(reportReady)

	// Hold at the gate. EOF without a start: the task was deleted (or the
	// shim died) — leave quietly like a killed created init.
	if !waitGate() {
		return 0
	}
	// The gate is consumed; its read end must not leak into the user
	// process (runc's init execs with exactly 0/1/2).
	unix.Close(fdGate) //nolint:errcheck

	// Everything the user process must already carry is applied to this
	// process before the exec: the exec'd init inherits it all.
	if err := applyProcessAttrs(spec); err != nil {
		reportLine(reportStartPrefix+"%v", err)
		return 126
	}

	// Become the user process in place: PID 1 of the container, children
	// reparent to it, its exit ends the namespace.
	argv0, err := resolveExecPath(spec)
	if err != nil {
		reportLine(reportStartPrefix+"%v", err)
		return 127
	}
	// The report fd closes before the exec too (a live write end in the
	// user process would hold the shim's report-EOF fallback open until
	// the container exits by itself). A close-on-exec duplicate carries
	// the failure report if the execve fails — the only case the dup is
	// still open after the exec attempt.
	const fdReportFinal = 201
	if err := unix.Dup3(fdReport, fdReportFinal, unix.O_CLOEXEC); err != nil {
		reportLine(reportStartPrefix+"dup report: %v", err)
		return 126
	}
	unix.Close(fdReport) //nolint:errcheck — dup'ed above
	if err := unix.Exec(argv0, spec.Process.Args, spec.Process.Env); err != nil {
		reportLineTo(fdReportFinal, reportStartPrefix+"exec %s: %v", argv0, err)
		return 127
	}
	return 0 // unreached
}

// utsNamespaceShared reports a UTS namespace joined by path (host uts or
// container:<x>): the hostname is shared, so it must not be set.
func utsNamespaceShared(spec *specs.Spec) bool {
	if spec.Linux == nil {
		return false
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.UTSNamespace && ns.Path != "" {
			return true
		}
	}
	return false
}

// joinNamespacesApply joins the non-pid namespaces of the spec in stage B:
// pathed ones via setns (fds opened before any mount-ns switch), pathless
// ones via unshare. The pid namespace is skipped (stage A's clone put this
// process inside it — unsharing another would nest a second one). The cgroup
// namespace is unshared here too: it roots at the caller's current cgroup,
// which stage A already moved into the container's — so the spec mount of
// the cgroup root exposes only the container's subtree, as runc's private
// cgroup namespace does.
func joinNamespacesApply(spec *specs.Spec) error {
	if spec.Linux == nil {
		return nil
	}
	type nsEntry struct {
		typ  specs.LinuxNamespaceType
		path string
	}
	var (
		pathed []nsEntry
		flags  uint64
	)
	for _, ns := range spec.Linux.Namespaces {
		switch ns.Type {
		case specs.PIDNamespace:
			if ns.Path != "" {
				return fmt.Errorf("pid namespace joined by path is not supported")
			}
			continue // already active
		case specs.MountNamespace:
			if ns.Path != "" {
				return fmt.Errorf("mount namespace joined by path is not supported")
			}
		}
		if ns.Path != "" {
			pathed = append(pathed, nsEntry{ns.Type, ns.Path})
			continue
		}
		f, ok := namespaceCloneFlag(ns.Type)
		if !ok {
			return fmt.Errorf("unsupported namespace %q", ns.Type)
		}
		flags |= f
	}
	// Open all pathed namespace fds first (paths still resolve in the
	// shim's mount namespace view), then setns: everything but the mount
	// namespace first, mount last.
	type opened struct {
		fd  int
		typ specs.LinuxNamespaceType
		mnt bool
	}
	var fds []opened
	for _, p := range pathed {
		fd, err := unix.Open(p.path, unix.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", p.path, err)
		}
		fds = append(fds, opened{fd: fd, typ: p.typ, mnt: p.typ == specs.MountNamespace})
	}
	defer func() {
		for _, o := range fds {
			unix.Close(o.fd) //nolint:errcheck
		}
	}()
	for _, o := range fds {
		if !o.mnt {
			if err := unix.Setns(o.fd, 0); err != nil {
				return fmt.Errorf("setns %s: %w", o.typ, err)
			}
		}
	}
	for _, o := range fds {
		if o.mnt {
			if err := unix.Setns(o.fd, 0); err != nil {
				return fmt.Errorf("setns mount: %w", err)
			}
		}
	}
	if flags != 0 {
		if err := unix.Unshare(int(flags)); err != nil {
			return fmt.Errorf("unshare namespaces: %w", err)
		}
	}
	return nil
}

// joinCgroup moves this process into the container's cgroup (the shim
// already created it and wrote the spec's limits): the container init born
// from this process inherits the placement — the fast shim's equivalent of
// entering the cgroup at process creation.
func joinCgroup(spec *specs.Spec) error {
	if spec.Linux == nil || spec.Linux.CgroupsPath == "" {
		return nil
	}
	procs := filepath.Join(cgroupAbsPath(spec.Linux.CgroupsPath), "cgroup.procs")
	return os.WriteFile(procs, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// prSetOomScoreAdj is PR_SET_OOM_SCORE_ADJ (not in x/sys).
const prSetOomScoreAdj = 36

// applyProcessAttrs sets up everything that must be in place the moment the
// container init starts: oom score, rlimits, no-new-privileges, user, caps
// and (last) the seccomp filter. Order matters: credentials before caps,
// seccomp strictly last — the filter also applies to this process, and the
// exec'd user process inherits it across the exec.
func applyProcessAttrs(spec *specs.Spec) error {
	p := spec.Process
	if p.OOMScoreAdj != nil {
		if err := unix.Prctl(prSetOomScoreAdj, uintptr(*p.OOMScoreAdj), 0, 0, 0); err != nil {
			return fmt.Errorf("oom score adj: %w", err)
		}
	}
	for _, rl := range p.Rlimits {
		if err := unix.Setrlimit(rlimitFromName(rl.Type), &unix.Rlimit{Cur: rl.Soft, Max: rl.Hard}); err != nil {
			return fmt.Errorf("rlimit %s: %w", rl.Type, err)
		}
	}
	if p.NoNewPrivileges {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("no new privs: %w", err)
		}
	}
	if u := p.User; u.UID != 0 || u.GID != 0 || len(u.AdditionalGids) > 0 {
		if err := applyUser(u); err != nil {
			return err
		}
	}
	if p.Capabilities != nil {
		if err := applyCapabilities(p.Capabilities); err != nil {
			return err
		}
	}
	if spec.Linux != nil && spec.Linux.Seccomp != nil {
		reportLine(reportStartPrefix + "probe pre-seccomp")
		if err := applySeccomp(spec.Linux.Seccomp); err != nil {
			return err
		}
		reportLine(reportStartPrefix + "probe post-seccomp")
	}
	return nil
}

// applyUser drops the credentials (setgroups before setgid/setuid).
func applyUser(u specs.User) error {
	if len(u.AdditionalGids) > 0 {
		gids := make([]int, len(u.AdditionalGids))
		for i, g := range u.AdditionalGids {
			gids[i] = int(g)
		}
		if err := unix.Setgroups(gids); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
	}
	if err := unix.Setresgid(int(u.GID), int(u.GID), int(u.GID)); err != nil {
		return fmt.Errorf("setgid %d: %w", u.GID, err)
	}
	if err := unix.Setresuid(int(u.UID), int(u.UID), int(u.UID)); err != nil {
		return fmt.Errorf("setuid %d: %w", u.UID, err)
	}
	return nil
}

// capVersion3 is LINUX_CAPABILITY_VERSION_3.
const capVersion3 = 0x20080522

// applyCapabilities installs the spec's capability sets: bounding via
// PR_CAPBSET_DROP, the classic sets via capset (V3: two words per set) and
// ambient via PR_CAP_AMBIENT — the same sequence runc's capabilities_linux
// applies, so --user images keep their file caps working.
func applyCapabilities(caps *specs.LinuxCapabilities) error {
	perm := capBits(caps.Permitted)
	if perm == 0 {
		perm = capBits(caps.Bounding) // runc: fall back to bounding
	}
	eff := capBits(caps.Effective)
	if eff == 0 {
		eff = perm
	}
	inh := capBits(caps.Inheritable)
	amb := capBits(caps.Ambient)
	bound := capBits(caps.Bounding)

	for c := 0; c < 64; c++ {
		if bound&(1<<c) == 0 {
			if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil && err != unix.EINVAL {
				return fmt.Errorf("drop bounding cap %d: %w", c, err)
			}
		}
	}
	hdr := unix.CapUserHeader{Version: capVersion3, Pid: 0}
	data := []unix.CapUserData{
		{Effective: uint32(eff), Permitted: uint32(perm), Inheritable: uint32(inh)},
		{Effective: uint32(eff >> 32), Permitted: uint32(perm >> 32), Inheritable: uint32(inh >> 32)},
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	for c := 0; c < 64; c++ {
		if amb&(1<<c) != 0 {
			if err := unix.Prctl(unix.PR_CAP_AMBIENT, uintptr(unix.PR_CAP_AMBIENT_RAISE), uintptr(c), 0, 0); err != nil {
				return fmt.Errorf("ambient cap %d: %w", c, err)
			}
		}
	}
	return nil
}

// capBits converts a ["CAP_SYS_ADMIN", ...] name list into a bitmask.
func capBits(names []string) uint64 {
	var out uint64
	for _, n := range names {
		if idx, ok := capabilityIndex[n]; ok && idx < 64 {
			out |= 1 << idx
		}
	}
	return out
}

// rlimitFromName converts an OCI rlimit type ("RLIMIT_NOFILE") to a constant.
func rlimitFromName(t string) int {
	name := strings.TrimPrefix(strings.ToUpper(t), "RLIMIT_")
	if idx, ok := rlimitIndex[name]; ok {
		return idx
	}
	return 0
}

// readSpecFile parses the bundle's config.json.
func readSpecFile(path string) (*specs.Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &spec, nil
}

// exitStatus converts a wait status to the conventional exit code
// (128+signal for signal deaths), matching containerd's runc shim.
func exitStatus(ws unix.WaitStatus) uint32 {
	if ws.Signaled() {
		return uint32(128 + int(ws.Signal()))
	}
	return uint32(ws.ExitStatus())
}

// capabilityIndex maps OCI capability names to their kernel bit numbers
// (CAP_* constants; the list is stable).
var capabilityIndex = map[string]int{
	"CAP_CHOWN": 0, "CAP_DAC_OVERRIDE": 1, "CAP_DAC_READ_SEARCH": 2,
	"CAP_FOWNER": 3, "CAP_FSETID": 4, "CAP_KILL": 5, "CAP_SETGID": 6,
	"CAP_SETUID": 7, "CAP_SETPCAP": 8, "CAP_LINUX_IMMUTABLE": 9,
	"CAP_NET_BIND_SERVICE": 10, "CAP_NET_BROADCAST": 11, "CAP_NET_ADMIN": 12,
	"CAP_NET_RAW": 13, "CAP_IPC_LOCK": 14, "CAP_IPC_OWNER": 15,
	"CAP_SYS_MODULE": 16, "CAP_SYS_RAWIO": 17, "CAP_SYS_CHROOT": 18,
	"CAP_SYS_PTRACE": 19, "CAP_SYS_PACCT": 20, "CAP_SYS_ADMIN": 21,
	"CAP_SYS_BOOT": 22, "CAP_SYS_NICE": 23, "CAP_SYS_RESOURCE": 24,
	"CAP_SYS_TIME": 25, "CAP_SYS_TTY_CONFIG": 26, "CAP_MKNOD": 27,
	"CAP_LEASE": 28, "CAP_AUDIT_WRITE": 29, "CAP_AUDIT_CONTROL": 30,
	"CAP_SETFCAP": 31, "CAP_MAC_OVERRIDE": 32, "CAP_MAC_ADMIN": 33,
	"CAP_SYSLOG": 34, "CAP_WAKE_ALARM": 35, "CAP_BLOCK_SUSPEND": 36,
	"CAP_AUDIT_READ": 37, "CAP_PERFMON": 38, "CAP_BPF": 39,
	"CAP_CHECKPOINT_RESTORE": 40,
}

// rlimitIndex maps OCI rlimit names to resource constants.
var rlimitIndex = map[string]int{
	"CPU": unix.RLIMIT_CPU, "FSIZE": unix.RLIMIT_FSIZE, "DATA": unix.RLIMIT_DATA,
	"STACK": unix.RLIMIT_STACK, "CORE": unix.RLIMIT_CORE, "RSS": unix.RLIMIT_RSS,
	"NPROC": unix.RLIMIT_NPROC, "NOFILE": unix.RLIMIT_NOFILE, "MEMLOCK": unix.RLIMIT_MEMLOCK,
	"AS": unix.RLIMIT_AS, "LOCKS": unix.RLIMIT_LOCKS, "SIGPENDING": unix.RLIMIT_SIGPENDING,
	"MSGQUEUE": unix.RLIMIT_MSGQUEUE, "NICE": unix.RLIMIT_NICE, "RTPRIO": unix.RLIMIT_RTPRIO,
	"RTTIME": unix.RLIMIT_RTTIME,
}

// resolveExecPath resolves argv[0] against the container's PATH (post-pivot
// the current root IS the container; the kernel does no PATH search).
func resolveExecPath(spec *specs.Spec) (string, error) {
	argv := spec.Process.Args
	if len(argv) == 0 {
		return "", fmt.Errorf("no process args")
	}
	if strings.ContainsRune(argv[0], '/') {
		return argv[0], nil
	}
	var tried []string
	for _, e := range spec.Process.Env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			for _, dir := range filepath.SplitList(v) {
				if dir == "" {
					dir = "."
				}
				p := filepath.Join(dir, argv[0])
				if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
					return p, nil
				} else if err != nil {
					tried = append(tried, fmt.Sprintf("%s: %v", p, err))
				} else {
					tried = append(tried, fmt.Sprintf("%s: mode %v", p, st.Mode()))
				}
			}
			break
		}
	}
	return "", fmt.Errorf("exec %q: executable file not found in $PATH (tried %v)", argv[0], tried)
}

// argvBytePtrs / envBytePtrs build NUL-terminated pointer arrays for execve.
func argvBytePtrs(argv []string) ([]*byte, error) {
	out := make([]*byte, 0, len(argv)+1)
	for _, a := range argv {
		p, err := unix.BytePtrFromString(a)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return append(out, nil), nil
}

func envBytePtrs(env []string) ([]*byte, error) {
	return argvBytePtrs(env)
}
