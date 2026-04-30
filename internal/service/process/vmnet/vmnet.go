package vmnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"anvil/internal/cli"
	"anvil/internal/service/process"
	"anvil/internal/util/osutil"

	"github.com/sirupsen/logrus"
)

const Name = "vmnet"

const (
	SubProcessEnvVar = "anvil_VMNET"

	SharedGateway = "192.168.106.1"
	SharedDHCPEnd = "192.168.106.254"
)

var _ process.Process = (*vmnetService)(nil)

// NewService creates a vmnet background process.
func NewService(mode, netInterface, profileShortName string) process.Process {
	return &vmnetService{
		mode:             mode,
		netInterface:     netInterface,
		profileShortName: profileShortName,
	}
}

type vmnetService struct {
	mode             string
	netInterface     string
	profileShortName string
}

// Alive verifies the vmnet process and socket are active.
func (s *vmnetService) Alive(ctx context.Context) error {
	info := ServicePaths(s.profileShortName)
	socketFile := info.Socket.Path()
	pidFile := info.PidFile

	if _, err := os.Stat(socketFile); err != nil {
		return fmt.Errorf("vmnet socket missing: %w", err)
	}
	conn, err := net.Dial("unix", socketFile)
	if err != nil {
		return fmt.Errorf("vmnet socket unreachable: %w", err)
	}
	if err := conn.Close(); err != nil {
		logrus.Debugln("error closing vmnet health socket:", err)
	}

	if _, err := os.Stat(pidFile); err == nil {
		// #nosec G204 — hard-coded system command for health check.
		cmd := exec.CommandContext(ctx, "sudo", "/usr/bin/pkill", "-0", "-F", pidFile)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("vmnet process check failed: %w", err)
		}
	}
	return nil
}

// Name implements process.Process.
func (*vmnetService) Name() string { return Name }

// Start launches the vmnet binary and waits until it exits or ctx is cancelled.
func (s *vmnetService) Start(ctx context.Context) error {
	info := ServicePaths(s.profileShortName)
	socket := info.Socket.Path()
	pid := info.PidFile

	_ = cleanupSocket(socket)

	done := make(chan error, 1)
	go func() {
		cmd := cli.Exec("sudo", s.buildArguments(socket, pid)...).Interactive()
		if cli.Settings.Verbose {
			cmd.Env = append(cmd.Env, os.Environ()...)
			cmd.Env = append(cmd.Env, "DEBUG=1")
		}
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("vmnet process exited: %w", err)
		}
	case <-ctx.Done():
		if err := signalStop(pid); err != nil {
			return fmt.Errorf("cannot stop vmnet: %w", err)
		}
	}
	return nil
}

// buildArguments assembles the command-line flags for the vmnet binary.
func (s *vmnetService) buildArguments(socket, pid string) []string {
	if s.mode == "bridged" {
		return []string{
			BinaryPath,
			"--vmnet-mode", "bridged",
			"--socket-group", "staff",
			"--vmnet-interface", s.netInterface,
			"--pidfile", pid,
			socket,
		}
	}
	return []string{
		BinaryPath,
		"--vmnet-mode", "shared",
		"--socket-group", "staff",
		"--vmnet-gateway", SharedGateway,
		"--vmnet-dhcp-end", SharedDHCPEnd,
		"--pidfile", pid,
		socket,
	}
}

// Dependencies implements process.Process.
func (vmnetService) Dependencies() (deps []process.Dependency, root bool) {
	return []process.Dependency{
		sudoersEntry{},
		vmnetBinaries{},
		vmnetStateDir{},
	}, true
}

// signalStop kills the vmnet process via its pidfile.
func signalStop(pidFile string) error {
	if _, err := os.Stat(pidFile); err == nil {
		if err := cli.Exec("sudo", "/usr/bin/pkill", "-F", pidFile).Interactive().Run(); err != nil {
			return fmt.Errorf("cannot kill vmnet process: %w", err)
		}
	}
	return nil
}

// cleanupSocket removes the socket path if it is a regular file.
func cleanupSocket(name string) error {
	if stat, err := os.Stat(name); err == nil && !stat.IsDir() {
		return os.Remove(name)
	}
	return nil
}

// ServicePaths returns the pidfile and socket locations for a profile.
func ServicePaths(profileShortName string) struct {
	PidFile string
	Socket  osutil.SockPath
} {
	return struct {
		PidFile string
		Socket  osutil.SockPath
	}{
		PidFile: filepath.Join(stateDir(), "vmnet-"+profileShortName+".pid"),
		Socket:  osutil.SockPath(filepath.Join(process.Dir(), "vmnet.sock")),
	}
}
