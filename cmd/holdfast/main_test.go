package main

import "testing"

func TestResolveVersion(t *testing.T) {
	if got := resolveVersion("1.2.3"); got != "1.2.3" {
		t.Errorf("the version from ldflags must be kept: %q", got)
	}
	// In tests the module is built from source: the version "(devel)" stays "dev".
	if got := resolveVersion("dev"); got != "dev" {
		t.Errorf("local build: %q", got)
	}
}
