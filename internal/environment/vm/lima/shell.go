package lima

import (
	"io"
)

func (l limaVM) Run(args ...string) error {
	args = append([]string{limactl, "shell", l.configService.Profile().ID}, args...)
	return l.host.Run(args...)
}

func (l limaVM) SSH(workingDir string, args ...string) error {
	shellArgs := []string{limactl, "shell"}
	if workingDir != "" {
		shellArgs = append(shellArgs, "--workdir", workingDir)
	}
	shellArgs = append(shellArgs, l.configService.Profile().ID)
	shellArgs = append(shellArgs, args...)

	return l.host.RunInteractive(shellArgs...)
}

func (l limaVM) RunInteractive(args ...string) error {
	args = append([]string{limactl, "shell", l.configService.Profile().ID}, args...)
	return l.host.RunInteractive(args...)
}

func (l limaVM) RunWith(stdin io.Reader, stdout io.Writer, args ...string) error {
	args = append([]string{limactl, "shell", l.configService.Profile().ID}, args...)
	return l.host.RunWith(stdin, stdout, args...)
}

func (l limaVM) RunOutput(args ...string) (out string, err error) {
	args = append([]string{limactl, "shell", l.configService.Profile().ID}, args...)
	return l.host.RunOutput(args...)
}

func (l limaVM) RunQuiet(args ...string) (err error) {
	args = append([]string{limactl, "shell", l.configService.Profile().ID}, args...)
	return l.host.RunQuiet(args...)
}
