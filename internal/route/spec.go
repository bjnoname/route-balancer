package route

import (
	"fmt"
	"strconv"
	"strings"
)

type ECMPSpec struct {
	Family   int
	Metric   string
	Proto    string
	Nexthops []NexthopSpec
}

type NexthopSpec struct {
	Via    string
	Dev    string
	Weight int
}

func (e ECMPSpec) Args(verb string) []string {
	args := []string{FamilyFlag(e.Family), "route", verb, "default",
		"metric", e.Metric, "proto", e.Proto}
	for _, nh := range e.Nexthops {
		args = append(args, "nexthop")
		if nh.Via != "" {
			args = append(args, "via", nh.Via)
		}
		args = append(args, "dev", nh.Dev, "weight", strconv.Itoa(nh.Weight))
	}
	return args
}

func (e ECMPSpec) String() string { return strings.Join(e.Args("add"), " ") }

func (e ECMPSpec) nexthopMap() map[nexthopKey]int {
	want := make(map[nexthopKey]int, len(e.Nexthops))
	for _, nh := range e.Nexthops {
		want[nexthopKey{via: nh.Via, dev: nh.Dev}] = nh.Weight
	}
	return want
}

type TableSpec struct {
	Family  int
	IfIndex int
	IfName  string
	Table   string
	Src     string
	Subnet  string
	Via     string
	Prio    string
}

func (t TableSpec) AddrFamily() int {
	if t.Family == FamilyV6 {
		return FamilyV6
	}
	return FamilyV4
}

func (t TableSpec) String() string {
	return fmt.Sprintf("%s dev %s table %s src %s subnet %q via %q prio %s",
		FamilyName(t.AddrFamily()), t.IfName, t.Table, t.Src, t.Subnet, t.Via, t.Prio)
}

type FwmarkSpec struct {
	Mark  int
	Table string
	Prio  string
}

func (f FwmarkSpec) String() string {
	return fmt.Sprintf("fwmark %d lookup %s priority %s", f.Mark, f.Table, f.Prio)
}
