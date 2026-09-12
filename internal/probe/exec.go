package probe

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type execProbe struct {
	Command []string
}

func (p *execProbe) String() string {
	return fmt.Sprintf("exec(%s)", strings.Join(p.Command, " "))
}

func (p *execProbe) Check(ctx context.Context, _ Target) error {
	if len(p.Command) == 0 {
		return fmt.Errorf("exec probe: no command configured")
	}
	cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("command failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
