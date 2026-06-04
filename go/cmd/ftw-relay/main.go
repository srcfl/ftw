// ftw-relay — HTTPS request-response tunnel for relay.fortytwowatts.com.
//
// See docs/goals/relay-as-tunnel.md for the design and docs/relay-deploy.md
// for operator setup (Cloudflare Origin Cert + systemd).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

var Version = "dev"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	addr := flag.String("addr", ":7378", "listen address")
	cert := flag.String("cert", "", "TLS cert path (HTTPS mode if set)")
	key := flag.String("key", "", "TLS key path (HTTPS mode if set)")
	pollTimeout := flag.Duration("poll-timeout", 25*time.Second, "long-poll deadline per /tunnel/<host>/next call")
	homeHost := flag.String("home-host", "", "bare host that maps to a single owner Pi (e.g. home.fortytwowatts.com); requires -home-site")
	homeSite := flag.String("home-site", "", "site_id the -home-host forwards to (e.g. site:Home)")
	homePubKey := flag.String("home-pubkey", "", "operator-provisioned ES256 public key (hex X||Y) the -home-site must register with; pins the home mapping across relay restarts so it is never first-come TOFU")
	flag.Parse()

	if *version {
		fmt.Println(Version)
		return
	}

	owners := NewOwnerRegistry()
	// Pre-pin the home site's key so the internet-exposed home route is
	// authoritative from boot and a racing attacker can never TOFU-claim it
	// after a relay restart. Without this flag the home site still falls back
	// to trust-on-first-use, but operators of a public home host should set it.
	if *homeSite != "" && *homePubKey != "" {
		owners.Pin(*homeSite, *homePubKey)
		slog.Info("ftw-relay: pinned home-site key", "site", *homeSite, "pubkey_prefix", safePrefix(*homePubKey))
	}

	r := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      owners,
		PollTimeout: *pollTimeout,
		HomeHost:    *homeHost,
		HomeSite:    *homeSite,
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           r.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// Janitor: periodically evict expired/revoked pair tokens so the in-memory
	// registry doesn't grow unbounded between relay restarts.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n := r.Tokens.GC(); n > 0 {
					slog.Info("ftw-relay: token GC", "removed", n)
				}
			}
		}
	}()

	mode := "HTTP"
	var err error
	if *cert != "" && *key != "" {
		mode = "HTTPS"
		slog.Info("ftw-relay starting", "mode", mode, "addr", *addr, "version", Version)
		err = srv.ListenAndServeTLS(*cert, *key)
	} else {
		slog.Info("ftw-relay starting", "mode", mode, "addr", *addr, "version", Version)
		err = srv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		slog.Error("ftw-relay server", "mode", mode, "err", err)
		os.Exit(1)
	}
	slog.Info("ftw-relay shut down cleanly", "mode", mode, "addr", *addr)
}

// safePrefix returns a short, log-safe prefix of a public key (never a secret,
// but no need to spill the whole thing into logs).
func safePrefix(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "…"
}
