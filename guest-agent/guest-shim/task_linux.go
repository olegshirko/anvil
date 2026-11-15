//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"syscall"

	"github.com/containerd/cgroups/v3/cgroup2"
	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TaskService implementation, Linux side. Create/Start/exec live in
// task.go and exec.go; this file is teardown and supervision: kill,
// delete, pids, pause/resume, update, resize, close-io and stats.

// --- TaskService RPCs -------------------------------------------------------

// Kill signals the init (or an exec) — or the whole cgroup with All. In the
// created state the init process exists (born at create, holding the gate)
// and is the signal target; its supervising helper reports the exit and the
// report EOF resolves the task.
func (s *taskService) Kill(ctx context.Context, r *taskAPI.KillRequest) (*emptypb.Empty, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var (
		pid       int
		cgPath    string
		stopped   bool
		isCreated bool
	)
	if r.ExecID == "" {
		pid, cgPath, stopped, isCreated = c.pid, c.cgPath, c.exited, c.status == task.Status_CREATED
	} else {
		e := c.execs[r.ExecID]
		if e == nil {
			s.mu.Unlock()
			return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
		}
		pid, cgPath, stopped, isCreated = e.pid, c.cgPath, e.exited, e.status == task.Status_CREATED
	}
	s.mu.Unlock()

	if stopped {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "process %s already finished", r.ID)
	}
	sig := int(r.Signal)
	if r.All {
		killCgroup(cgPath)
	}
	target := pid
	if target <= 0 && r.ExecID == "" && isCreated {
		// The created init's pid report never landed (broken protocol):
		// sweep the cgroup — the gate-holding helper and anything it
		// created are its only members — and signal the helper, so the
		// report EOF still resolves the task instead of leaving a created
		// record that nothing can exit.
		killCgroup(cgPath)
		target = helperPid(c)
	}
	if target > 0 {
		if err := unixKill(target, sig); err != nil {
			return nil, errgrpc.ToGRPCf(errdefs.ErrUnknown, "kill %d: %v", target, err)
		}
	}
	return &emptypb.Empty{}, nil
}

// unixKill sends a signal, treating an already-dead target as success.
func unixKill(pid, sig int) error {
	err := unix.Kill(pid, syscall.Signal(sig))
	if err == unix.ESRCH {
		return nil
	}
	return err
}

// helperPid returns the init helper's pid (the exec.Cmd process). In the
// created state it is the process a kill must reach.
func helperPid(c *container) int {
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Delete tears the task down: kill anything left, close the io (loggers see
// EOF and exit), unmount the rootfs, drop the cgroup, publish TaskDelete
// and remove the record. containerd deletes the bundle after the shim's
// Shutdown.
func (s *taskService) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	if r.ExecID != "" {
		s.mu.Lock()
		e := c.execs[r.ExecID]
		if e == nil {
			s.mu.Unlock()
			return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
		}
		running := e.status == task.Status_RUNNING
		pid, code, at := e.pid, e.exitCode, e.exitedAt
		s.mu.Unlock()
		if running {
			killCgroup(c.cgPath) // force path: take the whole cgroup down
			select {
			case <-e.waitCh:
			case <-time.After(3 * time.Second):
			}
		}
		s.mu.Lock()
		delete(c.execs, r.ExecID)
		s.mu.Unlock()
		return &taskAPI.DeleteResponse{
			Pid:        uint32(pid),
			ExitStatus: code,
			ExitedAt:   timestamppb.New(at),
		}, nil
	}

	// Container delete.
	s.mu.Lock()
	running := c.status == task.Status_RUNNING || c.status == task.Status_CREATED
	pid := c.pid
	s.mu.Unlock()
	if running {
		// Kill through the cgroup (instant for every process, frozen ones
		// included) plus the helper directly (created state, pre-cgroup).
		killCgroup(c.cgPath)
		if p := helperPid(c); p > 0 {
			unixKill(p, int(unix.SIGKILL)) //nolint:errcheck
		}
		if pid > 0 {
			unixKill(pid, int(unix.SIGKILL)) //nolint:errcheck
		}
		select {
		case <-c.waitCh:
		case <-time.After(5 * time.Second):
			log.G(ctx).WithField("id", c.id).Warn("init did not exit after kill; deleting anyway")
		}
	}
	// Close the gate first: a created helper blocked on it exits cleanly.
	c.gateW.Close()
	// End the OOM watcher before the cgroup it reads disappears.
	c.stopOOMWatcher()
	// IO teardown: fifo write ends and logger pipes close — the loggers
	// (agent-side fifo logger, binary-v2 json logger) see EOF and exit.
	c.io.close()

	// Snapshot mounts off the rootfs.
	if err := mount.UnmountRecursive(filepath.Join(c.bundle, "rootfs"), 0); err != nil {
		if _, statErr := os.Stat(filepath.Join(c.bundle, "rootfs")); statErr == nil {
			log.G(ctx).WithError(err).Warn("unmount rootfs at delete")
		}
	}
	removeCgroup(c.cgPath)

	s.mu.Lock()
	code, at := c.exitCode, c.exitedAt
	delete(s.containers, r.ID)
	s.mu.Unlock()

	s.send(&eventstypes.TaskDelete{
		ContainerID: c.id,
		Pid:         uint32(pid),
		ExitStatus:  code,
		ExitedAt:    timestamppb.New(at),
	})
	return &taskAPI.DeleteResponse{
		Pid:        uint32(pid),
		ExitStatus: code,
		ExitedAt:   timestamppb.New(at),
	}, nil
}

// Pids lists the processes of the container's pid namespace.
func (s *taskService) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	pid := c.pid
	s.mu.Unlock()
	if pid <= 0 {
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "container %s is not running", r.ID)
	}
	procs, err := pidNamespacePids(pid)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	out := make([]*task.ProcessInfo, 0, len(procs))
	for _, p := range procs {
		out = append(out, &task.ProcessInfo{Pid: p})
	}
	return &taskAPI.PidsResponse{Processes: out}, nil
}

