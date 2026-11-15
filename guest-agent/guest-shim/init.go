//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// The init helper ("stage 1"): a fresh process the shim starts with exec.Cmd
// (a plain fork+exec, no unsafe Go-in-forked-child window). It joins the
// container's namespaces, moves into the prepared cgroup, applies the spec
// mounts and pivot_roots, then holds on the start gate. On start it applies
// the exec-time attributes (oom score, rlimits, no-new-privileges, user,
// capabilities, seccomp) and creates the container init process: a raw
// fork+execve window (pre-built pointers, syscalls only — the exact contract
// os/exec's forkAndExecInChild keeps) so the user process is PID 1 of the
// container's pid namespace without any Go code running inside the fork.
//
// Why not clone3 from the shim: Go code cannot run safely in a child of a
// multithreaded fork beyond raw syscalls, and clone3 cannot join namespaces
// that have paths (the pre-created netns). The unshare-based trampoline is
// the safe equivalent: one process creation into every namespace, the same
// work runc's nsexec bootstrap does, without the C constructor.
//
// Report protocol over fd 4 (one line each):
//
//	R\n          setup complete, holding at the gate
//	E <detail>\n setup failed (create fails)
//	S <pid>\n    init created and alive, <pid> is its global pid
//	X <detail>\n init creation failed (start fails)
//	D <code>\n   init exited with <code> (128+signal for signal deaths)

// Fixed fd numbers the shim hands over via exec.Cmd ExtraFiles (fds start
// at 3).
const (
	fdGate   = 3 // read end: "s" releases the created init, EOF exits it
	fdReport = 4 // write end: progress back to the shim
	fdStdin  = 5 // final stdin (fifo read end or console slave)
	fdStdout = 6 // final stdout (logger pipe or fifo write end)
	fdStderr = 7 // final stderr
)

// isInitHelper reports whether this process runs as the container init
// helper (second argument "init").
func isInitHelper() bool {
	return len(os.Args) >= 2 && os.Args[1] == "init"
}

// isExecHelper reports whether this process runs as an exec helper
// (second argument "exec-init").
func isExecHelper() bool {
	return len(os.Args) >= 2 && os.Args[1] == "exec-init"
}

