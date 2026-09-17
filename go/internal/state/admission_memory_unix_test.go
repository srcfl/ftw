//go:build linux || darwin

package state

import (
	"runtime"
	"syscall"
	"testing"
)

func checkStorageAdmissionRSS(t *testing.T) {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	bytes := int64(usage.Maxrss) * 1024
	if runtime.GOOS == "darwin" {
		bytes = int64(usage.Maxrss)
	}
	t.Logf("kernel peak RSS: %.2f MiB", float64(bytes)/(1<<20))
	if bytes > 256<<20 {
		t.Fatalf("peak RSS exceeds target: %d bytes", bytes)
	}
}
