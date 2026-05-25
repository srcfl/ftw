package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	out, err := exec.Command("go", "run", ".", "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ftw-connect") {
		t.Fatalf("expected ftw-connect, got: %s", out)
	}
}
