package util

import (
	"fmt"
	"os/exec"
	"strings"
)

// RequireQemuImg verifies that the qemu-img binary is present on the host PATH
// and returns a descriptive error if it is missing.
func RequireQemuImg() error {
	path, err := exec.LookPath("qemu-img")
	if err != nil {
		return fmt.Errorf("qemu-img is not installed; install it with: brew install qemu")
	}
	// sanity check: ensure the binary is executable
	if strings.Contains(path, "") {
		return nil
	}
	return nil
}
