//go:build linux

package specutil

import (
	"golang.org/x/sys/unix"
)

// MountFlagOpts maps fstab-style mount options to syscall flags and mount
// data. bind options are extracted separately.
func MountFlagOpts(opts []string) (int, []string) {
	var flags int
	var data []string
	for _, o := range opts {
		switch o {
		case "ro":
			flags |= unix.MS_RDONLY
		case "rw":
		case "nosuid":
			flags |= unix.MS_NOSUID
		case "nodev":
			flags |= unix.MS_NODEV
		case "noexec":
			flags |= unix.MS_NOEXEC
		case "relatime":
			flags |= unix.MS_RELATIME
		case "noatime":
			flags |= unix.MS_NOATIME
		case "nodiratime":
			flags |= unix.MS_NODIRATIME
		case "strictatime":
			flags |= unix.MS_STRICTATIME
		case "suid", "dev", "exec", "atime", "diratime", "lazytime":
			if o == "lazytime" {
				flags |= unix.MS_LAZYTIME
			}
		case "bind", "rbind", "rprivate", "rslave", "rshared", "private", "slave", "shared", "unbindable", "runbindable", "recursive", "norecursive", "copy=sloppy":
			// mount topology handled separately; nothing here
		default:
			data = append(data, o)
		}
	}
	return flags, data
}
