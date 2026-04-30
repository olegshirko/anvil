package vmnet

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"anvil/internal/embedded"
	"anvil/internal/environment"
	"anvil/internal/service/process"
)

var _ process.Dependency = sudoersEntry{}

type sudoersEntry struct{}

// Present reports whether the anvil sudoers file is already installed.
func (s sudoersEntry) Present() bool {
	data, err := os.ReadFile(s.target())
	if err != nil {
		return false
	}
	expected, err := embedded.Read(s.asset())
	if err != nil {
		return false
	}
	return bytes.Contains(data, expected)
}

func (s sudoersEntry) target() string { return "/etc/sudoers.d/anvil" }
func (s sudoersEntry) asset() string  { return "network/sudo.txt" }

// Apply writes the embedded sudoers template to the host.
func (s sudoersEntry) Apply(host environment.HostActions) error {
	txt, err := embedded.ReadString(s.asset())
	if err != nil {
		return fmt.Errorf("cannot read embedded sudoers template: %w", err)
	}
	if err := host.RunInteractive("sudo", "mkdir", "-p", filepath.Dir(s.target())); err != nil {
		return fmt.Errorf("cannot prepare sudoers directory: %w", err)
	}
	out := &bytes.Buffer{}
	if err := host.RunWith(strings.NewReader(txt), out, "sudo", "sh", "-c", "cat > "+s.target()); err != nil {
		return fmt.Errorf("cannot write sudoers file (stderr: %s): %w", out.String(), err)
	}
	return nil
}

var _ process.Dependency = vmnetBinaries{}

const (
	BinaryPath       = "/opt/anvil/bin/socket_vmnet"
	ClientBinaryPath = "/opt/anvil/bin/socket_vmnet_client"
)

type vmnetBinaries struct{}

// Present reports whether both vmnet binaries exist on disk.
func (v vmnetBinaries) Present() bool {
	for _, p := range v.paths() {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

func (v vmnetBinaries) paths() []string {
	return []string{BinaryPath, ClientBinaryPath}
}

// Apply extracts the embedded vmnet tarball to /opt/anvil.
func (v vmnetBinaries) Apply(host environment.HostActions) error {
	arch := "x86_64"
	if runtime.GOARCH != "amd64" {
		arch = "arm64"
	}
	data, err := embedded.Read("network/vmnet_" + arch + ".tar.gz")
	if err != nil {
		return fmt.Errorf("cannot read embedded vmnet archive: %w", err)
	}

	tmp, err := os.CreateTemp("", "vmnet-*.tar.gz")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("cannot write temp file: %w", err)
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := host.RunInteractive("sudo", "mkdir", "-p", installPrefix); err != nil {
		return fmt.Errorf("cannot create install directory: %w", err)
	}
	if err := host.RunInteractive("sudo", "sh", "-c", fmt.Sprintf("cd %s && tar xfz %s 2>/dev/null", installPrefix, tmp.Name())); err != nil {
		return fmt.Errorf("cannot extract vmnet archive: %w", err)
	}
	return nil
}

var _ process.Dependency = vmnetStateDir{}

type vmnetStateDir struct{}

// Apply creates the runtime state directory for vmnet.
func (v vmnetStateDir) Apply(host environment.HostActions) error {
	return host.RunInteractive("sudo", "mkdir", "-p", stateDir())
}

// Present reports whether the vmnet state directory exists.
func (v vmnetStateDir) Present() bool {
	st, err := os.Stat(stateDir())
	return err == nil && st.IsDir()
}

const installPrefix = "/opt/anvil"

// stateDir returns the path to the privileged service runtime directory.
func stateDir() string { return filepath.Join(installPrefix, "run") }
