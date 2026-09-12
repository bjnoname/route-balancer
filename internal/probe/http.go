package probe

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type httpProbe struct {
	URL            string
	Method         string
	ExpectedStatus []int
	ExpectedBody   string
}

func (p *httpProbe) String() string {
	return fmt.Sprintf("http(%s %s)", p.Method, p.URL)
}

func (p *httpProbe) Check(ctx context.Context, target Target) error {
	dialer := BindToDeviceDialer(target.IfName)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, netw, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network(netw, target.Family), addr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if len(p.ExpectedStatus) > 0 {
		matched := false
		for _, s := range p.ExpectedStatus {
			if resp.StatusCode == s {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
	}

	if p.ExpectedBody != "" {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}
		if !strings.Contains(string(body), p.ExpectedBody) {
			return fmt.Errorf("body does not contain %q", p.ExpectedBody)
		}
	}

	return nil
}
