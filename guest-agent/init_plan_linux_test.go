//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/olegshirko/anvil/guest-agent/internal/specutil"
)

// planReader is a minimal mirror of the native helper's plan parser: the test
// decodes what anvil-init-native decodes, so a writer/reader drift fails here
// rather than in a container start.
type planReader struct {
	t     *testing.T
	buf   []byte
	off   int
	end   int
	ops   []uint32
	strs  []string
	attrs []uint32
}

func (r *planReader) u32() uint32 {
	r.t.Helper()
	if r.end-r.off < 4 {
		r.t.Fatalf("plan truncated reading u32 (off %d end %d)", r.off, r.end)
	}
	v := binary.LittleEndian.Uint32(r.buf[r.off:])
	r.off += 4
	return v
}

func (r *planReader) u64() uint64 {
	r.t.Helper()
	if r.end-r.off < 8 {
		r.t.Fatalf("plan truncated reading u64")
	}
	v := binary.LittleEndian.Uint64(r.buf[r.off:])
	r.off += 8
	return v
}

func (r *planReader) str() string {
	r.t.Helper()
	n := int(r.u32())
	if r.off+n+1 > r.end {
		r.t.Fatalf("plan truncated reading string")
	}
	s := string(r.buf[r.off : r.off+n])
	if r.buf[r.off+n] != 0 {
		r.t.Fatalf("plan string %q not NUL-terminated", s)
	}
	r.off += n + 1
	r.strs = append(r.strs, s)
	return s
}

// sub returns a reader over the next ln bytes without advancing this one.
func (r *planReader) sub(ln int) *planReader {
	return &planReader{t: r.t, buf: r.buf, off: r.off, end: r.off + ln}
}

// walk consumes the whole plan and records op kinds, strings and the leading
// attribute words.
func (r *planReader) walk() {
	r.t.Helper()
	if magic, version, total := r.u32(), r.u32(), r.u32(); magic != planMagic || version != planVersion || total != uint32(len(r.buf)) {
		r.t.Fatalf("header = %#x/%d/%d, want %#x/%d/%d", magic, version, total, planMagic, planVersion, len(r.buf))
	}
	for r.off < r.end {
		typ, ln := r.u32(), int(r.u32())
		if r.off+ln > r.end {
			r.t.Fatalf("section %d truncated", typ)
		}
		s := r.sub(ln)
		switch typ {
		case planSecOp:
			kind := s.u32()
			r.ops = append(r.ops, kind)
			switch kind {
			case planOpMkdirAll, planOpTouch, planOpRemove, planOpMask, planOpReadonly, planOpPivot, planOpSethostname, planOpChdir:
				s.str()
			case planOpMount:
				s.u32()
				s.str()
				s.str()
				s.str()
				s.str()
			case planOpMknod, planOpMknodOptional:
				s.u32()
				s.u64()
				s.str()
			case planOpSymlink, planOpWrite:
				s.str()
				s.str()
			case planOpRootReadonly:
			default:
				r.t.Fatalf("unknown op kind %d", kind)
			}
		case planSecCgroup:
			s.str()
		case planSecNS:
			s.u32()
			if s.u32() != 0 {
				s.str()
			}
		case planSecArgv, planSecEnv:
			for s.off < s.end {
				s.str()
			}
		case planSecAttrs:
			for i := 0; i < 6; i++ {
				r.attrs = append(r.attrs, s.u32())
			}
			nr := s.u32()
			for i := uint32(0); i < nr; i++ {
				s.u32()
				s.u64()
				s.u64()
			}
			ng := s.u32()
			for i := uint32(0); i < ng; i++ {
				s.u32()
			}
			s.u32() // hasCaps
			for i := 0; i < 5; i++ {
				s.u64()
			}
			ns := s.u32()
			for i := uint32(0); i < ns; i++ {
				s.u64()
			}
		default:
			r.t.Fatalf("unknown section %d", typ)
		}
		if s.off != s.end {
			r.t.Fatalf("section %d has %d unread bytes", typ, s.end-s.off)
		}
		r.strs = append(r.strs, s.strs...)
		r.off += ln
	}
}

