package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	control "github.com/moby/buildkit/api/services/control"
	pb "github.com/moby/buildkit/frontend/gateway/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// buildx's "docker" driver connects to the daemon's gRPC endpoint (POST /grpc,
// h2c hijack) and speaks the buildkit control API against what it believes is
// dockerd's embedded buildkit. We proxy that to the guest's own buildkitd —
// except for Solve, where the client-side exporter rewrite ("image" -> "moby",
// done because dockerd registers a private moby exporter) is undone: the
// containerd worker's image exporter with push=false imports straight into the
// shared containerd image store, which is what the moby exporter would do.

// buildkitRawConn connects to buildkitd with the raw-frame codec (see
// containerdClientConn) for transparently forwarded services.
var (
	buildkitRawOnce sync.Once
	buildkitRaw     *grpc.ClientConn
	buildkitRawErr  error
)

func buildkitRawConn() (*grpc.ClientConn, error) {
	buildkitRawOnce.Do(func() {
		if err := ensureBuildkitd(); err != nil {
			buildkitRawErr = err
			return
		}
		cc, err := grpc.NewClient("unix://"+buildkitSocket,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.CallCustomCodec(hybridCodec{})),
		)
		if err != nil {
			buildkitRawErr = err
			return
		}
		buildkitRaw = cc
	})
	return buildkitRaw, buildkitRawErr
}

var containerdProxyConn *grpc.ClientConn

var (
	proxyConnOnce sync.Once
	proxyConn     *grpc.ClientConn
	proxyConnErr  error
)

func controlClientConn() (*grpc.ClientConn, error) {
	proxyConnOnce.Do(func() {
		if err := ensureBuildkitd(); err != nil {
			proxyConnErr = err
			return
		}
		cc, err := grpc.NewClient("unix://"+buildkitSocket,
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			proxyConnErr = err
			return
		}
		proxyConn = cc
	})
	return proxyConn, proxyConnErr
}

type controlProxy struct {
	control.UnimplementedControlServer
	cc *grpc.ClientConn
}

func (p *controlProxy) client() control.ControlClient {
	return control.NewControlClient(p.cc)
}

func outgoing(ctx context.Context) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		return metadata.NewOutgoingContext(ctx, md)
	}
	return ctx
}

func (p *controlProxy) Solve(ctx context.Context, req *control.SolveRequest) (*control.SolveResponse, error) {
	// buildx rewrites the "image" exporter to dockerd's private "moby"
	// exporter when the driver is the docker driver; the standalone
	// buildkitd has no moby exporter, so undo the rewrite and use the
	// image exporter, which imports into the shared containerd store.
	for _, e := range req.Exporters {
		if e.Type == "moby" {
			e.Type = "image"
			if e.Attrs == nil {
				e.Attrs = map[string]string{}
			}
			e.Attrs["push"] = "false"
			e.Attrs["name"] = normalizeImageNames(e.Attrs["name"])
		}
	}
	if req.ExporterDeprecated == "moby" {
		req.ExporterDeprecated = "image"
		if req.ExporterAttrsDeprecated == nil {
			req.ExporterAttrsDeprecated = map[string]string{}
		}
		req.ExporterAttrsDeprecated["push"] = "false"
		req.ExporterAttrsDeprecated["name"] = normalizeImageNames(req.ExporterAttrsDeprecated["name"])
	}
	return p.client().Solve(outgoing(ctx), req)
}

// normalizeImageNames appends the default :latest tag to every comma-separated
// image name that lacks an explicit tag — the moby exporter does this when it
// imports into the daemon store, the plain image exporter does not.
func normalizeImageNames(names string) string {
	if names == "" {
		return names
	}
	parts := strings.Split(names, ",")
	for i, n := range parts {
		if n == "" {
			continue
		}
		prefix := n
		seg := strings.SplitN(n, "/", 2)
		switch {
		case len(seg) == 2 && (strings.ContainsAny(seg[0], ".:") || seg[0] == "localhost"):
			// already has a registry
		case len(seg) == 1:
			prefix = "docker.io/library/" + n
		default:
			prefix = "docker.io/" + n
		}
		last := prefix[strings.LastIndex(prefix, "/")+1:]
		if !strings.ContainsAny(last, ":@") {
			prefix += ":latest"
		}
		parts[i] = prefix
	}
	return strings.Join(parts, ",")
}

