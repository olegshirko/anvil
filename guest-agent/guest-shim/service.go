//go:build shimserver

package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"sync"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// newTaskService constructs the TaskService singleton for this shim server
// process. Called once by the plugin graph after RunShim has parsed the
// flags: the context already carries the containerd namespace and the
// process cwd is the container bundle.
func newTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	s := &taskService{
		publisher:  publisher,
		sd:         sd,
		ns:         ns,
		bundlePath: cwd,
		containers: map[string]*container{},
		events:     make(chan any, 128),
	}
	log.G(ctx).WithFields(log.Fields{"namespace": ns, "bundle": cwd}).Debug("anvil-fast-shim service up")
	go s.forward(ctx, publisher)
	return s, nil
}

// taskService implements the containerd runtime-v2 TaskService for one shim
// server process (one container). The struct and the portable RPC plumbing
// live here; namespace/clone/mount work is in the linux-tagged files.
type taskService struct {
	publisher  shim.Publisher
	sd         shutdown.Service
	ns         string
	bundlePath string

	mu         sync.Mutex
	containers map[string]*container
	events     chan any
	closed     bool
}

// container is one task the shim supervises: its init helper process, io
// plumbing and lifecycle state.
type container struct {
	id     string
	bundle string

	// spec is the OCI spec from <bundle>/config.json.
	spec *specs.Spec

	// request io configuration (mirrored in State responses).
	term      bool
	stdin     string
	stdout    string
	stderr    string
	stdinOpen bool

	// init process state. pid is the container init's global pid, known
	// from the create-time "P" report (the init is born holding the gate
	// and the create response answers with it, like the runc shim does).
	pid      int
	status   task.Status
	exitCode uint32
	exitedAt time.Time
	exited   bool

	// started is set once TaskStart has been published, exitPending marks an
	// exit recorded before that happened. A fast container (alpine true)
	// can exit before its Start RPC returns; publishing TaskExit first makes
	// containerd drop the exit for a task it never saw start, leaving the
	// container "running" forever. The exit event is therefore held until
	// TaskStart is out.
	started     bool
	exitPending bool

	// waitCh is closed once the init exit is recorded; late waiters read
	// exitCode directly (the code is cached, mirroring the agent's
	// watchTaskExit semantics).
	waitCh chan struct{}

	// cmd is the init helper process (stage 1); gateW releases it, the
	// report pipe carries its progress ("R"/"S"/"X"/"D" lines).
	cmd       *exec.Cmd
	gateW     *os.File
	reportR   *os.File
	reportBuf *bufio.Reader
	startedCh chan startMsg

	// io is the parent-side stdio plumbing (logger process, fifo ends,
	// console master).
	io *stdioSet

	// cgPath is the container's cgroup v2 path (absolute; empty when the
	// spec places no cgroup).
	cgPath string

	// oomStop ends the cgroup OOM watcher (closed at delete).
	oomStop chan struct{}

	execs map[string]*execProcess
}

// startMsg is the start outcome delivered over the report pipe.
type startMsg struct {
	pid int
	err error
}

// execProcess is one `docker exec` process inside a container.
type execProcess struct {
	id        string
	pid       int
	status    task.Status
	exitCode  uint32
	exitedAt  time.Time
	exited    bool
	waitCh    chan struct{}
	cmd       *exec.Cmd
	gateW     *os.File
	reportR   *os.File
	reportBuf *bufio.Reader
	startedCh chan startMsg
	io        *stdioSet
	stdinOpen bool
	term      bool
}

func (s *taskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

// forward publishes queued events to containerd over the ttrpc publisher.
func (s *taskService) forward(ctx context.Context, publisher shim.Publisher) {
	defer publisher.Close()
	for e := range s.events {
		if err := publisher.Publish(ctx, runtime.GetTopic(e), e); err != nil {
			log.G(ctx).WithError(err).Error("publish shim event")
		}
	}
}

// send queues one event for the publisher; never blocks the RPC path.
func (s *taskService) send(e any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.events <- e:
	default:
		log.L.Warn("shim event queue full, dropping event")
	}
}

// getContainer returns the container record or NotFound (ttrpc-ready error).
func (s *taskService) getContainer(id string) (*container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[id]
	if !ok {
		return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
	}
	return c, nil
}

// --- portable RPCs ----------------------------------------------------------

// State reports the runtime state of the init process or an exec process.
func (s *taskService) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var e *execProcess
	if r.ExecID != "" {
		e = c.execs[r.ExecID]
		if e == nil {
			return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
		}
	}
	return stateOf(r.ID, c, e), nil
}

// Wait blocks until the init (or exec) process exits. The exit code is
// cached: waiters arriving after the exit read it directly, and waiters
// blocked on the channel re-read it once the exit lands (a code captured
// before the exit would be a stale zero).
func (s *taskService) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var waitCh chan struct{}
	var code uint32
	if r.ExecID == "" {
		waitCh, code = c.waitCh, c.exitCode
	} else {
		e, ok := c.execs[r.ExecID]
		if !ok {
			s.mu.Unlock()
			return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
		}
		waitCh, code = e.waitCh, e.exitCode
	}
	s.mu.Unlock()

	select {
	case <-waitCh:
	case <-ctx.Done():
		return nil, errgrpc.ToGRPC(context.Canceled)
	}
	// The channel closed while we waited: pick up the final code.
	s.mu.Lock()
	if r.ExecID == "" {
		code = c.exitCode
	} else if e := c.execs[r.ExecID]; e != nil {
		code = e.exitCode
	}
	s.mu.Unlock()
	return &taskAPI.WaitResponse{ExitStatus: code}, nil
}

// Connect returns the shim's and the init's pids.
func (s *taskService) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: uint32(c.pid),
	}, nil
}

// Shutdown tears the shim server down. containerd calls it after the task
// record is gone; the socket cleanup happens in RunShim's exit path (the
// shim deletes its socket at exit, which the agent's fallback wait relies
// on).
func (s *taskService) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return &emptypb.Empty{}, nil
	}
	s.closed = true
	close(s.events)
	s.mu.Unlock()
	s.sd.Shutdown() //nolint:errcheck — RunShim turns it into process exit
	return &emptypb.Empty{}, nil
}

// stateOf builds a StateResponse for the init or an exec process.
func stateOf(id string, c *container, e *execProcess) *taskAPI.StateResponse {
	if e != nil {
		return &taskAPI.StateResponse{
			ID:         id,
			Bundle:     c.bundle,
			Pid:        uint32(e.pid),
			Status:     e.status,
			Stdin:      c.stdin,
			Stdout:     c.stdout,
			Stderr:     c.stderr,
			Terminal:   e.term,
			ExitStatus: e.exitCode,
			ExitedAt:   timestamppb.New(e.exitedAt),
			ExecID:     e.id,
		}
	}
	return &taskAPI.StateResponse{
		ID:         id,
		Bundle:     c.bundle,
		Pid:        uint32(c.pid),
		Status:     c.status,
		Stdin:      c.stdin,
		Stdout:     c.stdout,
		Stderr:     c.stderr,
		Terminal:   c.term,
		ExitStatus: c.exitCode,
		ExitedAt:   timestamppb.New(c.exitedAt),
	}
}

// publishTaskExit queues the TaskExit event containerd (and the agent's
// docker events stream) expect for every process death.
func publishTaskExit(s *taskService, id, execID string, pid int, code uint32, at time.Time) {
	s.send(&eventstypes.TaskExit{
		ContainerID: id,
		ID:          execID,
		Pid:         uint32(pid),
		ExitStatus:  code,
		ExitedAt:    timestamppb.New(at),
	})
}
