package desktop

import (
	"context"
	"net"
	"sync"
)

// recordingDialer remembers the connections it opened so a client can drop
// its WebSocket without any protocol-level close (which the inspector would
// treat as a detach anyway, and which rod's client does not expose).
type recordingDialer struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (d *recordingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conns = append(d.conns, c)
	d.mu.Unlock()
	return c, nil
}

func (d *recordingDialer) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		_ = c.Close()
	}
	d.conns = nil
}
