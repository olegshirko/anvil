package service

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"anvil/internal/cli"
	"anvil/internal/service/process"
	"anvil/internal/util/fsutil"

	godaemon "github.com/sevlyar/go-daemon"
	"github.com/sirupsen/logrus"
)

const (
	pidFileName = "daemon.pid"
	logFileName = "daemon.log"
)

// Supervisor manages background daemon processes for a profile.
type Supervisor struct {
	workDir string
}

// NewSupervisor creates a Supervisor for the given working directory.
func NewSupervisor(dir string) *Supervisor {
	return &Supervisor{workDir: dir}
}

// Start forks the current process into a background daemon and runs the given processes.
func (s *Supervisor) Start(ctx context.Context, procs []process.Process) error {
	if s.Alive() {
		logrus.Info("daemon already running, startup ignored")
		return nil
	}

	gctx, child, err := s.reborn()
	if err != nil {
		return err
	}
	if gctx != nil {
		defer func() { _ = gctx.Release() }()
	}
	if !child {
		return nil
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return s.run(ctx, procs...)
}

// Stop gracefully shuts down the daemon.
func (s *Supervisor) Stop(ctx context.Context) error {
	if !s.Alive() {
		return nil
	}

	pidFile := filepath.Join(s.workDir, pidFileName)
	if err := cli.Exec("/usr/bin/pkill", "-F", pidFile).Interactive().Run(); err != nil {
		return fmt.Errorf("error sending sigterm to daemon: %w", err)
	}

	logrus.Info("waiting for process to terminate")
	for {
		if !s.Alive() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// Alive reports whether the daemon is currently running.
func (s *Supervisor) Alive() bool {
	pidFile := filepath.Join(s.workDir, pidFileName)
	if _, err := os.Stat(pidFile); err != nil {
		return false
	}

	data, err := os.ReadFile(filepath.Clean(pidFile))
	if err != nil {
		return false
	}
	pid, _ := strconv.Atoi(string(data))
	if pid == 0 {
		return false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func (s *Supervisor) reborn() (*godaemon.Context, bool, error) {
	if err := fsutil.EnsureDir(s.workDir); err != nil {
		return nil, false, fmt.Errorf("cannot create daemon directory: %w", err)
	}

	gctx := &godaemon.Context{
		PidFileName: filepath.Join(s.workDir, pidFileName),
		PidFilePerm: 0644,
		LogFileName: filepath.Join(s.workDir, logFileName),
		LogFilePerm: 0644,
	}

	child, err := gctx.Reborn()
	if err != nil {
		return gctx, false, fmt.Errorf("error starting daemon: %w", err)
	}
	if child != nil {
		return gctx, false, nil
	}

	logrus.Info("- - - - - - - - - - - - - - -")
	logrus.Info("daemon started by anvil")
	logrus.Infof("Run `/usr/bin/pkill -F %s` to kill the daemon", gctx.PidFileName)

	return gctx, true, nil
}

func (s *Supervisor) run(ctx context.Context, procs ...process.Process) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(len(procs))

	for _, p := range procs {
		go func(proc process.Process) {
			defer wg.Done()
			if err := proc.Start(ctx); err != nil {
				logrus.Errorf("error starting %s: %v", proc.Name(), err)
				cancel()
			}
		}(p)
	}

	<-ctx.Done()
	logrus.Info("terminate signal received")
	wg.Wait()
	return ctx.Err()
}
