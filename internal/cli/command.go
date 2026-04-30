package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Settings holds global CLI configuration toggles.
var Settings = struct {
	// Verbose enables detailed output for executed commands.
	Verbose bool
}{}

// ShellExec prepares an external command for execution.
type ShellExec struct {
	ctx  context.Context
	args []string
}

// New creates a ShellExec for the given command and arguments.
// The command is not executed until Cmd() or Interactive() is called.
func Exec(command string, args ...string) *ShellExec {
	return &ShellExec{
		ctx:  context.Background(),
		args: append([]string{command}, args...),
	}
}

// WithContext attaches a context to the command for cancellation and timeouts.
func (s *ShellExec) WithContext(ctx context.Context) *ShellExec {
	s.ctx = ctx
	return s
}

// Cmd returns a prepared *exec.Cmd using the command's context.
func (s *ShellExec) Cmd() *exec.Cmd {
	if len(s.args) == 0 {
		return nil
	}
	c := exec.CommandContext(s.ctx, s.args[0], s.args[1:]...)
	c.Env = os.Environ()
	return c
}

// Interactive returns a command wired to the process Stdin, Stdout and Stderr.
func (s *ShellExec) Interactive() *exec.Cmd {
	c := s.Cmd()
	if c == nil {
		return nil
	}
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c
}

// Quote formats a slice of arguments as a single human-readable string.
func Quote(args []string) string {
	var sb strings.Builder
	for i, a := range args {
		if i > 0 {
			sb.WriteString(" ")
		}
		if strings.ContainsAny(a, " \t\n") {
			fmt.Fprintf(&sb, "%q", a)
		} else {
			sb.WriteString(a)
		}
	}
	return sb.String()
}

// ConfirmPrompt asks the user a yes/no question on stdin.
func ConfirmPrompt(question string) bool {
	fmt.Printf("%s [y/N] ", question)
	var resp string
	if _, err := fmt.Scanln(&resp); err != nil {
		return false
	}
	return resp == "y" || resp == "Y"
}
