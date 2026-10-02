package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// File changes made on the Mac reach the VM through virtiofs, but the guest
// kernel never sees them happen, so inotify inside containers stays silent:
// hot reload (Vite, webpack, nodemon, air, uvicorn --reload) missed every
// edit. The host watches the bind-mounted Mac directories with FSEvents and
// streams the changed paths here with their current times; re-applying
// those exact times (utimensat) raises IN_ATTRIB on the inode, which every
// watcher of the file or of its directory receives. Nothing changes on the
// Mac: the values are the ones the file already has.

// macShareRoots are where Mac paths live in the VM (same absolute paths).
var macShareRoots = []string{"/Users/", "/Volumes/", "/private/tmp/", "/private/var/folders/"}

func onMacShare(p string) bool {
	for _, r := range macShareRoots {
		if strings.HasPrefix(p, r) {
			return true
		}
	}
	return false
}

// containerWatchPaths lists the Mac paths a container bind-mounts
// (directories and single files).
func containerWatchPaths(ns, id string) []string {
	meta, err := loadContainerMeta(ns, id)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range meta.MountPoints {
		if m.Type != "bind" || !onMacShare(m.Source) {
			continue
		}
		out = append(out, filepath.Clean(m.Source))
	}
	return out
}

// minimalWatchPaths sorts, dedupes and drops paths inside another one (the
// host's FSEvents streams are recursive).
func minimalWatchPaths(paths []string) []string {
	slices.Sort(paths)
	var out []string
	for _, p := range paths {
		if n := len(out); n > 0 && (out[n-1] == p || strings.HasPrefix(p, out[n-1]+"/")) {
			continue
		}
		out = append(out, p)
	}
	return out
}

type fsEvent struct {
	Path  string `json:"p"`
	Atime int64  `json:"a"` // ns since the epoch
	Mtime int64  `json:"m"`
}

type fsEventBatch struct {
	Events []fsEvent `json:"events"`
}

// handleFSEvents serves an fs_events control connection: an ok response,
// then length-prefixed batches until the host closes it.
func handleFSEvents(conn net.Conn) {
	if err := writeResponse(conn, Response{Status: "ok"}); err != nil {
		return
	}
	for {
		var batch fsEventBatch
		if err := readFSEventFrame(conn, &batch); err != nil {
			if err != io.EOF {
				debugLog("[fsevents] %v", err)
			}
			return
		}
		for _, ev := range batch.Events {
			applyFSEvent(ev)
		}
	}
}

func applyFSEvent(ev fsEvent) {
	p := filepath.Clean(ev.Path)
	if !onMacShare(p) || ev.Mtime <= 0 {
		return
	}
	ts := []unix.Timespec{unix.NsecToTimespec(ev.Atime), unix.NsecToTimespec(ev.Mtime)}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil && !os.IsNotExist(err) {
		debugLog("[fsevents] %s: %v", p, err)
	}
}

// readFSEventFrame reads one length-prefixed JSON frame straight from conn
// (no buffered reader: one would swallow the next batch).
func readFSEventFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return io.EOF
		}
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > 4<<20 {
		return fmt.Errorf("bad fs_events frame length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		log.Printf("[fsevents] bad batch: %v", err)
		return nil
	}
	return nil
}
