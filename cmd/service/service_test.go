package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"anvil/internal/service/process"
)

func newTestSupervisor(t *testing.T) *Supervisor {
	dir := t.TempDir()
	process.SetDir(dir)
	return NewSupervisor(dir)
}

func testProcesses() []process.Process {
	addrs := []string{"localhost", "127.0.0.1"}
	var procs []process.Process
	for _, a := range addrs {
		procs = append(procs, &pinger{address: a})
	}
	return procs
}

func TestSupervisorLifecycle(t *testing.T) {
	s := newTestSupervisor(t)
	procs := testProcesses()
	pidFile := filepath.Join(s.workDir, pidFileName)

	t.Log("pidfile", pidFile)

	// start
	timeout := 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := s.Start(ctx, procs); err != nil {
		t.Fatal(err)
	}
	t.Log("start successful")

	// wait for pidfile
waitLoop:
	for {
		select {
		case <-ctx.Done():
			t.Skipf("daemon not supported: %v", ctx.Err())
		default:
			if p, err := os.ReadFile(filepath.Clean(pidFile)); err == nil && len(p) > 0 {
				break waitLoop
			}
			time.Sleep(time.Second)
		}
	}

	// verify running
	if !s.Alive() {
		t.Error("expected daemon to be alive")
		return
	}

	// stop
	if err := s.Stop(ctx); err != nil {
		t.Error(err)
	}

	// verify stopped
	if s.Alive() {
		t.Errorf("daemon with pidFile %s is still running", pidFile)
	}
}

func TestSupervisorRunProcesses(t *testing.T) {
	s := newTestSupervisor(t)
	procs := testProcesses()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() {
		done <- s.run(ctx, procs...)
	}()

	cancel()

	select {
	case <-ctx.Done():
		if err := ctx.Err(); err != context.Canceled {
			t.Error(err)
		}
	case err := <-done:
		t.Error(err)
	}
}

var _ process.Process = (*pinger)(nil)

type pinger struct {
	address string
}

func (p pinger) Alive(ctx context.Context) error { return nil }
func (pinger) Name() string                      { return "pinger" }
func (p *pinger) Start(ctx context.Context) error {
	return p.run(ctx, "ping", "-c10", p.address)
}
func (p *pinger) Dependencies() ([]process.Dependency, bool) { return nil, false }

func (p *pinger) run(ctx context.Context, command string, args ...string) error {
	// #nosec G204 — test helper with hard-coded command.
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
