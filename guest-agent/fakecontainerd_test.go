package main

// A fake containerd: an in-process gRPC server implementing the four services
// the Docker API read paths use (namespaces, containers, tasks, images), so
// list/inspect/df handlers run against real containerd-client code without a
// VM. Calls that need other services (content, snapshots) fail and take the
// handlers' existing degradation paths (size 0, empty labels) — same as a
// real daemon hiccup.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	containerspb "github.com/containerd/containerd/api/services/containers/v1"
	eventspb "github.com/containerd/containerd/api/services/events/v1"
	imagespb "github.com/containerd/containerd/api/services/images/v1"
	namespacespb "github.com/containerd/containerd/api/services/namespaces/v1"
	taskspb "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/api/types"
	tasktype "github.com/containerd/containerd/api/types/task"
)

// fakeNS is the per-namespace state served by the fake.
type fakeNS struct {
	containers map[string]*containerspb.Container
	tasks      map[string]*tasktype.Process // container ID -> task
	images     map[string]*imagespb.Image   // image name -> image
}

// One wrapper per service (embedding all Unimplemented servers in one
// struct makes shared method names like Create ambiguous selectors).
type fakeContainerd struct {
	ns map[string]fakeNamespaceData
}

type fakeNamespaceData struct {
	containers map[string]*containerspb.Container
	tasks      map[string]*tasktype.Process // container ID -> task
	images     map[string]*imagespb.Image   // image name -> image
}

type fakeNamespacesService struct {
	namespacespb.UnimplementedNamespacesServer
	f *fakeContainerd
}

type fakeContainersService struct {
	containerspb.UnimplementedContainersServer
	f *fakeContainerd
}

type fakeTasksService struct {
	taskspb.UnimplementedTasksServer
	f *fakeContainerd
}

type fakeImagesService struct {
	imagespb.UnimplementedImagesServer
	f *fakeContainerd
}

// requestNamespace mirrors containerd's namespace propagation: the client
// attaches it as gRPC metadata on every call.
func requestNamespace(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("containerd-namespace"); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func (s *fakeNamespacesService) List(ctx context.Context, _ *namespacespb.ListNamespacesRequest) (*namespacespb.ListNamespacesResponse, error) {
	var out []*namespacespb.Namespace
	for name := range s.f.ns {
		out = append(out, &namespacespb.Namespace{Name: name})
	}
	return &namespacespb.ListNamespacesResponse{Namespaces: out}, nil
}

func (s *fakeContainersService) List(ctx context.Context, _ *containerspb.ListContainersRequest) (*containerspb.ListContainersResponse, error) {
	ns, ok := s.f.ns[requestNamespace(ctx)]
	if !ok {
		return &containerspb.ListContainersResponse{}, nil
	}
	var out []*containerspb.Container
	for _, c := range ns.containers {
		out = append(out, c)
	}
	return &containerspb.ListContainersResponse{Containers: out}, nil
}

func (s *fakeContainersService) Get(ctx context.Context, req *containerspb.GetContainerRequest) (*containerspb.GetContainerResponse, error) {
	ns, ok := s.f.ns[requestNamespace(ctx)]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "namespace not found")
	}
	c, ok := ns.containers[req.ID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.ID)
	}
	return &containerspb.GetContainerResponse{Container: c}, nil
}

// Update applies the field-mask label updates the client sends for rename
// (SetLabels -> mask paths "labels.<key>").
func (s *fakeContainersService) Update(ctx context.Context, req *containerspb.UpdateContainerRequest) (*containerspb.UpdateContainerResponse, error) {
	ns, ok := s.f.ns[requestNamespace(ctx)]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "namespace not found")
	}
	c, ok := ns.containers[req.Container.ID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.Container.ID)
	}
	for _, path := range req.UpdateMask.GetPaths() {
		if key, found := strings.CutPrefix(path, "labels."); found {
			if c.Labels == nil {
				c.Labels = map[string]string{}
			}
			c.Labels[key] = req.Container.Labels[key]
		}
	}
	return &containerspb.UpdateContainerResponse{Container: c}, nil
}

func (s *fakeTasksService) Get(ctx context.Context, req *taskspb.GetRequest) (*taskspb.GetResponse, error) {
	ns, ok := s.f.ns[requestNamespace(ctx)]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "namespace not found")
	}
	p, ok := ns.tasks[req.ContainerID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.ContainerID)
	}
	return &taskspb.GetResponse{Process: p}, nil
}

