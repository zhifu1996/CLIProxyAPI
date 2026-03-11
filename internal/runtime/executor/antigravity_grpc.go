package executor

import (
	"context"
	"crypto/tls"
	"sync"

	pb "github.com/router-for-me/CLIProxyAPI/v6/internal/proto/v1internal"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// antigravityGRPCPool manages reusable gRPC connections keyed by target host.
type antigravityGRPCPool struct {
	mu    sync.RWMutex
	conns map[string]*grpc.ClientConn
}

var grpcPool = &antigravityGRPCPool{
	conns: make(map[string]*grpc.ClientConn),
}

// getOrCreate returns an existing or newly created CloudCodeClient for the given target.
func (p *antigravityGRPCPool) getOrCreate(target, token, userAgent string) (pb.CloudCodeClient, *grpc.ClientConn, error) {
	p.mu.RLock()
	conn, ok := p.conns[target]
	p.mu.RUnlock()
	if ok {
		return pb.NewCloudCodeClient(conn), conn, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock.
	if conn, ok = p.conns[target]; ok {
		return pb.NewCloudCodeClient(conn), conn, nil
	}

	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
		grpc.WithUserAgent(userAgent),
	)
	if err != nil {
		return nil, nil, err
	}

	p.conns[target] = conn
	log.Debugf("antigravity grpc: new connection to %s", target)
	return pb.NewCloudCodeClient(conn), conn, nil
}

// antigravityTokenCreds implements grpc.PerRPCCredentials to inject Bearer tokens.
type antigravityTokenCreds struct {
	token string
}

func (c *antigravityTokenCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + c.token,
	}, nil
}

func (c *antigravityTokenCreds) RequireTransportSecurity() bool {
	return true
}

// grpcOutgoingMetadata builds the gRPC metadata (headers) to attach to outgoing calls.
func grpcOutgoingMetadata(ctx context.Context, token, userAgent string) context.Context {
	md := metadata.Pairs(
		"authorization", "Bearer "+token,
		"user-agent", userAgent,
	)
	return metadata.NewOutgoingContext(ctx, md)
}
