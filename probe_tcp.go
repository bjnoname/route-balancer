package main

import (
	"context"
	"fmt"
)

// TCPProbe tests that a TCP connection can be established to a host:port via
// the target interface. The socket is bound via SO_BINDTODEVICE so the
// connection is forced through the gateway under test.
type TCPProbe struct {
	Host string
	Port uint16
}

func (p *TCPProbe) String() string {
	return fmt.Sprintf("tcp(%s:%d)", p.Host, p.Port)
}

func (p *TCPProbe) Check(ctx context.Context, target Target) error {
	addr := fmt.Sprintf("%s:%d", p.Host, p.Port)
	conn, err := bindToDeviceDialer(target.IfName).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	_ = conn.Close()
	return nil
}
