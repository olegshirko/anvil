//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/olegshirko/anvil/guest-agent/internal/cgroupv2"
	"github.com/olegshirko/anvil/guest-agent/internal/specutil"
)

// The init plan is the native helper's input: everything the container init
// has to do, precomputed from the OCI spec by the agent, so the helper needs
// no JSON parser, no seccomp assembler and no capability/rlimit tables at
// start. The Go helper keeps reading config.json; the agent writes one plan
// per native start into the bundle (tmpfs).
//
// Layout (little-endian): a 12-byte header {magic "ANPL", version, total
// length}, then sections {u32 type, u32 length, payload}. The op sections are
// ordered: the helper executes them in plan order. Strings are u32 length,
// the bytes, then a NUL byte (the helper passes the pointers to syscalls, and
// rejecting embedded NULs here keeps them C strings).
const (
	planMagic   = 0x4c504e41 // "ANPL"
	planVersion = 1

	planSecCgroup = 1
	planSecOp     = 5
	planSecArgv   = 6
	planSecEnv    = 7
	planSecNS     = 8
	planSecAttrs  = 9
)

// Plan op codes. Keep in sync with guest-shim/native/anvil-init.c.
const (
	planOpMkdirAll = iota + 1
	planOpTouch
	planOpMount
	planOpMknod
	planOpMknodOptional
	planOpSymlink
	planOpRemove
	planOpWrite
	planOpMask
	planOpReadonly
	planOpPivot
	planOpRootReadonly
	planOpSethostname
	planOpChdir
)

// Bounds mirrored by the C helper's static arrays; a spec beyond them takes
// the Go helper.
const (
	planMaxRlimits = 32
	planMaxGids    = 64
	planMaxArgs    = 63
	planMaxEnv     = 127
)

type planBuilder struct{ b []byte }

func (w *planBuilder) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *planBuilder) u64(v uint64) { w.b = binary.LittleEndian.AppendUint64(w.b, v) }

func (w *planBuilder) str(s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("string contains NUL: %q", s)
	}
	w.u32(uint32(len(s)))
	w.b = append(w.b, s...)
	w.b = append(w.b, 0)
	return nil
}

func (w *planBuilder) section(typ uint32, fn func(*planBuilder) error) error {
	var s planBuilder
	if err := fn(&s); err != nil {
		return err
	}
	w.u32(typ)
	w.u32(uint32(len(s.b)))
	w.b = append(w.b, s.b...)
	return nil
}

func (w *planBuilder) op(kind uint32, fn func(*planBuilder) error) error {
	return w.section(planSecOp, func(p *planBuilder) error {
		p.u32(kind)
		if fn == nil {
			return nil
		}
		return fn(p)
	})
}

func (w *planBuilder) opPath(kind uint32, path string) error {
	return w.op(kind, func(p *planBuilder) error { return p.str(path) })
}

