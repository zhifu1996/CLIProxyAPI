package executor

import (
	"context"
	stdtls "crypto/tls"
	"fmt"
	"net"
	"sync"
	"unsafe"

	utls "github.com/refraction-networking/utls"
	pb "github.com/router-for-me/CLIProxyAPI/v6/internal/proto/v1internal"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// utlsTransportCredentials implements credentials.TransportCredentials using uTLS
// to make the gRPC TLS ClientHello look like Chrome/BoringSSL.
type utlsTransportCredentials struct {
	serverName string
}

func newUtlsTransportCredentials() credentials.TransportCredentials {
	return &utlsTransportCredentials{}
}

func (c *utlsTransportCredentials) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = authority
	}
	serverName := c.serverName
	if serverName == "" {
		serverName = host
	}

	cfg := &utls.Config{ServerName: serverName}
	tlsConn := utls.UClient(rawConn, cfg, utls.HelloChrome_Auto)

	errCh := make(chan error, 1)
	go func() { errCh <- tlsConn.Handshake() }()

	select {
	case err := <-errCh:
		if err != nil {
			tlsConn.Close()
			return nil, nil, fmt.Errorf("utls handshake: %w", err)
		}
	case <-ctx.Done():
		tlsConn.Close()
		return nil, nil, ctx.Err()
	}

	// utls.ConnectionState and crypto/tls.ConnectionState are layout-identical;
	// convert via unsafe to satisfy the credentials.TLSInfo type.
	uState := tlsConn.ConnectionState()
	stdState := *(*stdtls.ConnectionState)(unsafe.Pointer(&uState))
	return tlsConn, credentials.TLSInfo{State: stdState}, nil
}

func (c *utlsTransportCredentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, fmt.Errorf("utlsTransportCredentials: server handshake not supported")
}

func (c *utlsTransportCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}

func (c *utlsTransportCredentials) Clone() credentials.TransportCredentials {
	return &utlsTransportCredentials{serverName: c.serverName}
}

func (c *utlsTransportCredentials) OverrideServerName(name string) error {
	c.serverName = name
	return nil
}

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
		grpc.WithTransportCredentials(newUtlsTransportCredentials()),
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