func testSpec() *specs.Spec {
	uid := uint32(1000)
	limit := int64(512 << 20)
	return &specs.Spec{
		Hostname: "box",
		Root:     &specs.Root{Path: "rootfs", Readonly: true},
		Process: &specs.Process{
			User: specs.User{UID: uid, GID: uid, AdditionalGids: []uint32{10, 20}},
			Args: []string{"sh", "-c", "true"},
			Env:  []string{"PATH=/usr/bin:/bin", "HOME=/root"},
			Cwd:  "/work",
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  []string{"CAP_CHOWN", "CAP_SETUID", "CAP_NET_BIND_SERVICE"},
				Permitted: []string{"CAP_CHOWN", "CAP_SETUID"},
				Effective: []string{"CAP_CHOWN"},
				Ambient:   []string{"CAP_CHOWN"},
			},
			Rlimits: []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Soft: 1024, Hard: 2048}},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.NetworkNamespace, Path: "/var/run/netns/x"},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
				{Type: specs.CgroupNamespace},
			},
			MaskedPaths:   []string{"/proc/kcore", "/proc/keys"},
			ReadonlyPaths: []string{"/proc/sys"},
			Sysctl:        map[string]string{"net.ipv4.ip_forward": "1", "kernel.domainname": "anvil"},
			Devices: []specs.LinuxDevice{
				{Path: "/dev/fuse", Type: "c", Major: 10, Minor: 229, FileMode: ptrMode(0o666)},
			},
			CgroupsPath: "/ns/id",
			Resources:   &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &limit}},
			Seccomp: &specs.LinuxSeccomp{
				DefaultAction: specs.ActErrno,
				Syscalls: []specs.LinuxSyscall{
					{Names: []string{"read", "write", "exit_group", "execve"}, Action: specs.ActAllow},
				},
			},
		},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/etc/hosts", Type: "bind", Source: "/run/anvil/hosts", Options: []string{"bind", "ro"}},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755"}},
			{Destination: "/sys/fs/cgroup", Type: "cgroup", Options: []string{"ro", "nosuid", "noexec", "relatime"}},
		},
	}
}

func ptrMode(m uint32) *os.FileMode {
	mode := os.FileMode(m)
	return &mode
}

