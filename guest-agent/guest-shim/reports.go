package main

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// Report line prefixes the helpers write to the shim over the report pipe
// (one line each; see init.go for the full protocol): "R" setup complete;
// "P <pid>" the init process exists (its global pid, reported at create);
// "E <detail>" setup failed at create; "X <detail>" start failed; "D <code>"
// init exited; "S <pid>" user process started (global pid).
const (
	reportReady         = "R\n"
	reportSetupPrefix   = "E " // setup failed: create fails
	reportStartPrefix   = "X " // init creation failed: start fails
	reportStartedPrefix = "S " // init created and alive, value = global pid
	reportDonePrefix    = "D " // init exited, value = exit code
	reportInitPrefix    = "P " // init process born, value = global pid
)

// consumeSetupReport reads one setup-phase report line from the init
// helper's report stream: a "P <pid>" line (the init process was born at
// create — the pid the create RPC answers with), the "R" ready marker
// (mounts applied, init holding the gate) or a setup failure ("E <detail>",
// returned as the error). Anything else is a protocol violation. Create
// calls this in a loop until ready, before the report-reader goroutine
// takes the stream over.
func consumeSetupReport(buf *bufio.Reader) (pid int, ready bool, err error) {
	line, rerr := buf.ReadString('\n')
	if rerr != nil {
		if line == "" {
			return 0, false, fmt.Errorf("report EOF: %w", rerr)
		}
		return 0, false, fmt.Errorf("report EOF after %q: %w", strings.TrimRight(line, "\n"), rerr)
	}
	line = strings.TrimRight(line, "\n")
	switch {
	case line == strings.TrimRight(reportReady, "\n"):
		return 0, true, nil
	case strings.HasPrefix(line, reportInitPrefix):
		pid, perr := strconv.Atoi(strings.TrimPrefix(line, reportInitPrefix))
		if perr != nil || pid <= 0 {
			return 0, false, fmt.Errorf("bad init pid report %q", line)
		}
		return pid, false, nil
	case strings.HasPrefix(line, reportSetupPrefix):
		return 0, false, fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(line, reportSetupPrefix)))
	}
	return 0, false, fmt.Errorf("unexpected report line %q", line)
}
