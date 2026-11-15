//go:build !linux

package main

import (
	"context"
	"os"
	"syscall"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Non-Linux stand-ins: the shim only ever runs in the Linux guest, but the
// package must build on the development Mac (go vet / go test ./...).

// stdioSet is the io plumbing type; off Linux it is an empty shell.
type stdioSet struct{}

// The helper entries are Linux-only; off Linux they never trigger.
func isInitHelper() bool { return false }
func isInitApply() bool  { return false }
func isExecHelper() bool { return false }
func isExecApply() bool  { return false }
func runInit(id, bundle string) int {
	_, _ = id, bundle
	return 1
}

func runInitApply(id, bundle string) int {
	_, _ = id, bundle
	return 1
}

func runExecHelper(initpid, bundle string) int {
	_, _ = initpid, bundle
	return 1
}

func runExecApply() int { return 1 }

// pidAlive reports whether the process is alive (Signal 0 probe).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// cleanupDeadShim is the "delete" action of the shim binary; nothing to
// clean up off Linux.
func cleanupDeadShim(ctx context.Context, id string) (shim.StopStatus, error) {
	return shim.StopStatus{}, nil
}

func (s *taskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Kill(ctx context.Context, r *taskAPI.KillRequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

func (s *taskService) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}
