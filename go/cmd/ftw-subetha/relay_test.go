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

// connectRaw dials the relay and sends the v0x02 handshake with the given
// role + token. Returns the raw conn (handshake bytes consumed, first ack
// pending read).
func connectRaw(t *testing.T, relayAddr, token string, role byte) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", relayAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	tok := []byte(token)
	hdr := []byte{relayProtoVersion, role, byte(len(tok))}
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

// TestRelayTokenMatch verifies that a host and a client with the same token
// are matched and can exchange bytes through the relay.
func TestRelayTokenMatch(t *testing.T) {
	addr := startTestRelay(t)
	const token = "test-token-abc"

	// Host connects first.
	host := connectRaw(t, addr, token, roleHost)
	defer host.Close()

	ack1 := readAck(t, host)
	if ack1 != 0x01 {
		t.Fatalf("host: expected ack 0x01 (waiting), got 0x%02x", ack1)
	}

	// Client connects.
	client := connectRaw(t, addr, token, roleClient)
	defer client.Close()

	ack2 := readAck(t, client)
	if ack2 != 0x00 {
		t.Fatalf("client: expected ack 0x00 (matched), got 0x%02x", ack2)
	}

	// Read the matched ack on the host side.
	ack1b := readAck(t, host)
	if ack1b != 0x00 {
		t.Fatalf("host: expected second ack 0x00 (matched), got 0x%02x", ack1b)
	}

	// Now bytes should flow between host and client.
	sent := []byte("hello relay")
	if _, err := host.Write(sent); err != nil {
		t.Fatalf("host write: %v", err)
	}

	buf := make([]byte, len(sent))
	client.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(buf) != string(sent) {
		t.Errorf("got %q, want %q", buf, sent)
	}
}

// TestRelayClientWithoutHostRejected verifies that a client arriving with no
// queued host is rejected with 0x04 ("no host ready").
func TestRelayClientWithoutHostRejected(t *testing.T) {
	addr := startTestRelay(t)
	c := connectRaw(t, addr, "orphan-token", roleClient)
	defer c.Close()
	if ack := readAck(t, c); ack != 0x04 {
		t.Fatalf("expected 0x04 (no host ready), got 0x%02x", ack)
	}
}

// TestRelayHostPoolFifo verifies that multiple hosts queued for the same token
// are popped in FIFO order by successive clients.
func TestRelayHostPoolFifo(t *testing.T) {
	addr := startTestRelay(t)
	const token = "fifo-token"

	// Queue three hosts.
	hosts := make([]net.Conn, 3)
	for i := range hosts {
		hosts[i] = connectRaw(t, addr, token, roleHost)
		defer hosts[i].Close()
		if ack := readAck(t, hosts[i]); ack != 0x01 {
			t.Fatalf("host %d: expected 0x01, got 0x%02x", i, ack)
		}
	}

	// Each client matches the next host in FIFO order.
	for i := 0; i < 3; i++ {
		c := connectRaw(t, addr, token, roleClient)
		defer c.Close()
		if ack := readAck(t, c); ack != 0x00 {
			t.Fatalf("client %d: expected 0x00, got 0x%02x", i, ack)
		}
		// The matched host receives 0x00 too.
		if ack := readAck(t, hosts[i]); ack != 0x00 {
			t.Fatalf("host %d matched-ack: expected 0x00, got 0x%02x", i, ack)
		}
		// Verify it's the right host by sending a token byte.
		sent := []byte{byte('A' + i)}
		hosts[i].Write(sent) //nolint:errcheck
		buf := make([]byte, 1)
		c.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("client %d read: %v", i, err)
		}
		if buf[0] != sent[0] {
			t.Errorf("client %d: got 0x%02x, want 0x%02x — FIFO order broken", i, buf[0], sent[0])
		}
	}

	// Fourth client should get rejected (queue empty).
	c4 := connectRaw(t, addr, token, roleClient)
	defer c4.Close()
	if ack := readAck(t, c4); ack != 0x04 {
		t.Errorf("client 4: expected 0x04 (no host ready), got 0x%02x", ack)
	}
}

// TestRelayHostReQueueAfterMatch verifies that after a host/client pair
// matches, a NEW host with the same token can re-queue (so the same session
// can serve many sequential connections).
func TestRelayHostReQueueAfterMatch(t *testing.T) {
	addr := startTestRelay(t)
	const token = "requeue-token"

	host1 := connectRaw(t, addr, token, roleHost)
	defer host1.Close()
	if ack := readAck(t, host1); ack != 0x01 {
		t.Fatalf("host1: expected 0x01, got 0x%02x", ack)
	}

	c1 := connectRaw(t, addr, token, roleClient)
	defer c1.Close()
	if ack := readAck(t, c1); ack != 0x00 {
		t.Fatalf("c1: expected 0x00, got 0x%02x", ack)
	}
	if ack := readAck(t, host1); ack != 0x00 {
		t.Fatalf("host1 matched-ack: expected 0x00, got 0x%02x", ack)
	}

	// New host re-queues with the same token; new client matches.
	host2 := connectRaw(t, addr, token, roleHost)
	defer host2.Close()
	if ack := readAck(t, host2); ack != 0x01 {
		t.Fatalf("host2: expected 0x01 (re-queue), got 0x%02x", ack)
	}
}

// TestRelayRateLimit verifies that excessive connections from a single IP are
// refused with a 0x02 error ack. We need > rateLimitMax connections.
func TestRelayRateLimit(t *testing.T) {
	addr := startTestRelay(t)
	// rateLimitMax is the new-conns-per-IP-per-minute cap. Send well past it.
	const total = rateLimitMax + 10
	rejected := 0
	conns := make([]net.Conn, 0, total)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	for i := 0; i < total; i++ {
		c := connectRaw(t, addr, "rate-limit-token-"+string(rune('a'+i)), roleHost)
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

	host1 := connectRaw(t, addr, "idle-token", roleHost)
	if ack := readAck(t, host1); ack != 0x01 {
		t.Fatalf("expected 0x01, got 0x%02x", ack)
	}

	// Close the host without a client arriving.
	host1.Close()
	time.Sleep(100 * time.Millisecond)

	// A new host with the same token should be accepted (idle entry will be reaped or simply queued anew).
	host2 := connectRaw(t, addr, "idle-token", roleHost)
	defer host2.Close()
	ack := readAck(t, host2)
	if ack == 0x02 {
		t.Errorf("relay returned error ack for new host after dropped predecessor")
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
