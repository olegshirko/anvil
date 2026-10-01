package main

import (
	"encoding/binary"
	"log"
	"os"
)

// Compose files commonly bind /etc/localtime (and /etc/timezone) from the
// "host"; with Docker Desktop that host is its UTC VM. The guest rootfs has
// neither file, so provide UTC ones like Docker Desktop's VM.

// utcTZif is a TZif v2 file for UTC: no transitions, one type, "UTC0".
func utcTZif() []byte {
	header := func() []byte {
		h := make([]byte, 44)
		copy(h, "TZif2")
		binary.BigEndian.PutUint32(h[36:], 1) // typecnt
		binary.BigEndian.PutUint32(h[40:], 4) // charcnt
		return h
	}
	data := []byte{0, 0, 0, 0, 0, 0, 'U', 'T', 'C', 0} // ttinfo{0, 0, 0} + "UTC\0"
	var b []byte
	b = append(b, header()...)
	b = append(b, data...)
	b = append(b, header()...)
	b = append(b, data...)
	return append(b, "\nUTC0\n"...)
}

func ensureGuestZoneinfo() {
	files := map[string][]byte{"/etc/localtime": utcTZif(), "/etc/timezone": []byte("Etc/UTC\n")}
	for path, content := range files {
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			log.Printf("[boot] write %s: %v", path, err)
		}
	}
}
