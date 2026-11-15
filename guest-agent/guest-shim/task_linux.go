//go:build linux

package main

import (
	"context"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TaskService implementation, Linux side. Create/Start live in task.go;
// the teardown and supervision RPCs below are filled in by the fast-path
// tasks. Everything not implemented yet fails loudly so containerd's
// surface probe shows exactly what is missing.

// startExec is filled in by the exec task; the skeleton reports it as not
// implemented.
func (s *taskService) startExec(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}

// runExecHelper is the exec helper's entry (second argument "exec-init");
// filled in by the exec task.
func runExecHelper(initpid, bundle string) int {
	return 1
}

// --- TaskService RPCs -------------------------------------------------------

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
