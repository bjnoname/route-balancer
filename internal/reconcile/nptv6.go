package reconcile

import "github.com/bjnoname/route-balancer/internal/command"

func (d *reconciler) cleanupNptv6Actions() []command.Action {
	return []command.Action{d.r.Npt.DeleteAction()}
}
