package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/settings"
)

type obs struct{ *commandtest.Asker }

func newObs(out map[string]string) obs { return obs{commandtest.New(out)} }

func (o obs) Observe(qs []command.Query) command.Answers {
	return command.Observe(o.Asker, qs)
}

type testReconciler struct {
	*reconciler
	obs

	offered [][]ndp.Claim
}

func newTestReconciler(t *testing.T) *testReconciler {
	t.Helper()

	d, err := newTestReconcilerWith(t, config.Default())
	if err != nil {
		t.Fatalf("newReconciler: %v", err)
	}
	return d
}

func newTestReconcilerWith(t *testing.T, c *config.Config) (*testReconciler, error) {
	t.Helper()

	set, err := settings.Resolve(c)
	if err != nil {
		return nil, err
	}

	td := &testReconciler{obs: newObs(map[string]string{})}

	in := actor.NewMailbox[Event](QueueDepth)
	actor.New(context.Background(), "Event loop", in, func(self *actor.Actor[Event]) actor.Behaviour {
		td.reconciler = newReconciler(set, in, self, td.offer)
		return td.reconciler
	})

	return td, nil
}

func (d *testReconciler) offer(claims []ndp.Claim) command.Action {
	return command.Action{
		Tool:  command.ToolProbeClaim,
		Args:  claimAddrs(claims),
		Order: command.OrderProxyEntry,
		Write: func() (string, error) {
			d.offered = append(d.offered, claims)
			return "", nil
		},
	}
}

func (d *testReconciler) useAsker(out map[string]string) obs {
	d.obs = newObs(out)
	return d.obs
}

func (d *testReconciler) useConfig(t *testing.T, c *config.Config) {
	t.Helper()
	if c == nil {
		c = config.Default()
	} else {
		c.SetDefaults()
	}
	set, err := settings.Resolve(c)
	if err != nil {
		t.Fatalf("settings.Resolve: %v", err)
	}
	d.cfg, d.r = c, set
}

func useRunner() *commandtest.Runner { return commandtest.NewRunner() }

func (d *testReconciler) proxyPass(inv Inventory) []command.Action {
	d.absorbInstalled(d.Observe(d.queryPlan()))
	return plan(d.proxyEntryResource(inv, time.Now()))
}

func (d *testReconciler) useNPTv6(t *testing.T, n *config.NPTv6) {
	t.Helper()
	d.useConfig(t, &config.Config{NPTv6: n})
}

func (d *testReconciler) useGatewayConfig(t *testing.T, gws map[string]config.Gateway) {
	t.Helper()
	c := *d.cfg
	c.Gateways = gws
	d.useConfig(t, &c)
}

func (d *testReconciler) look() installedState {
	d.absorbInstalled(d.Observe(d.queryPlan()))
	return d.state.Installed
}

func (d *testReconciler) post(ev Event) { actor.Post(context.Background(), d.events, ev) }

func (d *testReconciler) queued() int { return d.events.Len() }

func (d *testReconciler) fold() { d.foldBatch(context.Background()) }

func commands(acts []command.Action) []string {
	r := commandtest.NewRunner()
	r.RunAll(acts)
	return r.Commands()
}

func claimAddrs(claims []ndp.Claim) []string {
	addrs := make([]string, 0, len(claims))
	for _, c := range ndp.SortedClaims(claims) {
		addrs = append(addrs, c.Entry.Addr)
	}
	return addrs
}
