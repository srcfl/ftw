// ftw-relay — HTTPS request-response tunnel for relay.fortytwowatts.com.
//
// See docs/goals/relay-as-tunnel.md for the design and docs/relay-deploy.md
// for operator setup (Cloudflare Origin Cert + systemd).
package main

import (
	"context"
	"errors"
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
	homeAllowTOFU := flag.Bool("home-allow-tofu", false, "allow the home host to run WITHOUT -home-pubkey (trust-on-first-use); insecure across relay restarts — testing only")
	flag.Parse()

	if *version {
		fmt.Println(Version)
		return
	}

	// Fail closed: the internet-exposed home route must never run on
	// trust-on-first-use (see requireHomePin).
	if err := requireHomePin(*homeHost, *homeSite, *homePubKey, *homeAllowTOFU); err != nil {
		slog.Error("ftw-relay: " + err.Error())
		os.Exit(1)
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
				// Evict self-registered sites whose Pi stopped re-registering, so
				// the owner registry self-heals against a /me/register flood. The
				// home/pinned site is exempt; a live Pi re-registers every ~60s.
				if n := r.Owners.GC(30 * time.Minute); n > 0 {
					slog.Info("ftw-relay: owner GC", "removed", n)
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

// requireHomePin enforces that the internet-exposed home route never runs on
// trust-on-first-use: when a home host/site is configured, -home-pubkey must pin
// the key (so a racer can't claim home.* after a relay restart drops the
// in-memory pin) unless TOFU was explicitly allowed. Extracted from main so the
// fail-closed rule is unit-testable.
func requireHomePin(homeHost, homeSite, homePubKey string, allowTOFU bool) error {
	if (homeHost != "" || homeSite != "") && homePubKey == "" && !allowTOFU {
		return errors.New("-home-host/-home-site requires -home-pubkey to pin the home site key — refusing to run the public home route in trust-on-first-use mode. Pass the Pi's public key (it logs it at startup) via -home-pubkey, or -home-allow-tofu to override for testing")
	}
	return nil
}

// safePrefix returns a short, log-safe prefix of a public key (never a secret,
// but no need to spill the whole thing into logs).
func safePrefix(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "…"
}
