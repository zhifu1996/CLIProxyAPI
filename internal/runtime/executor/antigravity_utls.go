package executor

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// antigravityUtlsRT is a singleton uTLS-backed HTTP/2 round tripper shared
// by all Antigravity REST requests. It makes the TLS ClientHello look like
// Chrome/BoringSSL while supporting HTTP/2 (required by Google API endpoints).
var (
	antigravityUtlsRT     *antigravityUtlsRoundTripper
	antigravityUtlsRTOnce sync.Once
)

// antigravityUtlsRoundTripper uses uTLS with Chrome fingerprint over HTTP/2.
// It manages a pool of http2.ClientConn per host, similar to the Claude uTLS transport.
type antigravityUtlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]*sync.Cond
}

func newAntigravityUtlsRoundTripper() *antigravityUtlsRoundTripper {
	return &antigravityUtlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]*sync.Cond),
	}
}

func (rt *antigravityUtlsRoundTripper) getOrCreateConn(hostname, addr string) (*http2.ClientConn, error) {
	rt.mu.Lock()

	if h2c, ok := rt.connections[hostname]; ok && h2c.CanTakeNewRequest() {
		rt.mu.Unlock()
		return h2c, nil
	}

	if cond, ok := rt.pending[hostname]; ok {
		cond.Wait()
		if h2c, ok := rt.connections[hostname]; ok && h2c.CanTakeNewRequest() {
			rt.mu.Unlock()
			return h2c, nil
		}
	}

	cond := sync.NewCond(&rt.mu)
	rt.pending[hostname] = cond
	rt.mu.Unlock()

	h2c, err := rt.dialH2(hostname, addr)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.pending, hostname)
	cond.Broadcast()

	if err != nil {
		return nil, err
	}
	rt.connections[hostname] = h2c
	return h2c, nil
}

func (rt *antigravityUtlsRoundTripper) dialH2(hostname, addr string) (*http2.ClientConn, error) {
	rawConn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return nil, err
	}
	cfg := &utls.Config{ServerName: hostname}
	tlsConn := utls.UClient(rawConn, cfg, utls.HelloChrome_Auto)
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("utls handshake %s: %w", addr, err)
	}
	tr := &http2.Transport{}
	h2c, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	return h2c, nil
}

func (rt *antigravityUtlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	addr := host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	hostname := req.URL.Hostname()

	h2c, err := rt.getOrCreateConn(hostname, addr)
	if err != nil {
		return nil, err
	}
	resp, err := h2c.RoundTrip(req)
	if err != nil {
		rt.mu.Lock()
		if cached, ok := rt.connections[hostname]; ok && cached == h2c {
			delete(rt.connections, hostname)
		}
		rt.mu.Unlock()
		return nil, err
	}
	return resp, nil
}

// initAntigravityTransport creates the shared uTLS HTTP/2 round tripper exactly once.
func initAntigravityTransport() {
	antigravityUtlsRT = newAntigravityUtlsRoundTripper()
}
