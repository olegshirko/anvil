package main

import (
	"fmt"
	"syscall"
)

// diskUsage answers the `df` control command for anvil doctor (the guest
// has no df binary): "<used%> <total bytes> <available bytes>".
func diskUsage(path string) Response {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Response{Error: err.Error(), ExitCode: 1}
	}
	total := st.Blocks * uint64(st.Bsize)
	avail := st.Bavail * uint64(st.Bsize)
	used := total - st.Bfree*uint64(st.Bsize)
	pct := 0
	if used+avail > 0 {
		pct = int((used*100 + used + avail - 1) / (used + avail)) // rounded up, as df does
	}
	return Response{Stdout: fmt.Sprintf("%d %d %d\n", pct, total, avail)}
}
