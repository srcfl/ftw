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

// TestRelaySpliceHalfCloseUnblocksOtherSide is the regression test for the
// host-pool starvation bug: when a one-shot client (curl) closes its side of
// the splice after receiving the response, the host-side socket would stay
// open forever — the host's HTTP keep-alive Read kept the other io.Copy
// blocked, the relay's WaitGroup never finished, and the host's worker
// couldn't re-queue. After 4 such requests, every new client got
// "no host ready".
//
// The fix: close BOTH conns as soon as EITHER io.Copy returns. This test
// verifies that the host's Read returns shortly after the client closes,
// even when the host is idle (no traffic from host→client).
func TestRelaySpliceHalfCloseUnblocksOtherSide(t *testing.T) {
	addr := startTestRelay(t)
	const token = "half-close-token"

	host := connectRaw(t, addr, token, roleHost)
	defer host.Close()
	if ack := readAck(t, host); ack != 0x01 {
		t.Fatalf("host: expected 0x01, got 0x%02x", ack)
	}

	client := connectRaw(t, addr, token, roleClient)
	if ack := readAck(t, client); ack != 0x00 {
		t.Fatalf("client: expected 0x00, got 0x%02x", ack)
	}
	if ack := readAck(t, host); ack != 0x00 {
		t.Fatalf("host matched-ack: expected 0x00, got 0x%02x", ack)
	}

	// Client sends one byte (simulates HTTP request).
	if _, err := client.Write([]byte{'x'}); err != nil {
		t.Fatalf("client write: %v", err)
	}
	buf := make([]byte, 1)
	host.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	if _, err := io.ReadFull(host, buf); err != nil {
		t.Fatalf("host read: %v", err)
	}
	host.SetDeadline(time.Time{}) //nolint:errcheck

	// Host is idle — does NOT reply. Simulates HTTP keep-alive after the
	// response has already been sent on a different short-lived conn.
	// Client closes (curl finishes).
	client.Close()

	// Host MUST see EOF (or a closed-conn error) within a short window —
	// not after the 60 s idle timeout. Before the fix, this read would
	// block until the test timeout.
	host.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	n, err := host.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("host read returned %d bytes %q without error — splice did not propagate close", n, buf[:n])
	}
	if err == nil {
		t.Fatal("host read returned nil error — expected EOF after client close")
	}
	// Either io.EOF or a "use of closed network connection" — both are correct outcomes.

	// And a fresh host can re-queue immediately, proving the splice released
	// the queue slot.
	host2 := connectRaw(t, addr, token, roleHost)
	defer host2.Close()
	if ack := readAck(t, host2); ack != 0x01 {
		t.Fatalf("host2 (re-queue): expected 0x01, got 0x%02x", ack)
	}
}

// TestRelaySpliceHalfCloseHostSide is the mirror of HalfCloseUnblocksOtherSide:
// host closes first, client must see EOF promptly. This covers the case where
// the host's worker decides to tear down (TTL expiry, abort) while the client
// is mid-request.
func TestRelaySpliceHalfCloseHostSide(t *testing.T) {
	addr := startTestRelay(t)
	const token = "half-close-host-token"

	host := connectRaw(t, addr, token, roleHost)
	if ack := readAck(t, host); ack != 0x01 {
		t.Fatalf("host: expected 0x01, got 0x%02x", ack)
	}

	client := connectRaw(t, addr, token, roleClient)
	defer client.Close()
	if ack := readAck(t, client); ack != 0x00 {
		t.Fatalf("client: expected 0x00, got 0x%02x", ack)
	}
	if ack := readAck(t, host); ack != 0x00 {
		t.Fatalf("host matched-ack: expected 0x00, got 0x%02x", ack)
	}

	// Host closes while client is idle. Client must see EOF promptly.
	host.Close()

	buf := make([]byte, 1)
	client.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	_, err := client.Read(buf)
	if err == nil {
		t.Fatal("client read returned nil error — expected EOF after host close")
	}
}

// TestRelaySpliceSequentialRequests is the end-to-end regression for the bug
// that caused "no host ready" after 4 requests: simulate the host's worker
// pool re-queueing N times and verify each new client gets matched and the
// pipe terminates promptly.
func TestRelaySpliceSequentialRequests(t *testing.T) {
	addr := startTestRelay(t)
	const token = "sequential-token"
	const N = 10

	for i := 0; i < N; i++ {
		host := connectRaw(t, addr, token, roleHost)
		if ack := readAck(t, host); ack != 0x01 {
			host.Close()
			t.Fatalf("iter %d host: expected 0x01, got 0x%02x", i, ack)
		}

		client := connectRaw(t, addr, token, roleClient)
		if ack := readAck(t, client); ack != 0x00 {
			host.Close()
			client.Close()
			t.Fatalf("iter %d client: expected 0x00, got 0x%02x", i, ack)
		}
		if ack := readAck(t, host); ack != 0x00 {
			host.Close()
			client.Close()
			t.Fatalf("iter %d host matched-ack: expected 0x00, got 0x%02x", i, ack)
		}

		// One byte each way to confirm the splice is live.
		if _, err := client.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("iter %d client write: %v", i, err)
		}
		buf := make([]byte, 1)
		host.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
		if _, err := io.ReadFull(host, buf); err != nil {
			t.Fatalf("iter %d host read: %v", i, err)
		}
		if buf[0] != byte(i) {
			t.Errorf("iter %d: got 0x%02x, want 0x%02x", i, buf[0], byte(i))
		}

		// Client closes (curl finishes). Host MUST see EOF and the relay
		// MUST release this slot so the next iteration's host can queue.
		client.Close()
		host.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
		if _, err := host.Read(buf); err == nil {
			t.Fatalf("iter %d: host read returned nil error after client close — splice did not propagate", i)
		}
		host.Close()
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
