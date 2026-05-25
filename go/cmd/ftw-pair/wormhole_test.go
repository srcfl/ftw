package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	wh "github.com/frahlg/forty-two-watts/go/internal/wormhole"
)

// TestWormholeForwardEndToEnd performs a real end-to-end forwarding test using
// the fowld subprocess.  It is skipped unless WORMHOLE_TEST=1 is set, because
// it requires internet access to the magic-wormhole rendezvous server and
// fowl to be installed.
func TestWormholeForwardEndToEnd(t *testing.T) {
	if os.Getenv("WORMHOLE_TEST") == "" {
		t.Skip("set WORMHOLE_TEST=1 to run against real rendezvous (needs internet + fowl)")
	}
	if _, err := exec.LookPath("fowld"); err != nil {
		t.Skipf("fowld not installed — `uv tool install fowl` to enable this test: %v", err)
	}

	// Stand up a local echo server that the wormhole will forward to.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server listen: %v", err)
	}
	defer echoLn.Close()

	go func() {
		c, err := echoLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("PONG\n")) //nolint:errcheck
	}()

	// Start the host side via the shim.
	ctx := context.Background()
	host, err := StartWormholeHost(ctx, echoLn.Addr().String())
	if err != nil {
		t.Fatalf("StartWormholeHost: %v", err)
	}
	defer host.Close()
	t.Logf("wormhole code: %s", host.Code)

	// Start the client side via the shim.
	client, err := ConnectWormholeClient(ctx, host.Code)
	if err != nil {
		t.Fatalf("ConnectWormholeClient: %v", err)
	}
	defer client.Close()
	t.Logf("client local addr: %s", client.LocalAddr)

	// Dial the client's local port and verify the echo response.
	conn, err := net.DialTimeout("tcp", client.LocalAddr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial client local addr: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 8)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read from forwarded connection: %v", err)
	}
	if got := string(buf[:n]); got != "PONG\n" {
		t.Fatalf("expected %q, got %q", "PONG\n", got)
	}
}

// TestFowlMissing verifies that StartWormholeHost returns *wh.ErrFowlNotFound
// when fowld is not on PATH.
func TestFowlMissing(t *testing.T) {
	const emptyPath = "/usr/bin:/bin"

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", emptyPath)     //nolint:errcheck
	defer os.Setenv("PATH", oldPath) //nolint:errcheck

	if _, err := exec.LookPath("fowld"); err == nil {
		t.Skipf("fowld is in %s — cannot test missing-fowl branch", emptyPath)
	}

	_, err := StartWormholeHost(context.Background(), "127.0.0.1:9999")
	if err == nil {
		t.Fatal("expected error when fowld missing, got nil")
	}
	var notFound *wh.ErrFowlNotFound
	if !isErrFowlNotFound(err, &notFound) {
		t.Fatalf("expected *wh.ErrFowlNotFound, got %T: %v", err, err)
	}
}

// isErrFowlNotFound type-asserts err to *wh.ErrFowlNotFound.
func isErrFowlNotFound(err error, target **wh.ErrFowlNotFound) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*wh.ErrFowlNotFound); ok {
		if target != nil {
			*target = e
		}
		return true
	}
	return false
}
