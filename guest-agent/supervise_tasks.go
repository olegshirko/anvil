package main

import (
	"context"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Stage 5a task registry: the agent supervises eligible containers itself
// (guest-agent/direct.go) and this is its source of truth. containerd keeps
// the container record, snapshots and images; it never sees a task for these
// containers, so /wait, attach teardown, inspect, docker ps, port scanning,
// restart policy and exec read from here. Non-containmenterd containers
// (runc-shim, fast-shim fallback) keep the containerd task paths unchanged.
//
// (The design document calls this file supervise.go; that name is the
// existing containerd supervisor, so the registry lives here.)
//
// Crash safety (anti-goal: a container that runs but whose exit the agent
// misses): the registry is in memory and starts empty. Every recorded task
// is a child of this agent and its exit is joined by cmd.Wait in the start
// goroutine, so a missing record can only mean a cold boot — then the
// persisted container metadata (StartedAt/ExitCode) reports the container
// exited with its recorded code, and bootfinalize has already reaped stray
// helpers.

// directTaskState mirrors the containerd task states the agent's consumers
// understand ("created", "running", "paused", "stopped").
type directTaskState string

const (
	directStateCreated directTaskState = "created"
	directStateRunning directTaskState = "running"
	directStatePaused  directTaskState = "paused"
	directStateStopped directTaskState = "stopped"
)

// directTask is one agent-supervised container run: the helper process, the
// init process it created, the io/log plumbing and the exit state.
type directTask struct {
	ns, id, did string
	// run is the run generation handed to beginTaskRun; teardown applies
	// only while it is the current run (same guard as watchTaskExit).
	run uint64

	cmd       *exec.Cmd
	helperPID int
	bundle    string
	rootfs    string
	// cgroupPath is the container's cgroup v2 directory (from the spec);
	// empty when the spec places no cgroup.
	cgroupPath string

	// release unmounts the rootfs and closes direct io; idempotent.
	release func()

	mu        sync.Mutex
	pid       int
	state     directTaskState
	exitCode  int
	waitErr   error
	waitCh    chan struct{}
	exitedAt  time.Time
	exited    bool
	startedCh chan struct{}
}

func taskKey(ns, id string) string { return ns + "/" + id }

// directSupervisor is the registry of agent-supervised tasks.
type directSupervisor struct {
	mu    sync.Mutex
	tasks map[string]*directTask
	// exitSubs are notified after a task's exit is recorded (restart policy).
	exitSubs []func(*directTask)
}

var supervisor = &directSupervisor{tasks: map[string]*directTask{}}

// put registers t, replacing any previous run's record for the container.
func (s *directSupervisor) put(t *directTask) {
	s.mu.Lock()
	s.tasks[taskKey(t.ns, t.id)] = t
	s.mu.Unlock()
}

// lookup returns the container's direct task record (running, paused or the
// last run's exited record).
func (s *directSupervisor) lookup(ns, id string) (*directTask, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	t, ok := s.tasks[taskKey(ns, id)]
	s.mu.Unlock()
	return t, ok
}

// remove drops the container's record and returns it.
func (s *directSupervisor) remove(ns, id string) *directTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[taskKey(ns, id)]
	delete(s.tasks, taskKey(ns, id))
	return t
}

// runningTasks maps containerd container ID -> init pid for every running or
// paused direct task in one namespace — the same shape scanner's
// namespaceRunningTasks returns, so port pushing merges the two without
// further changes.
func (s *directSupervisor) runningTasks(ns string) map[string]uint32 {
	out := map[string]uint32{}
	s.mu.Lock()
	tasks := make([]*directTask, 0, len(s.tasks))
	for _, t := range s.tasks {
		if t.ns == ns {
			tasks = append(tasks, t)
		}
	}
	s.mu.Unlock()
	for _, t := range tasks {
		if pid, ok := t.pidFor(); ok {
			out[t.id] = uint32(pid)
		}
	}
	return out
}

// namespaceStates is runningTasks' counterpart for inspect/docker ps: every
// direct task of the namespace with its containerd-style state.
func (s *directSupervisor) namespaceStates(ns string) map[string]taskSnapshot {
	out := map[string]taskSnapshot{}
	s.mu.Lock()
	tasks := make([]*directTask, 0, len(s.tasks))
	for _, t := range s.tasks {
		if t.ns == ns {
			tasks = append(tasks, t)
		}
	}
	s.mu.Unlock()
	for _, t := range tasks {
		state, code, _ := t.stateInfo()
		out[t.id] = taskSnapshot{status: string(state), exitCode: code}
	}
	return out
}

