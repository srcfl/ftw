//go:build !windows

package main

import (
	"os/exec"
	"testing"
)

// TestSetPairSysProcAttrUnix verifies that setPairSysProcAttr sets Setpgid on unix
// platforms, ensuring the sidecar runs in its own process group.
func TestSetPairSysProcAttrUnix(t *testing.T) {
	cmd := exec.Command("true")
	setPairSysProcAttr(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr to be set on unix")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected Setpgid=true on unix")
	}
}