// buildInitPlan serializes the init plan for spec into
// <bundle>/init-plan.bin and returns its path. Any construct the plan cannot
// express is an error; the caller then keeps the Go helper.
func buildInitPlan(spec *specs.Spec, bundle string) (string, error) {
	if spec == nil || spec.Process == nil {
		return "", fmt.Errorf("spec has no process")
	}
	if spec.Process.Terminal {
		// TTY containers are not direct-eligible (the containerd fast-shim
		// path owns their console); the native helper has no console setup.
		return "", fmt.Errorf("tty init is not supported by the native helper")
	}
	rootfs := filepath.Join(bundle, "rootfs")
	var w planBuilder
	w.u32(planMagic)
	w.u32(planVersion)
	totalAt := len(w.b)
	w.u32(0) // patched at the end

	if err := writePlanNamespaces(&w, spec); err != nil {
		return "", err
	}
	// The helper removes the cgroup at container exit, like the Go helper; the
	// agent created it for the CLONE_INTO_CGROUP directory fd.
	if spec.Linux != nil && spec.Linux.CgroupsPath != "" {
		cgPath := cgroupv2.AbsPath(spec.Linux.CgroupsPath)
		if err := w.section(planSecCgroup, func(p *planBuilder) error { return p.str(cgPath) }); err != nil {
			return "", err
		}
	}
	if err := writePlanOps(&w, spec, rootfs); err != nil {
		return "", err
	}
	if err := writePlanAttrs(&w, spec); err != nil {
		return "", err
	}
	if len(spec.Process.Args) > planMaxArgs {
		return "", fmt.Errorf("too many argv entries (%d)", len(spec.Process.Args))
	}
	if len(spec.Process.Env) > planMaxEnv {
		return "", fmt.Errorf("too many env entries (%d)", len(spec.Process.Env))
	}
	if err := w.section(planSecArgv, func(p *planBuilder) error {
		for _, a := range spec.Process.Args {
			if err := p.str(a); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return "", err
	}
	if err := w.section(planSecEnv, func(p *planBuilder) error {
		for _, e := range spec.Process.Env {
			if err := p.str(e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return "", err
	}

	binary.LittleEndian.PutUint32(w.b[totalAt:], uint32(len(w.b)))
	path := filepath.Join(bundle, "init-plan.bin")
	if err := os.WriteFile(path, w.b, 0o600); err != nil {
		return "", fmt.Errorf("write init plan: %w", err)
	}
	return path, nil
}

// writePlanNamespaces mirrors joinNamespacesApply: pathed entries are opened
// before any mount-namespace switch, the mount namespace is joined last, and
// the pathless set is unshared in one call. The helper reproduces that order
// from the list; the pid namespace is represented so it can be skipped.
func writePlanNamespaces(w *planBuilder, spec *specs.Spec) error {
	if spec.Linux == nil {
		return nil
	}
	for _, ns := range spec.Linux.Namespaces {
		flag, ok := specutil.NamespaceCloneFlag(ns.Type)
		if !ok {
			return fmt.Errorf("unsupported namespace %q", ns.Type)
		}
		if ns.Path != "" && (ns.Type == specs.PIDNamespace || ns.Type == specs.MountNamespace) {
			return fmt.Errorf("%s namespace joined by path is not supported", ns.Type)
		}
		if err := w.section(planSecNS, func(p *planBuilder) error {
			p.u32(uint32(flag))
			if ns.Path == "" {
				p.u32(0)
				return nil
			}
			p.u32(1)
			return p.str(ns.Path)
		}); err != nil {
			return err
		}
	}
	return nil
}

// writePlanOps emits the mount/device/pivot/sysctl/hostname/cwd ops in the
// exact order runInitApply performs them (the network namespace and /etc/hosts
// binds are already in place before the helper starts; seccomp is applied by
// the attrs, strictly after these).
func writePlanOps(w *planBuilder, spec *specs.Spec, rootfs string) error {
	// 1. Spec mounts, with the mountpoint prepared the way runc does it.
	for i := range spec.Mounts {
		m := &spec.Mounts[i]
		fileBind := false
		if m.Type == "bind" {
			if fi, err := os.Stat(m.Source); err == nil && !fi.IsDir() {
				fileBind = true
			}
		}
		dst, err := specutil.SafeJoin(rootfs, m.Destination)
		if err != nil {
			return err
		}
		if fileBind {
			if err := w.opPath(planOpMkdirAll, filepath.Dir(dst)); err != nil {
				return err
			}
			if err := w.opPath(planOpTouch, dst); err != nil {
				return err
			}
		} else if err := w.opPath(planOpMkdirAll, dst); err != nil {
			return err
		}
		flags, data := specutil.MountFlagOpts(m.Options)
		dataStr := strings.Join(data, ",")
		mountOp := func(src, fstype string, mflags int, mdata string) error {
			return w.op(planOpMount, func(p *planBuilder) error {
				p.u32(uint32(mflags))
				if err := p.str(src); err != nil {
					return err
				}
				if err := p.str(dst); err != nil {
					return err
				}
				if err := p.str(fstype); err != nil {
					return err
				}
				return p.str(mdata)
			})
		}
		switch m.Type {
		case "bind":
			bflags := unix.MS_BIND
			for _, o := range m.Options {
				if o == "rbind" {
					bflags |= unix.MS_REC
				}
			}
			if err := mountOp(m.Source, "", bflags, ""); err != nil {
				return err
			}
			if err := mountOp("", "", bflags|unix.MS_REMOUNT|flags, dataStr); err != nil {
				return err
			}
		case "cgroup":
			if !specutil.CgroupNamespacePrivate(spec) {
				if err := mountOp("/sys/fs/cgroup", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
					return err
				}
				if err := mountOp("", "", unix.MS_BIND|unix.MS_REC|unix.MS_REMOUNT|flags, ""); err != nil {
					return err
				}
				break
			}
			if err := mountOp("cgroup2", "cgroup2", flags, dataStr); err != nil {
				return err
			}
		default:
			src := m.Source
			if src == "" {
				src = m.Type
			}
			if err := mountOp(src, m.Type, flags, dataStr); err != nil {
				return err
			}
		}
	}

	// 2. Devices (standard nodes, --device nodes, /dev symlinks).
	devDir := filepath.Join(rootfs, "dev")
	for _, d := range specutil.StandardDevices {
		if err := devOp(w, planOpMknod, filepath.Join(devDir, d.Path), unix.S_IFCHR|d.Mode, d.Dev); err != nil {
			return err
		}
	}
	if spec.Linux != nil {
		for _, d := range spec.Linux.Devices {
			mode := uint32(0o666)
			if d.FileMode != nil {
				mode = uint32(*d.FileMode & 0o7777)
			}
			typ := uint32(unix.S_IFCHR)
			switch d.Type {
			case "b":
				typ = unix.S_IFBLK
			case "p":
				typ = unix.S_IFIFO
			}
			if err := devOp(w, planOpMknodOptional, filepath.Join(devDir, strings.TrimPrefix(d.Path, "/dev")),
				typ|mode, specutil.Mkdev(uint64(d.Major), uint64(d.Minor))); err != nil {
				return err
			}
		}
	}
	for _, l := range [][2]string{
		{"/proc/self/fd", "fd"},
		{"/proc/self/fd/0", "stdin"},
		{"/proc/self/fd/1", "stdout"},
		{"/proc/self/fd/2", "stderr"},
		{"pts/ptmx", "ptmx"},
	} {
		target, name := l[0], l[1]
		if err := w.op(planOpSymlink, func(p *planBuilder) error {
			if err := p.str(target); err != nil {
				return err
			}
			return p.str(filepath.Join(devDir, name))
		}); err != nil {
			return err
		}
	}

	// 3. Masked and read-only paths (their existence is checked by the helper
	// at execution time, after the mounts above).
	if spec.Linux != nil {
		for _, p := range spec.Linux.MaskedPaths {
			if dst, err := specutil.SafeJoin(rootfs, p); err == nil {
				if err := w.opPath(planOpMask, dst); err != nil {
					return err
				}
			}
		}
		for _, p := range spec.Linux.ReadonlyPaths {
			if dst, err := specutil.SafeJoin(rootfs, p); err == nil {
				if err := w.opPath(planOpReadonly, dst); err != nil {
					return err
				}
			}
		}
	}

	// 4. pivot_root into the prepared rootfs.
	if err := w.opPath(planOpPivot, rootfs); err != nil {
		return err
	}
	if spec.Root != nil && spec.Root.Readonly {
		if err := w.op(planOpRootReadonly, nil); err != nil {
			return err
		}
	}

	// 5. Sysctls (post-pivot: the proc mount of the container's namespaces).
	if spec.Linux != nil && len(spec.Linux.Sysctl) > 0 {
		keys := make([]string, 0, len(spec.Linux.Sysctl))
		for k := range spec.Linux.Sysctl {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			path := filepath.Join("/proc/sys", strings.ReplaceAll(k, ".", "/"))
			value := spec.Linux.Sysctl[k]
			if err := w.op(planOpWrite, func(p *planBuilder) error {
				if err := p.str(path); err != nil {
					return err
				}
				return p.str(value)
			}); err != nil {
				return err
			}
		}
	}

	// 6. Hostname (not for a shared UTS namespace) and cwd.
	if spec.Hostname != "" && !specutil.UTSNamespaceShared(spec) {
		if err := w.opPath(planOpSethostname, spec.Hostname); err != nil {
			return err
		}
	}
	if spec.Process.Cwd != "" {
		if err := w.opPath(planOpChdir, spec.Process.Cwd); err != nil {
			return err
		}
	}
	return nil
}

func devOp(w *planBuilder, kind uint32, path string, mode uint32, dev uint64) error {
	return w.op(kind, func(p *planBuilder) error {
		p.u32(mode)
		p.u64(dev)
		return p.str(path)
	})
}

// writePlanAttrs emits the exec-time attributes, mirroring applyProcessAttrs'
// order (credentials before caps, seccomp last — the helper applies the
// section in this order after the gate).
func writePlanAttrs(w *planBuilder, spec *specs.Spec) error {
	p := spec.Process
	var oom int32
	if p.OOMScoreAdj != nil {
		oom = int32(*p.OOMScoreAdj)
	}
	nonRoot := p.User.UID != 0 || p.User.GID != 0 || len(p.User.AdditionalGids) > 0
	if len(p.Rlimits) > planMaxRlimits {
		return fmt.Errorf("too many rlimits (%d)", len(p.Rlimits))
	}
	if len(p.User.AdditionalGids) > planMaxGids {
		return fmt.Errorf("too many additional gids (%d)", len(p.User.AdditionalGids))
	}
	var seccomp []unix.SockFilter
	if spec.Linux != nil && spec.Linux.Seccomp != nil {
		var err error
		seccomp, err = cachedSeccompFilter(spec.Linux.Seccomp)
		if err != nil {
			return err
		}
	}
	hasCaps := p.Capabilities != nil
	var caps [5]uint64
	if hasCaps {
		caps = [5]uint64{
			specutil.CapabilityBits(p.Capabilities.Bounding),
			specutil.CapabilityBits(p.Capabilities.Permitted),
			specutil.CapabilityBits(p.Capabilities.Effective),
			specutil.CapabilityBits(p.Capabilities.Inheritable),
			specutil.CapabilityBits(p.Capabilities.Ambient),
		}
	}
	return w.section(planSecAttrs, func(b *planBuilder) error {
		hasOOM := uint32(0)
		if p.OOMScoreAdj != nil {
			hasOOM = 1
		}
		b.u32(hasOOM)
		b.u32(uint32(oom))
		b.u32(boolU32(p.NoNewPrivileges))
		b.u32(boolU32(nonRoot))
		b.u32(p.User.UID)
		b.u32(p.User.GID)
		b.u32(uint32(len(p.Rlimits)))
		for _, rl := range p.Rlimits {
			b.u32(uint32(specutil.RlimitFromName(rl.Type)))
			b.u64(rl.Soft)
			b.u64(rl.Hard)
		}
		b.u32(uint32(len(p.User.AdditionalGids)))
		for _, g := range p.User.AdditionalGids {
			b.u32(g)
		}
		b.u32(boolU32(hasCaps))
		for _, mask := range caps {
			b.u64(mask)
		}
		b.u32(uint32(len(seccomp)))
		for _, f := range seccomp {
			b.u64(uint64(f.Code) | uint64(f.Jt)<<16 | uint64(f.Jf)<<24 | uint64(f.K)<<32)
		}
		return nil
	})
}

func boolU32(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}

// seccompCache keeps the last assembled filter. Direct-eligible containers all
// carry containerd's default profile, so after the first start this is a
// byte-compare of the marshaled profile instead of a ~0.3 ms assembly (the
// share of the Go helper's attribute phase, measured on the dev guest).
var seccompCache struct {
	mu   sync.Mutex
	key  []byte
	prog []unix.SockFilter
}

func cachedSeccompFilter(sc *specs.LinuxSeccomp) ([]unix.SockFilter, error) {
	key, err := json.Marshal(sc)
	if err != nil {
		return nil, fmt.Errorf("seccomp key: %w", err)
	}
	seccompCache.mu.Lock()
	defer seccompCache.mu.Unlock()
	if seccompCache.key != nil && bytes.Equal(seccompCache.key, key) {
		return seccompCache.prog, nil
	}
	prog, err := specutil.BuildSeccompFilter(sc)
	if err != nil {
		return nil, err
	}
	seccompCache.key, seccompCache.prog = key, prog
	return prog, nil
}