// runInit is the init helper's main. Returns the process exit code.
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

	rootfs := filepath.Join(bundle, "rootfs")

	if err := joinNamespaces(spec); err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}
	if err := joinCgroup(spec); err != nil {
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
	if len(spec.Linux.Sysctl) > 0 {
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

	// Everything the user process must already carry is applied to this
	// process before the fork: the forked init inherits it all.
	if err := applyProcessAttrs(spec); err != nil {
		reportLine(reportStartPrefix+"%v", err)
		return 126
	}

	code, pid, startDetail, err := forkAndExecInit(spec)
	if err != nil {
		reportLine(reportStartPrefix+"%s", startDetail)
		return 127
	}
	// The container init is alive; report its global pid for the shim's
	// state (procfs translates NSpid across the pid namespace boundary).
	reportLine(reportStartedPrefix+"%d", pid)

	// Supervise: the stub is the parent of the container init (PID 1 of
	// the namespace). When it dies the namespace dies with it; report the
	// exit code first — the stub may be killed by the namespace teardown
	// right after.
	var ws unix.WaitStatus
	_, waitErr := unix.Wait4(pid, &ws, 0, nil)
	if waitErr != nil {
		code = 137 // lost the child (should not happen); kill-style code
	} else {
		code = int(exitStatus(ws))
	}
	reportLine(reportDonePrefix+"%d", code)
	return code // the shim's own reaper sees this as a fallback signal
}

// gateStartLine is the gate release message (written by the shim at start).
const gateStartLine = "s\n"

// Report line prefixes; the shim parses these over the report pipe.
const (
	reportReady         = "R\n"
	reportSetupPrefix   = "E " // setup failed: create fails
	reportStartPrefix   = "X " // init creation failed: start fails
	reportStartedPrefix = "S " // init created and alive, value = global pid
	reportDonePrefix    = "D " // init exited, value = exit code
)

// reportLine writes one line to the shim through the report fd.
func reportLine(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	f := os.NewFile(fdReport, "report")
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

// utsNamespaceShared reports a UTS namespace joined by path (host uts or
// container:<x>): the hostname is shared, so it must not be set.
func utsNamespaceShared(spec *specs.Spec) bool {
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.UTSNamespace && ns.Path != "" {
			return true
		}
	}
	return false
}

// joinNamespaces enters every namespace of the spec: pathed ones via setns
// (fds opened before any mount-ns switch), pathless ones via unshare. The
// mount namespace is unshared before the mount work; the cgroup namespace
// is unshared after the cgroup join in joinCgroup.
func joinNamespaces(spec *specs.Spec) error {
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
		case specs.CgroupNamespace:
			continue // handled in joinCgroup (after the cgroup switch)
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
		mnt bool
	}
	var fds []opened
	for _, p := range pathed {
		fd, err := unix.Open(p.path, unix.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", p.path, err)
		}
		fds = append(fds, opened{fd: fd, mnt: p.typ == specs.MountNamespace})
	}
	defer func() {
		for _, o := range fds {
			unix.Close(o.fd) //nolint:errcheck
		}
	}()
	for _, o := range fds {
		if !o.mnt {
			if err := unix.Setns(o.fd, 0); err != nil {
				return fmt.Errorf("setns: %w", err)
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
// already created it and wrote the spec's limits): the forked container
// init inherits it, which is the clone3(CLONE_INTO_CGROUP) equivalent for
// the trampoline design.
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
// forked init inherits it across the exec.
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
		if err := applySeccomp(spec.Linux.Seccomp); err != nil {
			return err
		}
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

// forkAndExecInit creates the container init: a raw fork whose child
// execve's the user process (PID 1 of the namespace). Between fork and
// execve the child executes raw syscalls only — pointers prepared here —
// the same discipline os/exec's forkAndExecInChild applies. A failed execve
// is reported through the report fd by the child itself.
func forkAndExecInit(spec *specs.Spec) (code int, pid int, detail string, err error) {
	argv0, err := resolveExecPath(spec)
	if err != nil {
		return 127, 0, err.Error(), err
	}
	// Pre-build every pointer the raw window needs: no allocation there.
	pathPtr, err := unix.BytePtrFromString(argv0)
	if err != nil {
		return 127, 0, "", err
	}
	argvPtrs, err := argvBytePtrs(spec.Process.Args)
	if err != nil {
		return 127, 0, "", err
	}
	envPtrs, err := envBytePtrs(spec.Process.Env)
	if err != nil {
		return 127, 0, "", err
	}
	args := pid0Args{
		reportFD:   fdReport,
		gateFD:     fdGate,
		pathPtr:    pathPtr,
		argvPtrs:   argvPtrs,
		envPtrs:    envPtrs,
		failMsg:    &failExecMessage[0],
		failMsgLen: len(failExecMessage),
	}

	// Raw fork. In the child only raw syscalls run (pid0RawChild).
	pid, errno := forkRaw()
	if errno != 0 {
		return 127, 0, "", fmt.Errorf("fork init: %w", errno)
	}
	if pid == 0 {
		pid0RawChild(args)
		// unreached
	}
	// Parent: did the child get to execve? A death within the window is an
	// exec failure (the child reports the generic detail itself; we surface
	// the code).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var ws unix.WaitStatus
		wpid, werr := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
		if werr == nil && wpid == pid {
			return 127, 0, fmt.Sprintf("init exited during start (code %d)", exitStatus(ws)), fmt.Errorf("exec failed")
		}
		if werr != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return 0, pid, "", nil
}

// pid0Args carries the pre-built pointers across the fork boundary.
type pid0Args struct {
	reportFD   int
	gateFD     int
	pathPtr    *byte
	argvPtrs   []*byte
	envPtrs    []*byte
	failMsg    *byte
	failMsgLen int
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
	for _, e := range spec.Process.Env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			for _, dir := range filepath.SplitList(v) {
				if dir == "" {
					dir = "."
				}
				p := filepath.Join(dir, argv[0])
				if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
					return p, nil
				}
			}
			break
		}
	}
	return "", fmt.Errorf("exec %q: executable file not found in $PATH", argv[0])
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
