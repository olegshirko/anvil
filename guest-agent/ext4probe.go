package main

import (
	"encoding/binary"
	"os"
)

// ext4 (and ext2/3) superblocks start 1024 bytes into the device; the
// 16-bit magic 0xEF53 sits at offset 56 inside it.
const (
	ext4MagicOffset = 1024 + 56
	ext4Magic       = 0xEF53
)

// hasExt4Exit implements `guest-agent has-ext4 <device>`.
func hasExt4Exit(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 2
	}
	defer f.Close()
	ok, err := hasExt4Magic(f)
	switch {
	case err != nil:
		return 2
	case ok:
		return 0
	}
	return 1
}

// hasExt4Magic reports whether r carries an ext2/3/4 superblock magic.
func hasExt4Magic(r interface {
	ReadAt([]byte, int64) (int, error)
}) (bool, error) {
	var b [2]byte
	if _, err := r.ReadAt(b[:], ext4MagicOffset); err != nil {
		return false, err
	}
	return binary.LittleEndian.Uint16(b[:]) == ext4Magic, nil
}
