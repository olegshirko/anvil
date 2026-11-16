package specutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// SafeJoin joins dest under root, refusing escapes.
func SafeJoin(root, dest string) (string, error) {
	p := filepath.Join(root, filepath.Clean("/"+dest))
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("mount destination %q escapes rootfs", dest)
	}
	return p, nil
}

// Device is one device node the init creates in the container's /dev.
type Device struct {
	Path string // relative to /dev
	Mode uint32 // permission bits only
	Dev  uint64 // Mkdev-encoded device number
}

// StandardDevices are created inside the container's /dev unless already
// present (runc's createDevices).
var StandardDevices = []Device{
	{"null", 0o666, Mkdev(1, 3)},
	{"zero", 0o666, Mkdev(1, 5)},
	{"full", 0o666, Mkdev(1, 7)},
	{"random", 0o666, Mkdev(1, 8)},
	{"urandom", 0o666, Mkdev(1, 9)},
	{"tty", 0o666, Mkdev(5, 0)},
}

// Mkdev encodes a device number (glibc's makedev for <256 majors).
func Mkdev(major, minor uint64) uint64 {
	return minor | (major << 8)
}

// CgroupNamespacePrivate reports whether the spec gives the container its own
// cgroup namespace (a pathless CgroupNamespace entry). That is what makes a
// freshly mounted cgroup2 hierarchy root at the container's cgroup; a shared
// namespace (--cgroupns host: no entry) keeps the guest-wide view.
func CgroupNamespacePrivate(spec *specs.Spec) bool {
	if spec.Linux == nil {
		return false
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.CgroupNamespace {
			return ns.Path == ""
		}
	}
	return false
}
