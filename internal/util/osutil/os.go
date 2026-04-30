package osutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// EnvName represents the name of an environment variable.
type EnvName string

// IsSet reports whether the environment variable is present.
func (e EnvName) IsSet() bool {
	_, ok := os.LookupEnv(string(e))
	return ok
}

// Get retrieves the environment variable's value.
func (e EnvName) Get() string {
	return os.Getenv(string(e))
}

// GetOr returns the environment variable's value, or fallback if unset.
func (e EnvName) GetOr(fallback string) string {
	if v := os.Getenv(string(e)); v != "" {
		return v
	}
	return fallback
}

// AsBool parses the environment variable as a boolean.
func (e EnvName) AsBool() bool {
	ok, _ := strconv.ParseBool(e.Get())
	return ok
}

// AppendPath joins the current value with an additional path segment.
func (e EnvName) AppendPath(segment string) string {
	if v := e.Get(); v != "" {
		return v + string(os.PathListSeparator) + segment
	}
	return segment
}

const BinaryEnv = "anvil_BINARY"

// SelfPath returns the absolute path of the currently running executable.
func SelfPath() string {
	path, err := resolveExecutable(os.Args[0])
	if err != nil {
		logrus.Traceln(fmt.Errorf("cannot detect executable: %v", err))
		logrus.Traceln("falling back to os.Args[0]")
		return os.Args[0]
	}
	return path
}

func resolveExecutable(arg0 string) (string, error) {
	// Allow override via environment variable for nested processes.
	if override := os.Getenv(BinaryEnv); override != "" {
		return override, nil
	}

	if filepath.IsAbs(arg0) {
		return arg0, nil
	}

	lookup, err := exec.LookPath(arg0)
	if err != nil {
		return "", fmt.Errorf("lookpath failed for '%s': %w", arg0, err)
	}

	abs, err := filepath.Abs(lookup)
	if err != nil {
		return "", fmt.Errorf("abs failed for '%s': %w", lookup, err)
	}

	return abs, nil
}

// SockPath is a Unix domain socket path.
type SockPath string

// URI returns the socket as a unix:// URI.
func (s SockPath) URI() string { return "unix://" + s.Path() }

// Path returns the filesystem path, stripping any unix:// prefix.
func (s SockPath) Path() string { return strings.TrimPrefix(string(s), "unix://") }
