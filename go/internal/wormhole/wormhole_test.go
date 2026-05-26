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

// TestFowldErrorTextEmptyMessage checks that errorText() falls back to the raw
// event JSON when all named message fields are empty — the exact scenario that
// produced the blank "fowld error: " in the original bug.
func TestFowldErrorTextEmptyMessage(t *testing.T) {
	ev := &fowldEvent{
		Kind: "error",
		raw:  `{"kind":"error"}`,
	}
	got := ev.errorText()
	if !strings.Contains(got, "raw event") {
		t.Errorf("expected errorText() to contain 'raw event', got: %q", got)
	}
	if !strings.Contains(got, ev.raw) {
		t.Errorf("expected errorText() to embed the raw JSON, got: %q", got)
	}
}

// TestFowldErrorTextNamedFields checks that errorText() prefers named fields
// when they are present, and that it concatenates multiple non-empty fields.
func TestFowldErrorTextNamedFields(t *testing.T) {
	cases := []struct {
		ev   fowldEvent
		want string
	}{
		{
			fowldEvent{Kind: "error", Message: "rendezvous unreachable", raw: `{"kind":"error","message":"rendezvous unreachable"}`},
			"rendezvous unreachable",
		},
		{
			fowldEvent{Kind: "error", Summary: "PAKE failure", Detail: "wrong code", raw: `{}`},
			"PAKE failure",
		},
		{
			fowldEvent{Kind: "error", Message: "transport error", Summary: "detail here", raw: `{}`},
			"transport error",
		},
	}
	for _, tc := range cases {
		got := tc.ev.errorText()
		if !strings.Contains(got, tc.want) {
			t.Errorf("errorText() = %q, want substring %q", got, tc.want)
		}
		// Must not fall back to the raw-event path when named fields are present.
		if strings.Contains(got, "raw event") {
			t.Errorf("errorText() unexpectedly fell back to raw event: %q", got)
		}
	}
}

// TestBuildFowldErrorIncludesStderr verifies that buildFowldError appends
// captured stderr lines to the returned error string.
func TestBuildFowldErrorIncludesStderr(t *testing.T) {
	ring := newStderrRing(10)
	ring.add("Traceback (most recent call last):")
	ring.add("  File \"fowl/_proto.py\", line 1891")
	ring.add("ValueError: no code")

	err := buildFowldError("connection refused", "", ring)
	msg := err.Error()

	if !strings.Contains(msg, "fowld error: connection refused") {
		t.Errorf("missing primary message in: %q", msg)
	}
	if !strings.Contains(msg, "fowld stderr:") {
		t.Errorf("missing stderr section in: %q", msg)
	}
	if !strings.Contains(msg, "ValueError: no code") {
		t.Errorf("missing stderr content in: %q", msg)
	}
}

// TestBuildFowldErrorNoStderr verifies that when the ring buffer is empty the
// error message is concise and does not contain a "fowld stderr:" section.
func TestBuildFowldErrorNoStderr(t *testing.T) {
	ring := newStderrRing(10)
	err := buildFowldError("wormhole code already used", "", ring)
	msg := err.Error()

	if !strings.Contains(msg, "wormhole code already used") {
		t.Errorf("missing primary message in: %q", msg)
	}
	if strings.Contains(msg, "fowld stderr:") {
		t.Errorf("unexpected stderr section when ring is empty: %q", msg)
	}
}

// TestBuildFowldErrorNonJSON verifies that non-JSON stdout lines are included
// as the "raw" fragment — this covers the LocalCommandDispatch.lineReceived
// error path that fowl emits as plain text.
func TestBuildFowldErrorNonJSON(t *testing.T) {
	ring := newStderrRing(10)
	rawLine := "b'{\"kind\":\"bad-command\"}': failed: 'bad-command'"
	err := buildFowldError("non-JSON stdout", rawLine, ring)
	msg := err.Error()

	if !strings.Contains(msg, "non-JSON stdout") {
		t.Errorf("missing 'non-JSON stdout' label in: %q", msg)
	}
	if !strings.Contains(msg, rawLine) {
		t.Errorf("missing raw line in: %q", msg)
	}
}

// TestStderrRingCapacity verifies that the ring buffer evicts old lines once
// it reaches capacity, keeping exactly the most-recent N entries.
func TestStderrRingCapacity(t *testing.T) {
	const cap = 5
	ring := newStderrRing(cap)
	for i := 0; i < 10; i++ {
		ring.add(fmt.Sprintf("line %d", i))
	}
	tail := ring.tail()
	lines := strings.Split(tail, "\n")
	if len(lines) != cap {
		t.Fatalf("expected %d lines after overflow, got %d: %q", cap, len(lines), tail)
	}
	// The last line added must be line 9.
	if lines[len(lines)-1] != "line 9" {
		t.Errorf("last line should be 'line 9', got %q", lines[len(lines)-1])
	}
	// The first retained line must be line 5 (0-4 were evicted).
	if lines[0] != "line 5" {
		t.Errorf("first retained line should be 'line 5', got %q", lines[0])
	}
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
