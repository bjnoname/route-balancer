package probe

import (
	"context"
	"fmt"
	"net"
	"strconv"
)

type tcpProbe struct {
	Host string
	Port uint16
}

func (p *tcpProbe) addr() string {
	return net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
}

func (p *tcpProbe) String() string {
	return "tcp(" + p.addr() + ")"
}

func (p *tcpProbe) Check(ctx context.Context, target Target) error {
	conn, err := BindToDeviceDialer(target.IfName).DialContext(ctx, network("tcp", target.Family), p.addr())
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	_ = conn.Close()
	return nil
}
