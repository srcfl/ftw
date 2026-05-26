package main

import (
	"io"
	"net"
	"testing"
	"time"
)

// startTestRelay starts an in-process relay for testing and returns its address.
func startTestRelay(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	relay := NewRelay()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go relay.Handle(conn)
		}
	}()

	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// connectRaw dials the relay and sends the handshake with the given token.
// Returns the raw conn (handshake bytes consumed, first ack pending read).
func connectRaw(t *testing.T, relayAddr, token string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", relayAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	tok := []byte(token)
	hdr := []byte{relayProtoVersion, byte(len(tok))}
	hdr = append(hdr, tok...)
	if _, err := conn.Write(hdr); err != nil {
		t.Fatalf("send handshake: %v", err)
	}
	return conn
}

// readAck reads a single ack byte from conn with a short deadline.
func readAck(t *testing.T, conn net.Conn) byte {
	t.Helper()
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	conn.SetDeadline(time.Time{}) //nolint:errcheck
	return b[0]
}

// TestRelayTokenMatch verifies that two connections with the same token are
// matched and can exchange bytes through the relay.
func TestRelayTokenMatch(t *testing.T) {
	addr := startTestRelay(t)
	const token = "test-token-abc"

	// First peer connects.
	peer1 := connectRaw(t, addr, token)
	defer peer1.Close()

	ack1 := readAck(t, peer1)
	if ack1 != 0x01 {
		t.Fatalf("peer1: expected ack 0x01 (waiting), got 0x%02x", ack1)
	}

	// Second peer connects.
	peer2 := connectRaw(t, addr, token)
	defer peer2.Close()

	ack2 := readAck(t, peer2)
	if ack2 != 0x00 {
		t.Fatalf("peer2: expected ack 0x00 (matched), got 0x%02x", ack2)
	}

	// Read the matched ack for peer1.
	ack1b := readAck(t, peer1)
	if ack1b != 0x00 {
		t.Fatalf("peer1: expected second ack 0x00 (matched), got 0x%02x", ack1b)
	}

	// Now bytes should flow between peer1 and peer2.
	sent := []byte("hello relay")
	if _, err := peer1.Write(sent); err != nil {
		t.Fatalf("peer1 write: %v", err)
	}

	buf := make([]byte, len(sent))
	peer2.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	if _, err := io.ReadFull(peer2, buf); err != nil {
		t.Fatalf("peer2 read: %v", err)
	}
	if string(buf) != string(sent) {
		t.Errorf("got %q, want %q", buf, sent)
	}
}

// TestRelayDoublePopPrevented verifies that a third connection with the same
// token as an already-matched pair is treated as a new first-peer (the match
// table is cleared after the first pair is matched).
func TestRelayDoublePopPrevented(t *testing.T) {
	addr := startTestRelay(t)
	const token = "pop-test-token"

	peer1 := connectRaw(t, addr, token)
	defer peer1.Close()
	if ack := readAck(t, peer1); ack != 0x01 {
		t.Fatalf("peer1: expected 0x01, got 0x%02x", ack)
	}

	peer2 := connectRaw(t, addr, token)
	defer peer2.Close()
	if ack := readAck(t, peer2); ack != 0x00 {
		t.Fatalf("peer2: expected 0x00, got 0x%02x", ack)
	}
	if ack := readAck(t, peer1); ack != 0x00 {
		t.Fatalf("peer1 second ack: expected 0x00, got 0x%02x", ack)
	}

	// Third connection with the same token: should be treated as a new first peer
	// (the previous pair was removed from pending).
	peer3 := connectRaw(t, addr, token)
	defer peer3.Close()
	ack3 := readAck(t, peer3)
	if ack3 != 0x01 {
		t.Fatalf("peer3: expected 0x01 (new first peer), got 0x%02x", ack3)
	}
}

// TestRelayRateLimit verifies that excessive connections from a single IP are
// refused with a 0x02 error ack. We need > rateLimitMax connections.
func TestRelayRateLimit(t *testing.T) {
	addr := startTestRelay(t)
	// rateLimitMax = 10; send 15 connections from the same IP.
	const total = 15
	rejected := 0
	conns := make([]net.Conn, 0, total)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	for i := 0; i < total; i++ {
		c := connectRaw(t, addr, "rate-limit-token-"+string(rune('a'+i)))
		conns = append(conns, c)
		ack := readAck(t, c)
		if ack == 0x02 {
			rejected++
		}
	}

	if rejected == 0 {
		t.Errorf("expected at least one connection to be rejected (rate-limited), got none in %d connections", total)
	}
}

// TestRelayIdleCleanup verifies that unmatched connections don't leak.
// We test only that the relay goroutine handles a dropped connection cleanly
// (the full 60 s idle timeout is not tested to keep CI fast).
func TestRelayIdleCleanup(t *testing.T) {
	addr := startTestRelay(t)

	peer1 := connectRaw(t, addr, "idle-token")
	if ack := readAck(t, peer1); ack != 0x01 {
		t.Fatalf("expected 0x01, got 0x%02x", ack)
	}

	// Close the first peer without a second peer arriving.
	peer1.Close()

	// Give the relay a moment to detect the close.
	time.Sleep(100 * time.Millisecond)

	// A new first peer with the same token should be accepted (old entry cleaned up).
	// Note: the relay reaps on idle timeout (60 s), but the conn is also cleaned when
	// ack write fails — closing peer1 should trigger that path.
	// We just verify the relay doesn't return an error for a new attempt.
	peer2 := connectRaw(t, addr, "idle-token")
	defer peer2.Close()
	ack := readAck(t, peer2)
	// Either 0x01 (new first peer, old entry was cleaned) or 0x00 (matched with old
	// entry somehow — shouldn't happen). Both are acceptable; what we're checking is
	// that the relay doesn't hang or return 0x02.
	if ack == 0x02 {
		t.Errorf("relay returned error ack for new connection after first peer dropped")
	}
}

// TestRelayBadVersion verifies that connections with an unknown protocol version
// are rejected with a 0x02 error.
func TestRelayBadVersion(t *testing.T) {
	addr := startTestRelay(t)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a bad version byte.
	conn.Write([]byte{0xFF, 5, 'h', 'e', 'l', 'l', 'o'}) //nolint:errcheck

	conn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack[0] != 0x02 {
		t.Errorf("expected 0x02 (error) for bad version, got 0x%02x", ack[0])
	}
}
