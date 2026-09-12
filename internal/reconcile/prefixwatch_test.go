package reconcile

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/prefix"
)

func TestRecordLeaseAcquire(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}

	l := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 56}
	_, had, changed := d.recordLease(prefix.Change{Lease: l})

	if had {
		t.Error("recordLease reported a previous lease where there was none")
	}
	if !changed {
		t.Error("acquiring a prefix was not reported as a change")
	}
	if got, ok := d.state.Prefixes.currentLease("wan0"); !ok || !got.SameAs(l) {
		t.Errorf("currentLease = %v/%v, want the acquired prefix", got, ok)
	}
}

func TestRecordLeaseRenewalIsNotAChange(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}

	l := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 56}
	d.recordLease(prefix.Change{Lease: l})

	_, had, changed := d.recordLease(prefix.Change{Lease: l})
	if !had {
		t.Error("recordLease lost the existing lease")
	}
	if changed {
		t.Error("an unchanged renewal was reported as a change, which would churn the rule set")
	}
}

func TestRecordLeaseNewPrefixSameLength(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}

	d.recordLease(prefix.Change{Lease: prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:aaaa::"), Length: 56}})
	prev, had, changed := d.recordLease(prefix.Change{Lease: prefix.Lease{
		Uplink: "wan0", Prefix: net.ParseIP("2001:db8:bbbb::"), Length: 56}})

	if !had || !changed {
		t.Fatalf("had = %v, changed = %v; want both true", had, changed)
	}
	if !prev.Prefix.Equal(net.ParseIP("2001:db8:aaaa::")) {
		t.Errorf("prev = %v, want the prefix being replaced", prev)
	}
}

func TestRecordLeaseLengthChange(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}

	d.recordLease(prefix.Change{Lease: prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 56}})
	_, _, changed := d.recordLease(prefix.Change{Lease: prefix.Lease{
		Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 60}})

	if !changed {
		t.Error("a delegation length change was not reported as a change")
	}
	if got, _ := d.state.Prefixes.currentLease("wan0"); got.Length != 60 {
		t.Errorf("stored length = %d, want 60", got.Length)
	}
}

func TestRecordLeaseWithdraw(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}

	l := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 56}
	d.recordLease(prefix.Change{Lease: l})

	if _, _, changed := d.recordLease(prefix.Change{Lease: l, Gone: true}); !changed {
		t.Error("withdrawing the held prefix was not reported as a change")
	}
	if _, ok := d.state.Prefixes.currentLease("wan0"); ok {
		t.Error("the lease survived its withdrawal")
	}
}

func TestRecordLeaseStaleWithdrawIgnored(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}

	old := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:aaaa::"), Length: 56}
	current := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:bbbb::"), Length: 56}
	d.recordLease(prefix.Change{Lease: current})

	if _, _, changed := d.recordLease(prefix.Change{Lease: old, Gone: true}); changed {
		t.Error("a stale withdrawal was applied, dropping the current prefix")
	}
	if got, ok := d.state.Prefixes.currentLease("wan0"); !ok || !got.SameAs(current) {
		t.Errorf("currentLease = %v, want the replacement prefix to survive", got)
	}
}

func TestLeaseWireRecordsAndClearsAWithdrawal(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}
	d.state.Prefixes.Withdrawn = map[string]bool{}

	l := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 64}

	d.handleLeaseWire(prefix.Change{Lease: l, Gone: true})
	if !d.state.Prefixes.Withdrawn["wan0"] {
		t.Error("a withdrawal on the wire was not recorded, so a lingering address will resurrect the prefix")
	}

	d.handleLeaseWire(prefix.Change{Lease: l})
	if d.state.Prefixes.Withdrawn["wan0"] {
		t.Error("a re-advertised prefix left the uplink suppressed, so the re-read will keep refusing it")
	}
}

func TestLeaseWireSuppressesOnlyItsOwnUplink(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}
	d.state.Prefixes.Withdrawn = map[string]bool{}

	d.handleLeaseWire(prefix.Change{
		Lease: prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 64},
		Gone:  true,
	})

	if d.state.Prefixes.Withdrawn["wan1"] {
		t.Error("withdrawing wan0's prefix suppressed wan1")
	}
}

func TestEmitQueuesTheChangeUnaltered(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	queue := d.inbox

	c := prefix.Change{Lease: prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8::"), Length: 56}}
	d.emit(context.Background(), c)

	select {
	case ev := <-queue:
		lc, ok := ev.(LeaseChanged)
		if !ok {
			t.Fatalf("queued a %T, want a LeaseChanged", ev)
		}
		if !lc.Change.Lease.SameAs(c.Lease) || lc.Change.Gone {
			t.Errorf("queued %+v, want the change verbatim", lc.Change)
		}
	default:
		t.Fatal("emit queued nothing")
	}
}

func TestRequestResyncQueuesAPass(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	queue := d.inbox

	d.requestResync(context.Background())

	select {
	case ev := <-queue:
		if _, ok := ev.(Resync); !ok {
			t.Fatalf("queued a %T, want a Resync", ev)
		}
	default:
		t.Fatal("requestResync queued nothing")
	}
}

func TestCallbacksGiveUpOnShutdown(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for name, call := range map[string]func(){
		"emit":          func() { d.emit(ctx, prefix.Change{}) },
		"requestResync": func() { d.requestResync(ctx) },
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				call()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s blocked on a cancelled context", name)
			}
		})
	}
}

func TestAddressTriggerBurstCostsOnePass(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	for i := 0; i < 8; i++ {
		d.post(Resync{})
	}

	d.fold()

	if d.queued() != 0 {
		t.Errorf("%d events left queued, want the fold to have taken them all", d.queued())
	}
}
