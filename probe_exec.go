package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ExecProbe runs an external command to determine gateway health.
// Exit 0 means healthy; any non-zero exit code means unhealthy.
// This covers edge cases that the built-in probe types cannot handle.
type ExecProbe struct {
	Command []string
}

func (p *ExecProbe) String() string {
	return fmt.Sprintf("exec(%s)", strings.Join(p.Command, " "))
}

func (p *ExecProbe) Check(ctx context.Context, _ Target) error {
	if len(p.Command) == 0 {
		return fmt.Errorf("exec probe: no command configured")
	}
	cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("command failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
