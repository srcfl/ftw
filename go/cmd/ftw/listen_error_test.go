package main

import (
	"net"
	"strings"
	"syscall"
	"testing"
)

func TestExplainBindErrorNamesATakenPortThatIsNotFTW(t *testing.T) {
	err := explainBindError("127.0.0.1:1", &net.OpError{Err: syscall.EADDRINUSE})
	if err == nil || !strings.Contains(err.Error(), "did not answer as FTW") || !strings.Contains(err.Error(), "-port") {
		t.Fatal(err)
	}
}
