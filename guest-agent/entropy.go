package main

import (
	"fmt"
	"os"
)

// seedEntropy credits the kernel entropy pool with the contents of the given
// file (written by vz-runner at VM start). Writing to /dev/urandom alone does
// not credit entropy; the ioctl does, completing crng init instantly.
func seedEntropy(path string) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(buf) == 0 {
		return fmt.Errorf("empty seed file %s", path)
	}
	if len(buf) > 256 {
		buf = buf[:256]
	}

	// rand_pool_info: entropy_count (bits) + buf_size + inline buffer.
	info := make([]byte, 8+len(buf))
	bits := uint32(len(buf) * 8)
	info[0] = byte(bits)
	info[1] = byte(bits >> 8)
	info[2] = byte(bits >> 16)
	info[3] = byte(bits >> 24)
	size := uint32(len(buf))
	info[4] = byte(size)
	info[5] = byte(size >> 8)
	copy(info[8:], buf)

	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	return rndAddEntropyIoctl(f, info)
}
