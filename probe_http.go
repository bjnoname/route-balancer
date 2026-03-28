package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPProbe performs an HTTP/HTTPS request bound to the target interface.
// The custom dialer sets SO_BINDTODEVICE so the connection is forced through
// the gateway under test, preventing the circular-dependency problem.
type HTTPProbe struct {
	URL            string
	Method         string
	ExpectedStatus []int
	ExpectedBody   string
}

func (p *HTTPProbe) String() string {
	return fmt.Sprintf("http(%s %s)", p.Method, p.URL)
}

func (p *HTTPProbe) Check(ctx context.Context, target Target) error {
	transport := &http.Transport{
		DialContext:           bindToDeviceDialer(target.IfName).DialContext,
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
