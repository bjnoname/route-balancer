package probe

import (
	"context"
	"syscall"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
)

func Query(ctx context.Context, c Checker, t Target, timeout time.Duration) command.Query {
	return command.Query{
		Tool: command.ToolProbe,
		Args: []string{c.String(), t.IfName, familyName(t.Family)},
		Read: func() (any, error) {
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			return "", c.Check(probeCtx, t)
		},
	}
}

func familyName(family int) string {
	if family == syscall.AF_INET6 {
		return "v6"
	}
	return "v4"
}
