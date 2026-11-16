// Package specutil holds the pure OCI-spec helpers shared by the container
// init helper and the guest agent's direct start path. The agent precomputes
// an init plan from the spec (mounts, devices, credentials, capabilities,
// seccomp) and either the Go helper or the native C helper executes it; one
// copy of the tables and flag conversions means the two paths cannot drift on
// capability bits, mount flags, rlimits or namespace flags.
package specutil

// CapabilityIndex maps OCI capability names to their kernel bit numbers
// (CAP_* constants; the list is stable).
var CapabilityIndex = map[string]int{
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

// CapabilityBits converts a ["CAP_SYS_ADMIN", ...] name list into a bitmask.
func CapabilityBits(names []string) uint64 {
	var out uint64
	for _, n := range names {
		if idx, ok := CapabilityIndex[n]; ok && idx < 64 {
			out |= 1 << idx
		}
	}
	return out
}
