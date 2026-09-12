package probe

import (
	"context"
	"fmt"
	"net"
	"strings"
)

type dnsProbe struct {
	Resolver string
	Query    string
}

func (p *dnsProbe) String() string {
	return fmt.Sprintf("dns(%s via %s)", p.Query, p.Resolver)
}

func withDefaultPort(addr, port string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), port)
}

func (p *dnsProbe) Check(ctx context.Context, target Target) error {
	resolver := withDefaultPort(p.Resolver, "53")

	conn, err := BindToDeviceDialer(target.IfName).DialContext(ctx, network("udp", target.Family), resolver)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if _, err := conn.Write(buildDNSQuery(p.Query)); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if n < 4 {
		return fmt.Errorf("DNS response too short (%d bytes)", n)
	}

	rcode := buf[3] & 0x0f
	if rcode != 0 && rcode != 3 {
		return fmt.Errorf("DNS RCODE %d", rcode)
	}

	return nil
}

func buildDNSQuery(name string) []byte {
	var buf []byte

	buf = append(buf,
		0x12, 0x34,
		0x01, 0x00,
		0x00, 0x01,
		0x00, 0x00,
		0x00, 0x00,
		0x00, 0x00,
	)

	name = strings.TrimSuffix(name, ".")
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			continue
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0x00)

	buf = append(buf, 0x00, 0x01)
	buf = append(buf, 0x00, 0x01)

	return buf
}
