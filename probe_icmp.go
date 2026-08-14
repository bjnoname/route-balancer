package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

var icmpSeq uint32

// ICMPProbe sends an ICMP echo request to the gateway and waits for a reply.
// The raw socket is bound to the target interface via SO_BINDTODEVICE so the
// probe travels through the gateway under test, not the current default route,
// avoiding the circular-dependency problem.
type ICMPProbe struct {
	PayloadSize int
}

func (p *ICMPProbe) String() string {
	return fmt.Sprintf("icmp(payload=%d)", p.PayloadSize)
}

func (p *ICMPProbe) Check(ctx context.Context, target Target) error {
	// Point-to-point links have no nexthop address, so there is nothing to
	// echo against. Fail loudly rather than silently pinging 0.0.0.0.
	if len(target.GatewayIP) == 0 {
		return fmt.Errorf("no nexthop address on %s: icmp cannot probe a point-to-point link, use an http, tcp, dns, or exec probe", target.IfName)
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer func() { _ = syscall.Close(fd) }()

	if err := syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, target.IfName); err != nil {
		return fmt.Errorf("SO_BINDTODEVICE: %w", err)
	}

	// Apply context deadline as socket-level receive/send timeouts.
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		tv := syscall.NsecToTimeval(remaining.Nanoseconds())
		_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
		_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &tv)
	}

	id := uint16(os.Getpid() & 0xffff)
	seq := uint16(atomic.AddUint32(&icmpSeq, 1) & 0xffff)
	payload := make([]byte, p.PayloadSize)
	msg := buildICMPEcho(id, seq, payload)

	dst := syscall.SockaddrInet4{}
	copy(dst.Addr[:], target.GatewayIP.To4())
	if err := syscall.Sendto(fd, msg, 0, &dst); err != nil {
		return fmt.Errorf("sendto: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return fmt.Errorf("recvfrom: %w", err)
		}
		// IP header length is encoded in the low nibble of byte 0, in 4-byte units.
		if n < 1 {
			continue
		}
		ihl := int(buf[0]&0x0f) * 4
		if n < ihl+8 {
			continue
		}
		icmp := buf[ihl:n]
		if icmp[0] != 0 { // type 0 = echo reply
			continue
		}
		rxID := binary.BigEndian.Uint16(icmp[4:6])
		rxSeq := binary.BigEndian.Uint16(icmp[6:8])
		if rxID == id && rxSeq == seq {
			return nil
		}
	}
}

// buildICMPEcho constructs an ICMP echo request message with the given
// identifier, sequence number, and payload.
func buildICMPEcho(id, seq uint16, payload []byte) []byte {
	msg := make([]byte, 8+len(payload))
	msg[0] = 8 // type: echo request
	msg[1] = 0 // code
	// checksum at [2:4] — computed after filling the rest
	binary.BigEndian.PutUint16(msg[4:6], id)
	binary.BigEndian.PutUint16(msg[6:8], seq)
	copy(msg[8:], payload)
	binary.BigEndian.PutUint16(msg[2:4], icmpChecksum(msg))
	return msg
}

// icmpChecksum computes the standard Internet (ones-complement) checksum.
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 != 0 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
