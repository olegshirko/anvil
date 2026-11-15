//go:build linux

package main

import (
	"context"
	"os"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TaskService implementation, Linux side. Create/Start and the rest of the
// task lifecycle are filled in by the fast-path tasks; the skeleton answers
// State/Connect/Shutdown (service.go) and reports the rest as not implemented
// so containerd's surface probe fails loudly instead of silently.

// cleanupDeadShim is the "delete" action of the shim binary: best-effort
// cleanup after the shim server died without a proper Delete. cwd is the
// bundle; unmount the rootfs so the snapshot mounts do not leak.
func cleanupDeadShim(ctx context.Context, id string) (shim.StopStatus, error) {
	if err := mount.UnmountRecursive("rootfs", 0); err != nil {
		cleanupErr := err
		if _, statErr := os.Stat("rootfs"); statErr != nil {
			cleanupErr = nil // no rootfs in this bundle, nothing to do
		}
		if cleanupErr != nil {
			return shim.StopStatus{}, err
		}
	}
	return shim.StopStatus{}, nil
}

// --- TaskService RPCs -------------------------------------------------------

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
