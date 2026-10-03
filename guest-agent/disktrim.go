package main

import (
	"log"
	"strings"
	"sync"
	"time"
)

// The containerd disk is a sparse file on the Mac, and the VM's virtio-blk
// device passes discards through: trimming the guest file system punches the
// freed blocks out of the file. Trimming runs a while after the last delete
// (rm, rmi, volume rm, prune — containerd's GC frees content shortly after),
// so `docker system prune` gives the space back to macOS without
// `make disk-compact` and without the per-delete cost of a `discard` mount.

const diskTrimDelay = 30 * time.Second

var diskTrim struct {
	sync.Mutex
	timer *time.Timer
}

// scheduleDiskTrim (re)arms the trim timer.
func scheduleDiskTrim() {
	diskTrim.Lock()
	defer diskTrim.Unlock()
	if diskTrim.timer != nil {
		diskTrim.timer.Stop()
	}
	diskTrim.timer = time.AfterFunc(diskTrimDelay, runDiskTrim)
}

func runDiskTrim() {
	start := time.Now()
	n, err := trimFilesystem("/var/lib")
	if err != nil {
		log.Printf("[disk] trim /var/lib: %v", err)
		return
	}
	log.Printf("[disk] trimmed %d MB of /var/lib in %v", n>>20, time.Since(start).Round(time.Millisecond))
}

// freesDiskSpace reports whether a Docker API request deletes data.
func freesDiskSpace(method, path string) bool {
	return method == "DELETE" || (method == "POST" && strings.HasSuffix(path, "/prune"))
}
