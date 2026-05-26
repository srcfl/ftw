package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	wh "github.com/frahlg/forty-two-watts/go/internal/wormhole"
)

// startTestRelay starts a minimal in-process relay for the ftw-pair shim tests.
func startTestRelay(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}

	var mu sync.Mutex
	hosts := make(map[string][]net.Conn) // FIFO queue of host conns per token

	handleConn := func(conn net.Conn) {
		// v0x02 handshake: version(1) + role(1) + tokenLen(1) + token(N)
		header := make([]byte, 3)
		if _, err := io.ReadFull(conn, header); err != nil {
			conn.Close()
			return
		}
		if header[0] != 0x02 {
			conn.Write([]byte{0x02}) //nolint:errcheck
			conn.Close()
			return
		}
		role := header[1]
		tokenLen := int(header[2])
		tok := make([]byte, tokenLen)
		if _, err := io.ReadFull(conn, tok); err != nil {
			conn.Close()
			return
		}
		token := string(tok)

		switch role {
		case 0x00: // host
			mu.Lock()
			hosts[token] = append(hosts[token], conn)
			mu.Unlock()
			if _, err := conn.Write([]byte{0x01}); err != nil {
				conn.Close()
			}
		case 0x01: // client
			mu.Lock()
			q := hosts[token]
			if len(q) == 0 {
				mu.Unlock()
				conn.Write([]byte{0x04}) //nolint:errcheck
				conn.Close()
				return
			}
			host := q[0]
			hosts[token] = q[1:]
			mu.Unlock()
			conn.Write([]byte{0x00}) //nolint:errcheck
			host.Write([]byte{0x00}) //nolint:errcheck
			done := make(chan struct{}, 2)
			go func() { io.Copy(host, conn); host.Close(); done <- struct{}{} }() //nolint:errcheck
			go func() { io.Copy(conn, host); conn.Close(); done <- struct{}{} }() //nolint:errcheck
			<-done
			<-done
		default:
			conn.Write([]byte{0x02}) //nolint:errcheck
			conn.Close()
		}
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(c)
		}
	}()

	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// TestStartWormholeHostViaShim verifies that StartWormholeHost returns a Host
// with a non-empty 6-word Code, using an in-process relay.
func TestStartWormholeHostViaShim(t *testing.T) {
	relayAddr := startTestRelay(t)

	// Temporarily set the relay addr via env var.
	t.Setenv("FTW_PAIR_RELAY", relayAddr)
	// The flag override is nil here; wormhole.go reads *relayAddrFlag which will be nil
	// — set it to an empty string (the env var takes precedence).
	oldFlag := relayAddrFlag
	empty := ""
	relayAddrFlag = &empty
	defer func() { relayAddrFlag = oldFlag }()

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoLn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	host, err := StartWormholeHost(ctx, echoLn.Addr().String())
	if err != nil {
		t.Fatalf("StartWormholeHost: %v", err)
	}
	defer host.Close()

	if host.Code == "" {
		t.Fatal("host.Code is empty")
	}
	// Token should be 6 hyphen-separated words.
	parts := splitToken(host.Code)
	if len(parts) != wh.TokenWordCount() {
		t.Errorf("token has %d words, want 6: %q", len(parts), host.Code)
	}
}

// TestWormholeShimEndToEnd exercises the ftw-pair shim (StartWormholeHost +
// Connect) against an in-process relay to ensure the shim wires correctly.
func TestWormholeShimEndToEnd(t *testing.T) {
	relayAddr := startTestRelay(t)

	// Echo server: accepts one conn and writes PONG.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const testToken = "zoo-zebra-zero-zone-yacht-year"
	host, err := wh.StartHost(ctx, echoLn.Addr().String(),
		wh.WithRelayAddr(relayAddr),
		wh.WithToken(testToken),
	)
	if err != nil {
		t.Fatalf("StartHost: %v", err)
	}
	defer host.Close()

	time.Sleep(30 * time.Millisecond)

	client, err := wh.Connect(ctx, testToken, wh.WithRelayAddr(relayAddr))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	time.Sleep(80 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", client.LocalAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("scan: %v", sc.Err())
	}
	if got := sc.Text(); got != "PONG" {
		t.Errorf("got %q, want %q", got, "PONG")
	}
}

// splitToken splits a token string on "-".
func splitToken(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}
