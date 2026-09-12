package probe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

type icmpProbe struct {
	PayloadSize int

	seq uint16
}

const (
	icmpv4Echo      = 8
	icmpv4EchoReply = 0
	icmpv6Echo      = 128
	icmpv6EchoReply = 129
)

const icmpv6Filter = 1

func (p *icmpProbe) String() string {
	return fmt.Sprintf("icmp(payload=%d)", p.PayloadSize)
}

func (p *icmpProbe) Check(ctx context.Context, target Target) error {
	if len(target.GatewayIP) == 0 {
		return fmt.Errorf("no nexthop address on %s: icmp cannot probe a point-to-point link, use an http, tcp, dns, or exec probe", target.IfName)
	}

	if target.Family == syscall.AF_INET6 {
		return p.check(ctx, target, icmpv6)
	}
	return p.check(ctx, target, icmpv4)
}

type dialect struct {
	name string

	domain   int
	proto    int
	echo     byte
	reply    byte
	checksum bool

	sockaddr func(ip net.IP, ifIndex int) (syscall.Sockaddr, bool)

	filter func(fd int) error

	payload func(buf []byte, n int) ([]byte, bool)
}

var icmpv4 = dialect{
	name:     "IPv4",
	domain:   syscall.AF_INET,
	proto:    syscall.IPPROTO_ICMP,
	echo:     icmpv4Echo,
	reply:    icmpv4EchoReply,
	checksum: true,

	sockaddr: func(ip net.IP, _ int) (syscall.Sockaddr, bool) {
		v4 := ip.To4()
		if v4 == nil {
			return nil, false
		}
		dst := &syscall.SockaddrInet4{}
		copy(dst.Addr[:], v4)
		return dst, true
	},

	payload: func(buf []byte, n int) ([]byte, bool) {
		if n < 1 {
			return nil, false
		}
		ihl := int(buf[0]&0x0f) * 4
		if n < ihl {
			return nil, false
		}
		return buf[ihl:n], true
	},
}

var icmpv6 = dialect{
	name:   "IPv6",
	domain: syscall.AF_INET6,
	proto:  syscall.IPPROTO_ICMPV6,
	echo:   icmpv6Echo,
	reply:  icmpv6EchoReply,

	sockaddr: func(ip net.IP, ifIndex int) (syscall.Sockaddr, bool) {
		if ip.To4() != nil || ip.To16() == nil {
			return nil, false
		}
		dst := &syscall.SockaddrInet6{ZoneId: uint32(ifIndex)}
		copy(dst.Addr[:], ip.To16())
		return dst, true
	},

	filter: func(fd int) error {
		err := syscall.SetsockoptString(fd, syscall.IPPROTO_ICMPV6, icmpv6Filter,
			string(echoReplyFilter()))
		if err != nil {
			return fmt.Errorf("ICMPV6_FILTER: %w", err)
		}
		return nil
	},

	payload: func(buf []byte, n int) ([]byte, bool) { return buf[:n], true },
}

func (p *icmpProbe) check(ctx context.Context, target Target, d dialect) error {
	dst, addressed := d.sockaddr(target.GatewayIP, target.IfIndex)
	if !addressed {
		return fmt.Errorf("nexthop %s on %s is not an %s address, but the probe was started for %s",
			target.GatewayIP, target.IfName, d.name, d.name)
	}

	fd, err := syscall.Socket(d.domain, syscall.SOCK_RAW, d.proto)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer func() { _ = syscall.Close(fd) }()

	if err := syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, target.IfName); err != nil {
		return fmt.Errorf("SO_BINDTODEVICE: %w", err)
	}

	if d.filter != nil {
		if err := d.filter(fd); err != nil {
			return err
		}
	}

	id, seq := echoID(), p.nextSeq()
	msg := buildEcho(d.echo, id, seq, make([]byte, p.PayloadSize))
	if d.checksum {
		binary.BigEndian.PutUint16(msg[2:4], icmpChecksum(msg))
	}

	if err := sendEcho(ctx, fd, msg, dst); err != nil {
		return err
	}

	buf := make([]byte, 1500)
	for {
		n, err := recvEcho(ctx, fd, buf)
		if err != nil {
			return err
		}
		reply, sane := d.payload(buf, n)
		if !sane {
			continue
		}
		if matchEcho(reply, d.reply, id, seq) {
			return nil
		}
	}
}

func echoID() uint16 { return uint16(os.Getpid() & 0xffff) }

func (p *icmpProbe) nextSeq() uint16 {
	p.seq++
	return p.seq
}

func randomSeq() uint16 {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return binary.BigEndian.Uint16(b[:])
}

func buildEcho(msgType byte, id, seq uint16, payload []byte) []byte {
	msg := make([]byte, 8+len(payload))
	msg[0] = msgType
	msg[1] = 0
	binary.BigEndian.PutUint16(msg[4:6], id)
	binary.BigEndian.PutUint16(msg[6:8], seq)
	copy(msg[8:], payload)
	return msg
}

func matchEcho(msg []byte, replyType byte, id, seq uint16) bool {
	if len(msg) < 8 || msg[0] != replyType {
		return false
	}
	return binary.BigEndian.Uint16(msg[4:6]) == id && binary.BigEndian.Uint16(msg[6:8]) == seq
}

func echoReplyFilter() []byte {
	filt := make([]byte, 32)
	for i := range filt {
		filt[i] = 0xff
	}
	word := icmpv6EchoReply >> 5
	binary.NativeEndian.PutUint32(filt[word*4:], ^uint32(1<<(icmpv6EchoReply&31)))
	return filt
}

func sendEcho(ctx context.Context, fd int, msg []byte, dst syscall.Sockaddr) error {
	if err := setSocketTimeout(ctx, fd, syscall.SO_SNDTIMEO); err != nil {
		return err
	}
	if err := syscall.Sendto(fd, msg, 0, dst); err != nil {
		return fmt.Errorf("sendto: %w", err)
	}
	return nil
}

func recvEcho(ctx context.Context, fd int, buf []byte) (int, error) {
	if err := setSocketTimeout(ctx, fd, syscall.SO_RCVTIMEO); err != nil {
		return 0, err
	}
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return 0, fmt.Errorf("recvfrom: %w", err)
	}
	return n, nil
}

func setSocketTimeout(ctx context.Context, fd, opt int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	remaining := time.Until(dl)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	tv := syscall.NsecToTimeval(remaining.Nanoseconds())
	return syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, opt, &tv)
}

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
