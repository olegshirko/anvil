//go:build linux

package specutil

import (
	"strings"

	"golang.org/x/sys/unix"
)

// rlimitIndex maps OCI rlimit names to resource constants.
var rlimitIndex = map[string]int{
	"CPU": unix.RLIMIT_CPU, "FSIZE": unix.RLIMIT_FSIZE, "DATA": unix.RLIMIT_DATA,
	"STACK": unix.RLIMIT_STACK, "CORE": unix.RLIMIT_CORE, "RSS": unix.RLIMIT_RSS,
	"NPROC": unix.RLIMIT_NPROC, "NOFILE": unix.RLIMIT_NOFILE, "MEMLOCK": unix.RLIMIT_MEMLOCK,
	"AS": unix.RLIMIT_AS, "LOCKS": unix.RLIMIT_LOCKS, "SIGPENDING": unix.RLIMIT_SIGPENDING,
	"MSGQUEUE": unix.RLIMIT_MSGQUEUE, "NICE": unix.RLIMIT_NICE, "RTPRIO": unix.RLIMIT_RTPRIO,
	"RTTIME": unix.RLIMIT_RTTIME,
}

// RlimitFromName converts an OCI rlimit type ("RLIMIT_NOFILE") to a constant.
func RlimitFromName(t string) int {
	name := strings.TrimPrefix(strings.ToUpper(t), "RLIMIT_")
	if idx, ok := rlimitIndex[name]; ok {
		return idx
	}
	return 0
}
