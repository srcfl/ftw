package wormhole

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWormholeForwardEndToEnd performs a real end-to-end forwarding test using
// the fowld subprocess.  It is skipped unless WORMHOLE_TEST=1 is set, because
// it requires internet access to the magic-wormhole rendezvous server and
// fowl to be installed.
func TestWormholeForwardEndToEnd(t *testing.T) {
	if os.Getenv("WORMHOLE_TEST") == "" {
		t.Skip("set WORMHOLE_TEST=1 to run against real rendezvous (needs internet + fowl)")
	}
	if _, err := exec.LookPath(fowldBinary); err != nil {
		t.Skipf("fowld not installed — `pipx install fowl` to enable this test: %v", err)
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

	// Start the host side, forwarding to the echo server.
	ctx := context.Background()
	host, err := StartHost(ctx, echoLn.Addr().String())
	if err != nil {
		t.Fatalf("StartHost: %v", err)
	}
	defer host.Close()
	t.Logf("wormhole code: %s", host.Code)

	// Start the client side using the host's code.
	client, err := Connect(ctx, host.Code)
	if err != nil {
		t.Fatalf("Connect: %v", err)
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

// TestFowlMissing verifies that StartHost returns ErrFowlNotFound when
// fowld is not on PATH.  It temporarily overrides PATH to a directory where
// fowld is unlikely to exist.
func TestFowlMissing(t *testing.T) {
	// Use a directory that almost certainly does not contain fowld.
	const emptyPath = "/usr/bin:/bin"

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", emptyPath)     //nolint:errcheck
	defer os.Setenv("PATH", oldPath) //nolint:errcheck

	if _, err := exec.LookPath(fowldBinary); err == nil {
		t.Skipf("fowld is in %s — cannot test missing-fowl branch", emptyPath)
	}

	_, err := StartHost(context.Background(), "127.0.0.1:9999")
	if err == nil {
		t.Fatal("expected error when fowld missing, got nil")
	}
	var notFound *ErrFowlNotFound
	if !isErrFowlNotFound(err, &notFound) {
		t.Fatalf("expected *ErrFowlNotFound, got %T: %v", err, err)
	}
}

// TestSplitCompositeCode validates the code-splitting helper.
func TestSplitCompositeCode(t *testing.T) {
	cases := []struct {
		code        string
		wantFowl    string
		wantPort    string
		wantErrFrag string
	}{
		{"7-spinach-atlas:9876", "7-spinach-atlas", "9876", ""},
		{"2-retrieval-robust:1234", "2-retrieval-robust", "1234", ""},
		{"nocolon", "", "", "expected"},
		{":9999", "", "", "non-empty"},
		{"7-spinach-atlas:", "", "", "non-empty"},
	}
	for _, tc := range cases {
		fowlCode, port, err := splitCompositeCode(tc.code)
		if tc.wantErrFrag != "" {
			if err == nil {
				t.Errorf("splitCompositeCode(%q): expected error containing %q, got nil", tc.code, tc.wantErrFrag)
				continue
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Errorf("splitCompositeCode(%q): error %q does not contain %q", tc.code, err, tc.wantErrFrag)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitCompositeCode(%q): unexpected error: %v", tc.code, err)
			continue
		}
		if fowlCode != tc.wantFowl || port != tc.wantPort {
			t.Errorf("splitCompositeCode(%q): got (%q, %q), want (%q, %q)",
				tc.code, fowlCode, port, tc.wantFowl, tc.wantPort)
		}
	}
}

// TestPickFreePort verifies that pickFreePort returns a usable port.
func TestPickFreePort(t *testing.T) {
	port, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("pickFreePort returned out-of-range port %d", port)
	}
	// Verify we can actually listen on the returned port.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen on picked port %d: %v", port, err)
	}
	ln.Close()
}

// isErrFowlNotFound is a helper that type-asserts err to *ErrFowlNotFound via
// errors.As semantics without importing errors (to keep the test file minimal).
func isErrFowlNotFound(err error, target **ErrFowlNotFound) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*ErrFowlNotFound); ok {
		if target != nil {
			*target = e
		}
		return true
	}
	return false
}
