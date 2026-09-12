package netlink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"
)

func Subscribe(ctx context.Context, name string, groups uint32,
	onMsg func(msg []byte), onOverrun func()) error {

	sock, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("%s socket: %w", name, err)
	}
	SetReceiveBuffer(sock)

	addr := syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: groups}
	if err := syscall.Bind(sock, &addr); err != nil {
		_ = syscall.Close(sock)
		return fmt.Errorf("%s bind: %w", name, err)
	}

	go func() {
		<-ctx.Done()
		_ = syscall.Close(sock)
	}()
	go readSocket(ctx, name, sock, onMsg, onOverrun)

	return nil
}

func readSocket(ctx context.Context, name string, sock int, onMsg func([]byte), onOverrun func()) {
	buf := make([]byte, 8192)

	for {
		n, _, err := syscall.Recvfrom(sock, buf, 0)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if IsOverrun(err) {
				slog.Warn("Netlink receive buffer overrun — re-reading rather than waiting",
					"socket", name)
				onOverrun()
				continue
			}
			slog.Error("netlink recvfrom error", "socket", name, "error", err)
			continue
		}

		for _, msg := range SplitMessages(buf[:n]) {
			onMsg(msg)
		}
	}
}

const ReceiveBufferBytes = 1 << 20

func SetReceiveBuffer(sock int) {
	if err := syscall.SetsockoptInt(sock, syscall.SOL_SOCKET, syscall.SO_RCVBUFFORCE, ReceiveBufferBytes); err == nil {
		return
	}
	if err := syscall.SetsockoptInt(sock, syscall.SOL_SOCKET, syscall.SO_RCVBUF, ReceiveBufferBytes); err != nil {
		slog.Warn("Could not enlarge the netlink receive buffer — a burst may be dropped",
			"wanted", ReceiveBufferBytes, "error", err)
	}
}

func IsOverrun(err error) bool { return errors.Is(err, syscall.ENOBUFS) }
