package sysctl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadTreatsAMissingFileAsEmpty(t *testing.T) {
	if got := Read(filepath.Join(t.TempDir(), "nothing-here")); got != "" {
		t.Errorf("Read of a missing file = %q, want empty", got)
	}
	if got := Read(t.TempDir()); got != "" {
		t.Errorf("Read of a directory = %q, want empty", got)
	}
}

func TestReadTrims(t *testing.T) {
	p := filepath.Join(t.TempDir(), "accept_ra")
	if err := os.WriteFile(p, []byte("2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := Read(p); got != "2" {
		t.Errorf("Read = %q, want %q", got, "2")
	}
}

func TestIfacePath(t *testing.T) {
	want := "/proc/sys/net/ipv6/conf/eth1/proxy_ndp"
	if got := path("eth1", "proxy_ndp"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}
