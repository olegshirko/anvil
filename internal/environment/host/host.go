package host

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"anvil/internal/cli"
	"anvil/internal/environment"
	"anvil/internal/util/terminal"
)

// New returns a fresh HostActions implementation backed by the OS.
func New() environment.Host {
	return &hostRunner{}
}

var _ environment.Host = (*hostRunner)(nil)

// hostRunner executes commands on the host machine.
type hostRunner struct {
	env []string
	dir string
}

// copy returns an independent shallow copy with the same state.
func (h hostRunner) copy() hostRunner {
	return hostRunner{
		env: append([]string(nil), h.env...),
		dir: h.dir,
	}
}

// WithEnv returns a new runner that appends the given env vars.
func (h hostRunner) WithEnv(env ...string) environment.HostActions {
	next := h.copy()
	next.env = append(next.env, env...)
	return next
}

// WithDir returns a new runner bound to the given working directory.
func (h hostRunner) WithDir(dir string) environment.HostActions {
	next := h.copy()
	next.dir = dir
	return next
}

// buildCmd prepares an *exec.Cmd from the runner's state.
func (h hostRunner) buildCmd(args []string) *exec.Cmd {
	cmd := cli.Exec(args[0], args[1:]...).Cmd()
	cmd.Env = append(os.Environ(), h.env...)
	if h.dir != "" {
		cmd.Dir = h.dir
	}
	return cmd
}

// Run executes a command, capturing stdout/stderr through a verbose writer.
func (h hostRunner) Run(args ...string) error {
	if len(args) == 0 {
		return errors.New("no command specified")
	}

	cmd := h.buildCmd(args)

	lineHeight := 6
	if cli.Settings.Verbose {
		lineHeight = -1
	}
	vw := terminal.NewVerboseWriter(lineHeight)
	cmd.Stdout = vw
	cmd.Stderr = vw

	if err := cmd.Run(); err != nil {
		_ = vw.Close()
		return err
	}
	return vw.Close()
}

// RunQuiet executes a command suppressing all output. Only the exit code matters.
func (h hostRunner) RunQuiet(args ...string) error {
	if len(args) == 0 {
		return errors.New("no command specified")
	}

	cmd := h.buildCmd(args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return formatRunError(cmd.Args, stderr.String(), err)
	}
	return nil
}

// RunOutput executes a command and returns its trimmed stdout.
func (h hostRunner) RunOutput(args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("no command specified")
	}

	cmd := h.buildCmd(args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", formatRunError(cmd.Args, stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// RunInteractive executes a command attached to the user's terminal.
func (h hostRunner) RunInteractive(args ...string) error {
	if len(args) == 0 {
		return errors.New("no command specified")
	}

	cmd := cli.Exec(args[0], args[1:]...).Interactive()
	cmd.Env = append(os.Environ(), h.env...)
	if h.dir != "" {
		cmd.Dir = h.dir
	}
	return cmd.Run()
}

// RunWith executes a command with custom stdin and stdout.
func (h hostRunner) RunWith(stdin io.Reader, stdout io.Writer, args ...string) error {
	if len(args) == 0 {
		return errors.New("no command specified")
	}

	cmd := cli.Exec(args[0], args[1:]...).Interactive()
	cmd.Env = append(os.Environ(), h.env...)
	if h.dir != "" {
		cmd.Dir = h.dir
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return formatRunError(cmd.Args, stderr.String(), err)
	}
	return nil
}

// Env retrieves an environment variable.
func (h hostRunner) Env(s string) (string, error) {
	return os.Getenv(s), nil
}

// Read reads the contents of a file.
func (h hostRunner) Read(fileName string) (string, error) {
	b, err := os.ReadFile(fileName)
	return string(b), err
}

// Write writes data to a file.
func (h hostRunner) Write(fileName string, body []byte) error {
	return os.WriteFile(fileName, body, 0644)
}

// Stat returns file info.
func (h hostRunner) Stat(fileName string) (os.FileInfo, error) {
	return os.Stat(fileName)
}

// IsInstalled verifies that all listed dependencies exist in $PATH.
func IsInstalled(deps environment.Dependencies) error {
	missing, check := make([]string, 0), func(p string) bool {
		_, err := exec.LookPath(p)
		return err == nil
	}

	for _, p := range deps.Dependencies() {
		if !check(p) {
			missing = append(missing, p)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("%s not found, run 'brew install %s' to install",
			strings.Join(missing, ", "), strings.Join(missing, " "))
	}
	return nil
}

// formatRunError produces a consistent error message from a failed command.
func formatRunError(args []string, stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("command %q failed: %s", args, msg)
}
