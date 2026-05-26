// ftw-pair-relay is the Sourceful-operated relay server for the ftw-pair
// transport. It listens for TCP connections, reads a token from each, and
// splices matching pairs of connections together.
//
// This binary is deployed standalone (not inside the main container image).
// Operators who want to self-host the relay can download it from GitHub Releases.
//
// Usage:
//
//	ftw-pair-relay [flags]
//
// Flags:
//
//	-addr string    listen address (default ":7777")
//	-tls-cert file  path to TLS certificate (PEM); enables TLS when set
//	-tls-key file   path to TLS private key (PEM); required when -tls-cert is set
//
// Environment:
//
//	RELAY_ADDR  same as -addr (flag takes precedence)
//
// The relay does NOT decrypt traffic. All application data is AEAD-encrypted
// end-to-end between the two peers; the relay only sees ciphertext.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
)

var Version = "dev"

func main() {
	addr := flag.String("addr", envOr("RELAY_ADDR", ":7777"), "listen address")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := flag.String("tls-key", "", "TLS private key file (PEM)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *version {
		fmt.Printf("ftw-pair-relay %s\n", Version)
		os.Exit(0)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	var ln net.Listener
	var err error

	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			slog.Error("both -tls-cert and -tls-key must be set for TLS mode")
			os.Exit(1)
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			slog.Error("load TLS keypair", "err", err)
			os.Exit(1)
		}
		cfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
		}
		ln, err = tls.Listen("tcp", *addr, cfg)
		if err != nil {
			slog.Error("TLS listen", "addr", *addr, "err", err)
			os.Exit(1)
		}
		slog.Info("ftw-pair-relay listening (TLS)", "addr", *addr, "version", Version)
	} else {
		ln, err = net.Listen("tcp", *addr)
		if err != nil {
			slog.Error("listen", "addr", *addr, "err", err)
			os.Exit(1)
		}
		slog.Info("ftw-pair-relay listening", "addr", *addr, "version", Version)
	}
	defer ln.Close()

	relay := NewRelay()
	relay.StartReaper()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Accept loop in a goroutine so we can select on ctx.Done for graceful shutdown.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return // listener closed by shutdown
				default:
					slog.Error("accept", "err", err)
					continue
				}
			}
			go relay.Handle(conn)
		}
	}()

	<-ctx.Done()
	slog.Info("ftw-pair-relay shutting down")
	ln.Close()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
