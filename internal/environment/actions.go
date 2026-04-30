package environment

import (
	"io"
	"os"
)

type runActions interface {
	// Run runs command
	Run(args ...string) error
	// RunQuiet runs command whilst suppressing the output.
	// Useful for commands that only the exit code matters.
	RunQuiet(args ...string) error
	// RunOutput runs command and returns its output.
	RunOutput(args ...string) (string, error)
	// RunInteractive runs command interactively.
	RunInteractive(args ...string) error
	// RunWith runs with stdin and stdout.
	RunWith(stdin io.Reader, stdout io.Writer, args ...string) error
}

type fileActions interface {
	Read(fileName string) (string, error)
	Write(fileName string, body []byte) error
	Stat(fileName string) (os.FileInfo, error)
}