func (s *fakeImagesService) List(ctx context.Context, _ *imagespb.ListImagesRequest) (*imagespb.ListImagesResponse, error) {
	ns, ok := s.f.ns[requestNamespace(ctx)]
	if !ok {
		return &imagespb.ListImagesResponse{}, nil
	}
	var out []*imagespb.Image
	for _, img := range ns.images {
		out = append(out, img)
	}
	return &imagespb.ListImagesResponse{Images: out}, nil
}

func (s *fakeImagesService) Get(ctx context.Context, req *imagespb.GetImageRequest) (*imagespb.GetImageResponse, error) {
	ns, ok := s.f.ns[requestNamespace(ctx)]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "namespace not found")
	}
	img, ok := ns.images[req.Name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "image %s not found", req.Name)
	}
	return &imagespb.GetImageResponse{Image: img}, nil
}

// fakeContainerdOption customizes one namespace of the fake.
type fakeContainerdOption func(*fakeNamespaceData)

func withContainer(c *containerspb.Container, task *tasktype.Process) fakeContainerdOption {
	return func(ns *fakeNamespaceData) {
		ns.containers[c.ID] = c
		if task != nil {
			// The client identifies the task by Process.ID on later calls.
			task.ContainerID = c.ID
			task.ID = c.ID
			ns.tasks[c.ID] = task
		}
	}
}

func withImage(name, digest string, size int64) fakeContainerdOption {
	return func(ns *fakeNamespaceData) {
		ns.images[name] = &imagespb.Image{
			Name: name,
			Target: &types.Descriptor{
				MediaType: "application/vnd.docker.distribution.manifest.list.v2+json",
				Digest:    digest,
				Size:      size,
			},
		}
	}
}

// ociSpecFor builds the container Spec blob the client unmarshals into an
// OCI spec (Process.Env / Process.Args feed the inspect Config).
func ociSpecFor(args, env []string) *anypb.Any {
	return &anypb.Any{
		TypeUrl: "json",
		Value: []byte(`{"process":{"args":` + jsonStringSlice(args) +
			`,"env":` + jsonStringSlice(env) + `}}`),
	}
}

func jsonStringSlice(ss []string) string {
	out := []byte{'['}
	for i, s := range ss {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '"')
		out = append(out, s...)
		out = append(out, '"')
	}
	return string(append(out, ']'))
}

// startFakeContainerd serves the fake on a unix socket and installs it as the
// package-level pc, restoring the previous client afterwards.
func startFakeContainerd(t *testing.T, namespace string, opts ...fakeContainerdOption) {
	t.Helper()
	nsData := fakeNamespaceData{
		containers: map[string]*containerspb.Container{},
		tasks:      map[string]*tasktype.Process{},
		images:     map[string]*imagespb.Image{},
	}
	for _, o := range opts {
		o(&nsData)
	}
	f := &fakeContainerd{ns: map[string]fakeNamespaceData{namespace: nsData}}

	// macOS sun_path is ~104 bytes; t.TempDir() paths exceed that.
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("fakecd-%d.sock", os.Getpid()))
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	namespacespb.RegisterNamespacesServer(srv, &fakeNamespacesService{f: f})
	containerspb.RegisterContainersServer(srv, &fakeContainersService{f: f})
	taskspb.RegisterTasksServer(srv, &fakeTasksService{f: f})
	eventspb.RegisterEventsServer(srv, &fakeEventsService{})
	imagespb.RegisterImagesServer(srv, &fakeImagesService{f: f})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Stop(); os.Remove(sock) })

	old := pc
	pc = newPersistentClient(sock)
	t.Cleanup(func() {
		pc.close()
		pc = old
	})
}

// fakeEventsService implements the events service: subscriptions live but
// never deliver (the handlers under test replay from the in-memory ring and
// use the live stream only as a keep-open select case).
type fakeEventsService struct {
	eventspb.UnimplementedEventsServer
}

func (s *fakeEventsService) Subscribe(req *eventspb.SubscribeRequest, stream eventspb.Events_SubscribeServer) error {
	<-stream.Context().Done()
	return nil
}

// runningTask is the task record for a live container.
func runningTask(pid uint32) *tasktype.Process {
	return &tasktype.Process{
		Pid:    pid,
		Status: tasktype.Status_RUNNING,
	}
}
