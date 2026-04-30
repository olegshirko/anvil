package limautil

import (
	"bytes"
	"encoding/json"
	"fmt"

	"anvil/internal/store"
)

// DiskExists reports whether a lima-managed disk is present.
func DiskExists(name string) bool {
	var info struct {
		Name string `json:"name"`
	}
	cmd := Limactl("disk", "list", "--json", name)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return false
	}
	if err := json.NewDecoder(&out).Decode(&info); err != nil {
		return false
	}
	return info.Name == name
}

// FetchDiskSize returns the size of a lima disk in GiB.
func FetchDiskSize(name string) (int, error) {
	var info struct {
		Size int64 `json:"size"`
	}
	cmd := Limactl("disk", "list", "--json", name)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("cannot query disk size: %w", err)
	}
	if err := json.NewDecoder(&out).Decode(&info); err != nil {
		return 0, fmt.Errorf("cannot parse disk size: %w", err)
	}
	return int(info.Size / 1024 / 1024 / 1024), nil
}

// AllocateDisk creates a new lima disk with the requested size in GiB.
func AllocateDisk(name string, size int) error {
	var out bytes.Buffer
	cmd := Limactl("disk", "create", name, "--size", fmt.Sprintf("%dGiB", size))
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("disk creation failed: %w, output: %s", err, out.String())
	}
	return nil
}

// ExpandDisk grows (or shrinks) a lima disk to the new size in GiB.
func ExpandDisk(name string, size int) error {
	var out bytes.Buffer
	cmd := Limactl("disk", "resize", name, "--size", fmt.Sprintf("%dGiB", size))
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("disk resize failed: %w, output: %s", err, out.String())
	}
	return nil
}

// ReleaseDisk removes a lima disk.
func ReleaseDisk(name string) error {
	var out bytes.Buffer
	cmd := Limactl("disk", "delete", name)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("disk deletion failed: %w, output: %s", err, out.String())
	}
	return nil
}

// DiskMountPath is the guest mount path for a named disk.
func DiskMountPath(name string) string { return fmt.Sprintf("/mnt/lima-%s", name) }

// IsDiskReady checks whether the disk exists and has been formatted for the given runtime.
func IsDiskReady(name string, runtime string, storeFile string) bool {
	if !DiskExists(name) {
		return false
	}
	s, _ := store.Fetch(storeFile)
	return s.DiskFormatted && s.DiskRuntime == runtime
}