// onExit registers fn to run after any direct task exit is recorded.
func (s *directSupervisor) onExit(fn func(*directTask)) {
	s.mu.Lock()
	s.exitSubs = append(s.exitSubs, fn)
	s.mu.Unlock()
}

func (s *directSupervisor) notifyExit(t *directTask) {
	s.mu.Lock()
	subs := append([]func(*directTask){}, s.exitSubs...)
	s.mu.Unlock()
	for _, fn := range subs {
		fn(t)
	}
}

// stateInfo reports the task's containerd-style state, exit code and whether
// it is running or paused.
func (t *directTask) stateInfo() (directTaskState, int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	running := t.state == directStateRunning || t.state == directStatePaused
	return t.state, t.exitCode, running
}

// inspect returns the fields docker inspect/ps need, in containerd task
// terms: status, exit code, running and the init pid.
func (t *directTask) inspect() (status string, exitCode int, running bool, pid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.state), t.exitCode, t.state == directStateRunning || t.state == directStatePaused, t.pid
}

// pidFor reports the task's init pid when it is live.
func (t *directTask) pidFor() (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if (t.state == directStateRunning || t.state == directStatePaused) && t.pid > 0 {
		return t.pid, true
	}
	return 0, false
}

// wait blocks until the run exits and returns its exit code. A start failure
// that was recorded before the run got going returns that error.
func (t *directTask) wait(ctx context.Context) (int, error) {
	for {
		t.mu.Lock()
		if t.waitErr != nil {
			err := t.waitErr
			t.mu.Unlock()
			return 0, err
		}
		ch := t.waitCh
		select {
		case <-ch:
			code := t.exitCode
			t.mu.Unlock()
			return code, nil
		default:
		}
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// markStarted records the init pid and flips the run to running. It is
// called once the helper reported the user process started.
func (t *directTask) markStarted(pid int) {
	t.mu.Lock()
	if t.pid == 0 {
		t.pid = pid
	}
	if !t.exited {
		t.state = directStateRunning
	}
	if t.startedCh == nil {
		t.startedCh = make(chan struct{})
	}
	close(t.startedCh)
	t.startedCh = nil
	t.mu.Unlock()
}

// signal delivers sig to the container init (PID 1 of its namespaces).
func (t *directTask) signal(sig syscall.Signal) error {
	t.mu.Lock()
	pid := t.pid
	helper := t.helperPID
	exited := t.exited
	t.mu.Unlock()
	if exited {
		return nil
	}
	if pid > 0 {
		if err := syscall.Kill(pid, sig); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	// Created but the init pid never reported: reach the helper, so its exit
	// resolves the run instead of leaving a record nothing can exit.
	if helper > 0 {
		if err := syscall.Kill(helper, sig); err != nil && err != syscall.ESRCH {
			return err
		}
	}
	return nil
}

// finished records the exit once and closes waitCh. Callers publish events
// and do teardown before this, so a waiter (and auto-remove) never runs
// ahead of the exit state.
func (t *directTask) finished(code int) {
	t.mu.Lock()
	if t.exited {
		t.mu.Unlock()
		return
	}
	t.exited = true
	t.exitCode = code
	t.exitedAt = time.Now()
	t.state = directStateStopped
	close(t.waitCh)
	t.mu.Unlock()
	supervisor.notifyExit(t)
}

// failStart records a start failure for a waiter already attached to the
// record (it retries against the containerd fallback after this).
func (t *directTask) failStart(err error) {
	t.mu.Lock()
	if t.exited {
		t.mu.Unlock()
		return
	}
	t.waitErr = err
	t.exited = true
	t.state = directStateStopped
	close(t.waitCh)
	t.mu.Unlock()
}

// waitExitWithTimeout polls the registry until the task exits or timeout.
func waitDirectExit(t *directTask, timeout time.Duration) (int, bool) {
	select {
	case <-t.waitCh:
		t.mu.Lock()
		code := t.exitCode
		t.mu.Unlock()
		return code, true
	case <-time.After(timeout):
		return 0, false
	}
}

// directStateOf returns the registry record's state for the consumers that
// used to ask containerd (containerTaskState, currentTaskStatus, ...).
func directStateOf(ns, id string) (state directTaskState, exitCode int, running bool, ok bool) {
	t, found := supervisor.lookup(ns, id)
	if !found {
		return "", 0, false, false
	}
	state, code, running := t.stateInfo()
	return state, code, running, true
}