func (p *controlProxy) DiskUsage(ctx context.Context, req *control.DiskUsageRequest) (*control.DiskUsageResponse, error) {
	return p.client().DiskUsage(outgoing(ctx), req)
}

func (p *controlProxy) ListWorkers(ctx context.Context, req *control.ListWorkersRequest) (*control.ListWorkersResponse, error) {
	return p.client().ListWorkers(outgoing(ctx), req)
}

func (p *controlProxy) Info(ctx context.Context, req *control.InfoRequest) (*control.InfoResponse, error) {
	return p.client().Info(outgoing(ctx), req)
}

func (p *controlProxy) UpdateBuildHistory(ctx context.Context, req *control.UpdateBuildHistoryRequest) (*control.UpdateBuildHistoryResponse, error) {
	return p.client().UpdateBuildHistory(outgoing(ctx), req)
}

func serverStream[T any, U any](src interface{ Recv() (T, error) }, dst interface {
	Send(U) error
}, conv func(T) U,
) error {
	for {
		v, err := src.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := dst.Send(conv(v)); err != nil {
			return err
		}
	}
}

func (p *controlProxy) Prune(req *control.PruneRequest, stream grpc.ServerStreamingServer[control.UsageRecord]) error {
	cs, err := p.client().Prune(outgoing(stream.Context()), req)
	if err != nil {
		return err
	}
	return serverStream(cs, stream, func(v *control.UsageRecord) *control.UsageRecord { return v })
}

func (p *controlProxy) Status(req *control.StatusRequest, stream grpc.ServerStreamingServer[control.StatusResponse]) error {
	cs, err := p.client().Status(outgoing(stream.Context()), req)
	if err != nil {
		return err
	}
	return serverStream(cs, stream, func(v *control.StatusResponse) *control.StatusResponse { return v })
}

func (p *controlProxy) ListenBuildHistory(req *control.BuildHistoryRequest, stream grpc.ServerStreamingServer[control.BuildHistoryEvent]) error {
	cs, err := p.client().ListenBuildHistory(outgoing(stream.Context()), req)
	if err != nil {
		return err
	}
	return serverStream(cs, stream, func(v *control.BuildHistoryEvent) *control.BuildHistoryEvent { return v })
}

