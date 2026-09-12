package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"

	"github.com/bjnoname/route-balancer/internal/config"
)

type Checker interface {
	Check(ctx context.Context, target Target) error
	String() string
}

type Target struct {
	Family int

	GatewayIP net.IP
	IfName    string
	IfIndex   int
}

func (t Target) SameAs(o Target) bool {
	return t.Family == o.Family && t.IfName == o.IfName && t.IfIndex == o.IfIndex &&
		t.GatewayIP.Equal(o.GatewayIP)
}

func network(base string, family int) string {
	if family == syscall.AF_INET6 {
		return base + "6"
	}
	return base + "4"
}

func BindToDeviceDialer(ifName string) *net.Dialer {
	return &net.Dialer{
		Control: func(_, _ string, c syscall.RawConn) error {
			var bind error
			if err := c.Control(func(fd uintptr) {
				bind = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifName)
			}); err != nil {
				return err
			}
			if bind != nil {
				return fmt.Errorf("SO_BINDTODEVICE %s: %w", ifName, bind)
			}
			return nil
		},
	}
}

func IsICMP(pcfg config.Probe) bool {
	switch pcfg.Type {
	case "http", "https", "tcp", "dns", "exec":
		return false
	default:
		return true
	}
}

func Validate(pcfg config.Probe) error {
	switch pcfg.Type {
	case "http", "https":
		if pcfg.URL == "" {
			return errors.New("an http probe needs a url")
		}
		u, err := url.ParseRequestURI(pcfg.URL)
		if err != nil {
			return fmt.Errorf("url %q: %w", pcfg.URL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("url %q: scheme is %q, want http or https", pcfg.URL, u.Scheme)
		}
	case "tcp":
		if pcfg.Host == "" {
			return errors.New("a tcp probe needs a host")
		}
		if pcfg.Port == 0 {
			return errors.New("a tcp probe needs a port")
		}
	case "dns":
		if pcfg.Resolver == "" {
			return errors.New("a dns probe needs a resolver")
		}
	case "exec":
		if len(pcfg.Command) == 0 {
			return errors.New("an exec probe needs a command")
		}
	case "icmp", "", config.ProbeNone:

	default:
		return fmt.Errorf("unknown probe type %q (want icmp, http, https, tcp, dns, exec or none)",
			pcfg.Type)
	}
	return nil
}

func New(pcfg config.Probe) Checker {
	switch pcfg.Type {
	case "http", "https":
		method := pcfg.Method
		if method == "" {
			method = "GET"
		}
		return &httpProbe{
			URL:            pcfg.URL,
			Method:         method,
			ExpectedStatus: pcfg.ExpectedStatus,
			ExpectedBody:   pcfg.ExpectedBody,
		}
	case "tcp":
		return &tcpProbe{Host: pcfg.Host, Port: pcfg.Port}
	case "dns":
		query := pcfg.Query
		if query == "" {
			query = "example.com"
		}
		return &dnsProbe{Resolver: pcfg.Resolver, Query: query}
	case "exec":
		return &execProbe{Command: pcfg.Command}
	default:
		size := pcfg.PayloadSize
		if size <= 0 {
			size = 56
		}
		return &icmpProbe{PayloadSize: size, seq: randomSeq()}
	}
}
