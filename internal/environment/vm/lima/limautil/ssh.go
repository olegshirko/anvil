package limautil

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const sshConfigFileName = "ssh.config"

// SSHResult holds the processed SSH configuration for a profile.
type SSHResult struct {
	Output string
	File   struct {
		Lima  string
		anvil string
	}
}

// GenerateSSHConfig reads the Lima SSH config, replaces the Host directive,
// and returns both the modified config and relevant file paths.
func GenerateSSHConfig(profileID string, anvilSSHConfigFile string, limaInstanceDir string) (SSHResult, error) {
	var r SSHResult
	cfg := sshFile{profileID: profileID, limaInstanceDir: limaInstanceDir}

	content, err := cfg.read()
	if err != nil {
		return r, fmt.Errorf("cannot read ssh config: %w", err)
	}

	r.Output, err = patchHostLine(content, profileID)
	if err != nil {
		return r, fmt.Errorf("cannot patch ssh config: %w", err)
	}

	r.File.Lima = cfg.path()
	r.File.anvil = anvilSSHConfigFile
	return r, nil
}

func patchHostLine(conf string, profileID string) (string, error) {
	var out bytes.Buffer
	scanner := bufio.NewScanner(strings.NewReader(conf))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Host ") {
			line = "Host " + profileID
		}
		if _, err := fmt.Fprintln(&out, line); err != nil {
			return "", err
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return out.String(), nil
}

type sshFile struct {
	profileID       string
	limaInstanceDir string
}

func (s sshFile) path() string {
	return filepath.Join(s.limaInstanceDir, sshConfigFileName)
}

func (s sshFile) read() (string, error) {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return "", fmt.Errorf("cannot read Lima SSH config for profile '%s': %w", s.profileID, err)
	}
	return string(b), nil
}