func (p *controlProxy) Session(stream grpc.BidiStreamingServer[control.BytesMessage, control.BytesMessage]) error {
	cs, err := p.client().Session(outgoing(stream.Context()))
	if err != nil {
		return err
	}
	errCh := make(chan error, 2)
	go func() {
		for {
			v, err := stream.Recv()
			if err == io.EOF {
				cs.CloseSend()
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			if err := cs.Send(v); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		for {
			v, err := cs.Recv()
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			if err := stream.Send(v); err != nil {
				errCh <- err
				return
			}
		}
	}()
	return <-errCh
}

// singleConnListener feeds one hijacked connection to grpc.Server.Serve.
type singleConnListener struct {
	once sync.Once
	conn net.Conn
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	// Serve keeps calling Accept; block forever instead of busy-looping.
	select {}
}

func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// prefixConn restores bytes the bufio reader swallowed after the hijack but
// before the HTTP/2 preface.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// serveBuildkitGRPC serves the buildkit control API on a hijacked docker-API
// connection (see handleBuildkitGRPC in buildkit.go).
func serveBuildkitGRPC(conn net.Conn, buffered io.Reader) error {
	cc, err := controlClientConn()
	if err != nil {
		return err
	}
	containerdProxyConn, err = containerdClientConn()
	if err != nil {
		return err
	}
	gs := grpc.NewServer(
		grpc.ForceServerCodec(hybridCodec{}),
		grpc.UnknownServiceHandler(proxyUnknownStream(containerdProxyConn)),
	)
	control.RegisterControlServer(gs, &controlProxy{cc: cc})
	pb.RegisterLLBBridgeServer(gs, &llbBridge{cc: cc})
	return gs.Serve(&singleConnListener{conn: prefixConn{Conn: conn, r: io.MultiReader(buffered, conn)}})
}

// llbBridge forwards the gateway frontend service (the same service buildkitd
// itself exposes; buildx's docker driver expects it on the daemon /grpc conn).
type llbBridge struct {
	pb.UnimplementedLLBBridgeServer
	cc *grpc.ClientConn
}

func (b *llbBridge) client() pb.LLBBridgeClient { return pb.NewLLBBridgeClient(b.cc) }

func (b *llbBridge) ResolveImageConfig(ctx context.Context, req *pb.ResolveImageConfigRequest) (*pb.ResolveImageConfigResponse, error) {
	return b.client().ResolveImageConfig(outgoing(ctx), req)
}

func (b *llbBridge) ResolveSourceMeta(ctx context.Context, req *pb.ResolveSourceMetaRequest) (*pb.ResolveSourceMetaResponse, error) {
	return b.client().ResolveSourceMeta(outgoing(ctx), req)
}

func (b *llbBridge) Solve(ctx context.Context, req *pb.SolveRequest) (*pb.SolveResponse, error) {
	return b.client().Solve(outgoing(ctx), req)
}

func (b *llbBridge) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	return b.client().ReadFile(outgoing(ctx), req)
}

func (b *llbBridge) ReadDir(ctx context.Context, req *pb.ReadDirRequest) (*pb.ReadDirResponse, error) {
	return b.client().ReadDir(outgoing(ctx), req)
}

func (b *llbBridge) StatFile(ctx context.Context, req *pb.StatFileRequest) (*pb.StatFileResponse, error) {
	return b.client().StatFile(outgoing(ctx), req)
}

func (b *llbBridge) Evaluate(ctx context.Context, req *pb.EvaluateRequest) (*pb.EvaluateResponse, error) {
	return b.client().Evaluate(outgoing(ctx), req)
}

func (b *llbBridge) Ping(ctx context.Context, req *pb.PingRequest) (*pb.PongResponse, error) {
	return b.client().Ping(outgoing(ctx), req)
}

func (b *llbBridge) Return(ctx context.Context, req *pb.ReturnRequest) (*pb.ReturnResponse, error) {
	return b.client().Return(outgoing(ctx), req)
}

func (b *llbBridge) Inputs(ctx context.Context, req *pb.InputsRequest) (*pb.InputsResponse, error) {
	return b.client().Inputs(outgoing(ctx), req)
}

func (b *llbBridge) NewContainer(ctx context.Context, req *pb.NewContainerRequest) (*pb.NewContainerResponse, error) {
	return b.client().NewContainer(outgoing(ctx), req)
}

func (b *llbBridge) ReleaseContainer(ctx context.Context, req *pb.ReleaseContainerRequest) (*pb.ReleaseContainerResponse, error) {
	return b.client().ReleaseContainer(outgoing(ctx), req)
}

func (b *llbBridge) Warn(ctx context.Context, req *pb.WarnRequest) (*pb.WarnResponse, error) {
	return b.client().Warn(outgoing(ctx), req)
}

func (b *llbBridge) ExecProcess(stream grpc.BidiStreamingServer[pb.ExecMessage, pb.ExecMessage]) error {
	cs, err := b.client().ExecProcess(outgoing(stream.Context()))
	if err != nil {
		return err
	}
	errCh := make(chan error, 2)
	go func() {
		for {
			v, err := stream.Recv()
			if err == io.EOF {
				cs.CloseSend()
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			if err := cs.Send(v); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		for {
			v, err := cs.Recv()
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			if err := stream.Send(v); err != nil {
				errCh <- err
				return
			}
		}
	}()
	return <-errCh
}

// bridgeSession tunnels a hijacked /session connection (the client's buildkit
// session, registered with X-Docker-Expose-Session-* headers) into buildkitd
// through a control.Session stream — the same wire form the remote driver
// uses via grpchijack, but bridged by us because the docker driver connects
// to the daemon's /session endpoint instead.
func bridgeSession(conn net.Conn, header http.Header) {
	cc, err := controlClientConn()
	if err != nil {
		conn.Close()
		return
	}
	md := metadata.MD{}
	for k, v := range header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-docker-expose-session-") {
			md[lk] = v
		}
	}
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	stream, err := control.NewControlClient(cc).Session(ctx)
	if err != nil {
		conn.Close()
		return
	}
	go func() {
		defer conn.Close()
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if serr := stream.Send(&control.BytesMessage{Data: buf[:n]}); serr != nil {
					return
				}
			}
			if err != nil {
				stream.CloseSend()
				return
			}
		}
	}()
	go func() {
		defer conn.Close()
		for {
			m, err := stream.Recv()
			if err != nil {
				return
			}
			if _, err := conn.Write(m.Data); err != nil {
				return
			}
		}
	}()
}

