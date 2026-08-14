//go:build linux

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// RNDADDENTROPY ioctl: struct rand_pool_info { __u32 entropy_count; __u32 buf_size; __u32 buf[]; }
const rndAddEntropy = 0x40085203

func rndAddEntropyIoctl(f *os.File, info []byte) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&info[0]))); errno != 0 {
		return fmt.Errorf("RNDADDENTROPY: %v", errno)
	}
	return nil
}
