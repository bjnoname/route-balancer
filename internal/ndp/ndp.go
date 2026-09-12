package ndp

import (
	"net"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
)

type Options struct {
	Table    string
	Proto    string
	MaxHosts int
	Timeout  time.Duration
	Uplinks  []Uplink
}

type Uplink struct {
	Name    string
	Subnets []Subnet
}

type Subnet struct {
	Name   string
	Prefix *net.IPNet
}

func (o Options) Enabled() bool { return len(o.Uplinks) > 0 }

func (o Options) UplinkNames() []string {
	out := make([]string, 0, len(o.Uplinks))
	for _, u := range o.Uplinks {
		out = append(out, u.Name)
	}
	return out
}

func (o Options) proxies(name string) bool {
	for _, u := range o.Uplinks {
		if u.Name == name {
			return true
		}
	}
	return false
}

func ipAction(order int, args ...string) command.Action {
	return command.Action{
		Tool:     command.ToolIP,
		Args:     args,
		OnFail:   command.FailWarn,
		Tolerate: []string{"File exists"},
		Order:    order,
	}
}
