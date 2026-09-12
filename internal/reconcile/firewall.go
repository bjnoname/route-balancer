package reconcile

import (
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/route"
)

func (d *reconciler) cleanupMangleActions() []command.Action {
	acts := append(d.r.Fw.CleanupIptablesActions(), d.r.Fw.CleanupNftActions()...)
	return append(acts, d.r.Fw.CleanupNft6Actions()...)
}

func (d *reconciler) cleanupFwmarkActions() []command.Action {
	fw := d.r.Fw

	var acts []command.Action
	for _, f := range []struct {
		family int
		rules  []config.Rule
	}{
		{route.FamilyV4, fw.Rules},
		{route.FamilyV6, fw.Rules6},
	} {
		for _, rule := range f.rules {
			mark := fw.Mark(rule.Gateway)
			if mark == 0 {
				continue
			}

			ifIndex, ok := d.state.Links.Ifaces[rule.Gateway]
			if !ok {
				continue
			}

			spec, usable := fwmarkSpec(d.r.Rt, mark, ifIndex)
			if !usable {
				continue
			}
			acts = append(acts, route.DeleteFwmarkAction(f.family, spec))
		}
	}
	return acts
}
