package main

import (
	"context"
	"fmt"
	"strings"
)

// DNSProbe sends a DNS A-record query to a specific resolver via the target
// interface and checks that a valid (non-SERVFAIL) response is returned.
// The UDP socket is bound via SO_BINDTODEVICE so queries travel through the
// gateway under test.
type DNSProbe struct {
	Resolver string // host:port, e.g. "8.8.8.8:53"
	Query    string // domain name, e.g. "example.com"
}

func (p *DNSProbe) String() string {
	return fmt.Sprintf("dns(%s via %s)", p.Query, p.Resolver)
}

func (p *DNSProbe) Check(ctx context.Context, target Target) error {
	resolver := p.Resolver
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}

	conn, err := bindToDeviceDialer(target.IfName).DialContext(ctx, "udp", resolver)
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

	// RCODE is the low 4 bits of byte 3 in the DNS header.
	// 0=NOERROR, 3=NXDOMAIN — both indicate a functioning resolver.
	rcode := buf[3] & 0x0f
	if rcode != 0 && rcode != 3 {
		return fmt.Errorf("DNS RCODE %d", rcode)
	}

	return nil
}

// buildDNSQuery constructs a minimal DNS wire-format query for an A record.
func buildDNSQuery(name string) []byte {
	var buf []byte

	// Header
	buf = append(buf,
		0x12, 0x34, // ID
		0x01, 0x00, // Flags: QR=0, RD=1
		0x00, 0x01, // QDCOUNT = 1
		0x00, 0x00, // ANCOUNT = 0
		0x00, 0x00, // NSCOUNT = 0
		0x00, 0x00, // ARCOUNT = 0
	)

	// QNAME: encode as length-prefixed labels, terminated by a zero byte.
	name = strings.TrimSuffix(name, ".")
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			continue
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0x00) // root label

	buf = append(buf, 0x00, 0x01) // QTYPE  = A
	buf = append(buf, 0x00, 0x01) // QCLASS = IN

	return buf
}