// pidNamespacePids scans /proc for processes sharing the init's pid
// namespace (docker-top parity with the runc shim's ns-based listing).
func pidNamespacePids(initPid int) ([]uint32, error) {
	inode, err := nsInode(fmt.Sprintf("/proc/%d/ns/pid", initPid))
	if err != nil {
		return nil, fmt.Errorf("init %d gone: %w", initPid, err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, entry := range entries {
		name := entry.Name()
		if !isNumeric(name) {
			continue
		}
		ino, err := nsInode(fmt.Sprintf("/proc/%s/ns/pid", name))
		if err != nil || ino != inode {
			continue
		}
		pid, err := strconv.ParseUint(name, 10, 32)
		if err == nil {
			out = append(out, uint32(pid))
		}
	}
	return out, nil
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// nsInode returns the namespace inode reported by readlink on ns/pid.
func nsInode(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	idx := strings.LastIndexByte(target, '[')
	if idx < 0 || !strings.HasSuffix(target, "]") {
		return "", fmt.Errorf("unexpected ns link %s", target)
	}
	return target[idx+1 : len(target)-1], nil
}

// Pause freezes the container (cgroup v2 freezer — the same mechanism
// containerd uses for the runc shim).
func (s *taskService) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*emptypb.Empty, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	st := c.status
	s.mu.Unlock()
	if st != task.Status_RUNNING {
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "container %s is not running", r.ID)
	}
	if err := freezeCgroup(c.cgPath, true); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	s.mu.Lock()
	c.status = task.Status_PAUSED
	s.mu.Unlock()
	s.send(&eventstypes.TaskPaused{ContainerID: r.ID})
	return &emptypb.Empty{}, nil
}

// Resume unfreezes the container.
func (s *taskService) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	st := c.status
	s.mu.Unlock()
	if st != task.Status_PAUSED {
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "container %s is not paused", r.ID)
	}
	if err := freezeCgroup(c.cgPath, false); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	s.mu.Lock()
	c.status = task.Status_RUNNING
	s.mu.Unlock()
	s.send(&eventstypes.TaskResumed{ContainerID: r.ID})
	return &emptypb.Empty{}, nil
}

// Checkpoint is not supported (docker checkpoint does not exist in anvil).
func (s *taskService) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

// ResizePty resizes the init's or an exec's console.
func (s *taskService) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*emptypb.Empty, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var con *stdioSet
	if r.ExecID == "" {
		con = c.io
	} else if e := c.execs[r.ExecID]; e != nil {
		con = e.io
	}
	s.mu.Unlock()
	if con == nil {
		return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
	}
	if err := con.resizePty(r.Width, r.Height); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &emptypb.Empty{}, nil
}

// CloseIO closes the stdin of the init or an exec (docker CLI's end of
// input; the container's read sees EOF once its own fifo writer closes).
func (s *taskService) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var con *stdioSet
	if r.ExecID == "" {
		con = c.io
		if r.Stdin {
			c.stdinOpen = false
		}
	} else if e := c.execs[r.ExecID]; e != nil {
		con = e.io
		if r.Stdin {
			e.stdinOpen = false
		}
	}
	s.mu.Unlock()
	if con == nil {
		return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
	}
	con.closeIOStdin()
	return &emptypb.Empty{}, nil
}

// Update applies new resource limits (docker update / task update).
func (s *taskService) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if r.Resources == nil || r.Resources.GetValue() == nil {
		return &emptypb.Empty{}, nil
	}
	var res specs.LinuxResources
	if err := jsonDecodeAny(r.Resources.Value, &res); err != nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrInvalidArgument, "resources: %v", err)
	}
	if err := updateCgroup(c.cgPath, &res); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &emptypb.Empty{}, nil
}

// Stats reads the container's cgroup into the same metrics type the runc
// shim returns (the agent's docker stats reads cgroup files directly; this
// serves containerd-level consumers like ctr).
func (s *taskService) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if c.cgPath == "" {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "cgroup does not exist")
	}
	rel := strings.TrimPrefix(c.cgPath, cgroupRoot)
	mg, err := cgroup2.Load(rel)
	if err != nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "cgroup does not exist: %v", err)
	}
	st, err := mg.Stat()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	data, err := typeurl.MarshalAny(st)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &taskAPI.StatsResponse{Stats: typeurl.MarshalProto(data)}, nil
}