func buildTestPlan(t *testing.T) ([]byte, *specs.Spec, string) {
	t.Helper()
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	// The bind source must be a regular file, as in the real /etc/hosts bind.
	spec.Mounts[1].Source = hosts
	bundle := filepath.Join(dir, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs"), 0o711); err != nil {
		t.Fatal(err)
	}
	planPath, err := buildInitPlan(spec, bundle)
	if err != nil {
		t.Fatalf("buildInitPlan: %v", err)
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	return data, spec, bundle
}

func TestBuildInitPlanSerializes(t *testing.T) {
	data, _, bundle := buildTestPlan(t)
	r := &planReader{t: t, buf: data, end: len(data)}
	r.walk()

	idx := func(kind uint32) []int {
		var out []int
		for i, k := range r.ops {
			if k == kind {
				out = append(out, i)
			}
		}
		return out
	}

	// Mountpoint preparation: the file bind gets a mkdir of its parent plus a
	// touch and two mounts (bind + remount); the others a mkdir and one mount.
	wantPrefix := []uint32{planOpMkdirAll, planOpMount,
		planOpMkdirAll, planOpTouch, planOpMount, planOpMount,
		planOpMkdirAll, planOpMount,
		planOpMkdirAll, planOpMount}
	for i, want := range wantPrefix {
		if i >= len(r.ops) || r.ops[i] != want {
			t.Fatalf("op %d = %v, want %d (ops %v)", i, r.ops, want, r.ops)
		}
	}

	pivot := idx(planOpPivot)
	writes := idx(planOpWrite)
	hostname := idx(planOpSethostname)
	chdir := idx(planOpChdir)
	mounts := idx(planOpMount)
	if len(pivot) != 1 || len(writes) != 2 || len(hostname) != 1 || len(chdir) != 1 {
		t.Fatalf("counts pivot=%v writes=%v hostname=%v chdir=%v", pivot, writes, hostname, chdir)
	}
	for _, m := range mounts {
		if m > pivot[0] {
			t.Fatalf("mount op at %d after pivot %d", m, pivot[0])
		}
	}
	for _, w := range writes {
		if w < pivot[0] {
			t.Fatalf("sysctl write at %d before pivot %d", w, pivot[0])
		}
	}
	if hostname[0] < pivot[0] || chdir[0] < hostname[0] {
		t.Fatalf("hostname/chdir out of order: pivot=%d hostname=%d chdir=%d", pivot[0], hostname[0], chdir[0])
	}
	ro := idx(planOpRootReadonly)
	if len(ro) != 1 || ro[0] < pivot[0] || ro[0] > writes[0] {
		t.Fatalf("root-ro op at %v, pivot=%d first write=%d", ro, pivot[0], writes[0])
	}

	// Attributes: nnps=0, nonroot=1, uid/gid=1000.
	if got := r.attrs[2:6]; got[0] != 0 || got[1] != 1 || got[2] != 1000 || got[3] != 1000 {
		t.Fatalf("attrs nnps/nonroot/uid/gid = %v", got)
	}

	// argv/env/cwd and the rootfs path survive verbatim.
	for _, want := range []string{"PATH=/usr/bin:/bin", "sh", "/work", filepath.Join(bundle, "rootfs")} {
		if !containsStr(r.strs, want) {
			t.Fatalf("plan strings missing %q: %v", want, r.strs)
		}
	}
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func TestPlanRejectsUnexpressible(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs"), 0o711); err != nil {
		t.Fatal(err)
	}
	// A mount namespace joined by path cannot be expressed (the helper refuses
	// it too).
	spec := &specs.Spec{
		Process: &specs.Process{Args: []string{"true"}},
		Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
			{Type: specs.MountNamespace, Path: "/proc/1/ns/mnt"},
		}},
	}
	if _, err := buildInitPlan(spec, bundle); err == nil {
		t.Fatal("mount namespace by path: want error")
	}
	// An argument with an embedded NUL cannot become a C string.
	spec = &specs.Spec{Process: &specs.Process{Args: []string{"a\x00b"}}}
	if _, err := buildInitPlan(spec, bundle); err == nil {
		t.Fatal("NUL in argv: want error")
	}
}

func TestPlanSeccompMatchesAssembled(t *testing.T) {
	data, _, _ := buildTestPlan(t)
	sc := testSpec().Linux.Seccomp
	want, err := specutil.BuildSeccompFilter(sc)
	if err != nil {
		t.Fatal(err)
	}
	// Find the attrs section and its trailing instruction array.
	r := &planReader{t: t, buf: data, end: len(data)}
	r.u32()
	r.u32()
	r.u32()
	var prog []uint64
	for r.off < r.end {
		typ, ln := r.u32(), int(r.u32())
		if typ == planSecAttrs {
			a := r.sub(ln)
			for i := 0; i < 6; i++ {
				a.u32()
			}
			nr := a.u32()
			for i := uint32(0); i < nr; i++ {
				a.u32()
				a.u64()
				a.u64()
			}
			ng := a.u32()
			for i := uint32(0); i < ng; i++ {
				a.u32()
			}
			a.u32()
			for i := 0; i < 5; i++ {
				a.u64()
			}
			ns := a.u32()
			for i := uint32(0); i < ns; i++ {
				prog = append(prog, a.u64())
			}
		}
		r.off += ln
	}
	if len(prog) != len(want) {
		t.Fatalf("plan has %d seccomp instructions, want %d", len(prog), len(want))
	}
	for i, f := range want {
		got := uint64(f.Code) | uint64(f.Jt)<<16 | uint64(f.Jf)<<24 | uint64(f.K)<<32
		if prog[i] != got {
			t.Fatalf("instruction %d = %#x, want %#x", i, prog[i], got)
		}
	}
	if !bytes.Equal(data[:4], []byte("ANPL")) {
		t.Fatalf("magic = %q", data[:4])
	}
}
