package sysctl

import (
	"os"
	"strings"
)

func path(iface, name string) string {
	return "/proc/sys/net/ipv6/conf/" + iface + "/" + name
}

func Read(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func ReadIface(iface, name string) string { return Read(path(iface, name)) }

func WriteIface(iface, name, value string) (changed bool, err error) {
	p := path(iface, name)
	if Read(p) == value {
		return false, nil
	}
	if err := os.WriteFile(p, []byte(value+"\n"), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
