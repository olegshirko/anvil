package lima

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"anvil/internal/environment"
)

// Read reads a file from the guest via sudo cat.
func (l limaVM) Read(path string) (string, error) {
	out, err := l.RunOutput("sudo", "cat", path)
	if err != nil {
		return "", fmt.Errorf("cannot read %q: %w", path, err)
	}
	return out, nil
}

// Write writes data to a file on the guest, creating parent directories as needed.
func (l *limaVM) Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := l.RunQuiet("sudo", "mkdir", "-p", dir); err != nil {
		return fmt.Errorf("cannot create directory %q: %w", dir, err)
	}
	stdin := bytes.NewReader(data)
	return l.RunWith(stdin, nil, "sudo", "sh", "-c", "cat > "+path)
}

// Stat returns file metadata for the given path inside the guest.
func (l *limaVM) Stat(path string) (os.FileInfo, error) {
	return probeGuestFile(l, path)
}

var _ os.FileInfo = (*guestFileInfo)(nil)

type guestFileInfo struct {
	dir     bool
	modTime time.Time
	mode    fs.FileMode
	name    string
	size    int64
}

// probeGuestFile parses `stat` output from the guest to build a FileInfo.
func probeGuestFile(guest environment.GuestActions, path string) (guestFileInfo, error) {
	var info guestFileInfo
	// format: size,permission,modtime,type
	raw, err := guest.RunOutput("sudo", "stat", "-c", "%s,%a,%Y,%F", path)
	if err != nil {
		return info, fileNotFound(path, err)
	}
	parts := strings.Split(raw, ",")
	if len(parts) < 4 {
		return info, fmt.Errorf("unexpected stat output for %q", path)
	}
	info.name = path
	info.size, _ = strconv.ParseInt(parts[0], 10, 64)
	mode, _ := strconv.ParseUint(parts[1], 10, 32)
	info.mode = fs.FileMode(mode)
	unix, _ := strconv.ParseInt(parts[2], 10, 64)
	info.modTime = time.Unix(unix, 0)
	info.dir = parts[3] == "directory"
	return info, nil
}

func fileNotFound(path string, err error) error {
	return fmt.Errorf("cannot stat %q: %w", path, err)
}

func (f guestFileInfo) IsDir() bool        { return f.dir }
func (f guestFileInfo) ModTime() time.Time { return f.modTime }
func (f guestFileInfo) Mode() fs.FileMode  { return f.mode }
func (f guestFileInfo) Name() string       { return f.name }
func (f guestFileInfo) Size() int64        { return f.size }
func (guestFileInfo) Sys() any             { return nil }
