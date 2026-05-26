//go:build windows

package main

import (
	"os/exec"
	"testing"
)

// TestSetPairSysProcAttrWindows verifies that the Windows stub is a no-op
// and does not panic. ftw-pair is not shipped on Windows, so this function
// is never reached in production — but it must compile and not crash.
func TestSetPairSysProcAttrWindows(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	// Should not panic, should leave SysProcAttr nil.
	setPairSysProcAttr(cmd)
	if cmd.SysProcAttr != nil {
		t.Fatal("expected SysProcAttr to remain nil on windows (no-op stub)")
	}
}
