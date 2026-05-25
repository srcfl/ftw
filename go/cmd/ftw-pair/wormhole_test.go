package main

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

func TestWormholeForwardEndToEnd(t *testing.T) {
	if os.Getenv("WORMHOLE_TEST") == "" {
		t.Skip("set WORMHOLE_TEST=1 to run against real rendezvous (needs internet)")
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			c.Write([]byte("PONG\n"))
			c.Close()
		}
	}()
	defer ln.Close()

	host, hostErr := StartWormholeHost(context.Background(), ln.Addr().String())
	if hostErr != nil {
		t.Fatalf("host: %v", hostErr)
	}
	defer host.Close()

	t.Logf("wormhole code: %s", host.Code)

	connClient, err := ConnectWormholeClient(context.Background(), host.Code)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer connClient.Close()

	c, _ := net.DialTimeout("tcp", connClient.LocalAddr, 5*time.Second)
	defer c.Close()
	buf := make([]byte, 8)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := c.Read(buf)
	if string(buf[:n]) != "PONG\n" {
		t.Fatalf("unexpected reply: %q", buf[:n])
	}
}