// --- transparent proxy for containerd services -------------------------------

// rawFrame carries gRPC messages verbatim, so unknown services can be
// forwarded without their protobuf definitions.
type rawFrame []byte

func (f *rawFrame) Reset()         {}
func (f *rawFrame) String() string { return "rawFrame" }
func (f *rawFrame) ProtoMessage()  {}

// hybridCodec passes rawFrame through untouched and delegates everything
// else to the proto codec, letting registered (proto-typed) services and the
// transparent proxy share one gRPC server.
type hybridCodec struct{}

func (hybridCodec) Name() string   { return "proto" }
func (hybridCodec) String() string { return "proto" }

func (hybridCodec) Marshal(v any) ([]byte, error) {
	if f, ok := v.(*rawFrame); ok {
		return *f, nil
	}
	return proto.Marshal(v.(proto.Message))
}

func (hybridCodec) Unmarshal(data []byte, v any) error {
	if f, ok := v.(*rawFrame); ok {
		*f = append((*f)[:0], data...)
		return nil
	}
	return proto.Unmarshal(data, v.(proto.Message))
}

var (
	containerdConnOnce sync.Once
	containerdConn     *grpc.ClientConn
	containerdConnErr  error
)

func containerdClientConn() (*grpc.ClientConn, error) {
	containerdConnOnce.Do(func() {
		cc, err := grpc.NewClient("unix:///run/containerd/containerd.sock",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.CallCustomCodec(hybridCodec{})),
		)
		if err != nil {
			containerdConnErr = err
			return
		}
		containerdConn = cc
	})
	return containerdConn, containerdConnErr
}

// proxyUnknownStream forwards a gRPC stream for an unregistered service to
// the guest's containerd daemon. buildx's docker driver expects dockerd's
// containerd services (e.g. content.v1 for provenance records) on the same
// /grpc endpoint as the buildkit control API.
func proxyUnknownStream(cc *grpc.ClientConn) grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		method, ok := grpc.MethodFromServerStream(ss)
		if !ok {
			return status.Error(codes.Internal, "unknown method")
		}
		debugLog("grpc-bridge: forwarding %s", method)
		ctx := outgoing(ss.Context())
		target := cc
		// buildx's docker driver reads build-record provenance through the
		// content service, which buildkitd itself fronts on its gRPC server
		// (the same service remote-driver clients use); everything else goes
		// to the containerd daemon.
		if strings.HasPrefix(method, "/containerd.services.content.v1.") {
			if bk, berr := buildkitRawConn(); berr == nil {
				target = bk
			}
		} else if md, ok := metadata.FromOutgoingContext(ctx); !ok || len(md.Get("containerd-namespace")) == 0 {
			// dockerd fronts containerd with its own namespace; buildx
			// relies on that and sends none, so inject it when missing.
			ctx = metadata.AppendToOutgoingContext(ctx, "containerd-namespace", "default")
		}
		cs, err := target.NewStream(ctx, &grpc.StreamDesc{
			StreamName:    method,
			ClientStreams: true,
			ServerStreams: true,
		}, method)
		if err != nil {
			return err
		}
		errCh := make(chan error, 2)
		go func() {
			for {
				var f rawFrame
				if err := ss.RecvMsg(&f); err != nil {
					// Client half-closed; the RPC only finishes once the
					// upstream response arrives — do NOT signal completion
					// here or the handler returns before forwarding it.
					cs.CloseSend()
					return
				}
				if err := cs.SendMsg(&f); err != nil {
					debugLog("grpc-bridge: %s upstream send: %v", method, err)
					errCh <- err
					return
				}
			}
		}()
		go func() {
			for {
				var f rawFrame
				if err := cs.RecvMsg(&f); err != nil {
					debugLog("grpc-bridge: %s upstream recv done: %v", method, err)
					if err == io.EOF {
						errCh <- nil
					} else {
						errCh <- err // forward the upstream status to the client
					}
					return
				}
				if err := ss.SendMsg(&f); err != nil {
					debugLog("grpc-bridge: %s downstream send: %v", method, err)
					errCh <- err
					return
				}
			}
		}()
		rerr := <-errCh
		debugLog("grpc-bridge: %s done: %v", method, rerr)
		return rerr
	}
}
