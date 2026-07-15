// ftw-relay — HTTPS request-response tunnel for relay.ftw.sourceful.energy.
//
// See docs/archive/agent-artifacts/goals/relay-as-tunnel.md for the design and
// docs/relay-deploy.md for operator setup (Cloudflare Origin Cert + systemd).
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

	"github.com/srcfl/ftw/go/internal/tunnel"
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
	homeWeb := flag.String("home-web", "", "path to the relay bootstrap web files. In multi-tenant mode this is a small allowlisted loader/login bundle; dashboard app assets are fetched from the chosen Pi.")
	requireDeviceKey := flag.Bool("require-device-key", false, "ENFORCE the device-key signaling gate (C2): an offer must carry a verified device-key proof or the Pi is never contacted. Off (default) keeps the pre-C2 behaviour so the relay can serve the shell + identity (slices 1+2) while a home Pi that doesn't yet publish device-keys keeps working. Turn on once device-keys are enrolled.")
	homeAllowTOFU := flag.Bool("home-allow-tofu", false, "allow the home host to run WITHOUT -home-pubkey (trust-on-first-use); insecure across relay restarts — testing only")
	trustCFIP := flag.Bool("trust-cf-ip", false, "behind Cloudflare: trust CF-Connecting-IP for the per-IP signaling throttle, but ONLY from validated Cloudflare edge peers (else the throttle keys on the shared CF edge IP). Also firewall the origin to Cloudflare's ranges.")
	iceStun := flag.String("ice-stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs published from /signal/ice; empty disables STUN")
	turnURL := flag.String("turn-url", "", "comma-separated TURN URLs published from /signal/ice (e.g. turn:relay.example:3478?transport=udp)")
	turnSecret := flag.String("turn-secret", os.Getenv("FTW_TURN_SECRET"), "coturn static-auth-secret used to mint short-lived TURN REST credentials; may also be set with FTW_TURN_SECRET")
	multiTenant := flag.Bool("multi-tenant", false, "public multi-tenant home route: home.* serves only a tiny relay-disk loader until the browser decrypts its directory; dashboard static GETs then forward to the selected Pi, while owner data stays P2P. Requires -home-web; -require-device-key remains an optional extra gate; -home-site/-home-pubkey become no-ops.")
	walletBlobDir := flag.String("wallet-blob-dir", "", "directory holding the per-wallet encrypted directory blobs (one <user_handle>.blob file each). Required under -multi-tenant; the one piece of durable relay state. The relay never decrypts the contents.")
	walletBlobMaxBytes := flag.Int("wallet-blob-max-bytes", 65536, "per-wallet ciphertext byte cap; a PUT over this is rejected 413 so a hostile client can't grow the blob store without bound")
	flag.Parse()

	if *version {
		fmt.Println(Version)
		return
	}

	// Fail closed: the internet-exposed SINGLE-TENANT home route must never run on
	// trust-on-first-use (see requireHomePin). Under -multi-tenant there is no
	// single -home-site to pin, so requireHomePin does NOT apply — requireMultiTenant
	// (below) is the boot rule instead.
	if !*multiTenant {
		if err := requireHomePin(*homeHost, *homeSite, *homePubKey, *homeWeb, *homeAllowTOFU); err != nil {
			slog.Error("ftw-relay: " + err.Error())
			os.Exit(1)
		}
	}

	// -multi-tenant requires the tiny relay bootstrap bundle (-home-web). The
	// dashboard app itself is fetched from the chosen Pi after the browser decrypts
	// its directory. -require-device-key stays available as an explicit hardening
	// flag, but is no longer forced by multi-tenant mode.
	if err := requireMultiTenant(*multiTenant, *requireDeviceKey, *homeWeb); err != nil {
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

	// Under multi-tenant, build the durable per-wallet encrypted-blob store. It is
	// loaded from -wallet-blob-dir at boot so blobs survive a relay restart. The
	// store caps the number of distinct wallets and the per-blob size; the janitor
	// GCs idle wallets below.
	var walletBlobs *WalletBlobStore
	var bootstrap *BootstrapStore
	if *multiTenant {
		if *walletBlobDir == "" {
			slog.Error("ftw-relay: -multi-tenant requires -wallet-blob-dir (the durable encrypted-blob store)")
			os.Exit(1)
		}
		// maxBlobs 0 -> the store's generous default. Wallet blobs are not GC'd
		// (see the janitor note), so the wallet-count cap is the sole bound.
		wb, err := NewWalletBlobStore(*walletBlobDir, *walletBlobMaxBytes, 0)
		if err != nil {
			slog.Error("ftw-relay: wallet blob store", "err", err)
			os.Exit(1)
		}
		walletBlobs = wb
		// Ephemeral first-enrollment store: 64 KiB per descriptor, 4096 concurrent
		// bootstraps. Each entry self-expires (bootstrapTTL) and the janitor GCs the
		// stragglers, so this is purely an in-memory onboarding scratchpad.
		bootstrap = NewBootstrapStore(65536, 4096)
		slog.Info("ftw-relay: multi-tenant mode", "wallet_blob_dir", *walletBlobDir, "max_bytes", *walletBlobMaxBytes)
	}

	r := &Relay{
		Queue:            tunnel.NewQueue(),
		Tokens:           NewTokenRegistry(),
		Owners:           owners,
		Polls:            NewPollSecrets(),
		Signals:          NewSignalMailbox(),
		Challenges:       NewSignalChallenges(),
		TrustCFIP:        *trustCFIP,
		PollTimeout:      *pollTimeout,
		HomeHost:         *homeHost,
		HomeSite:         *homeSite,
		HomeWeb:          *homeWeb,
		HomePubKey:       *homePubKey,
		RequireDeviceKey: *requireDeviceKey,
		ICEStunURLs:      parseURLList(*iceStun),
		TURNURLs:         parseURLList(*turnURL),
		TURNSecret:       *turnSecret,
		MultiTenant:      *multiTenant,
		WalletBlobs:      walletBlobs,
		Bootstrap:        bootstrap,
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
				if n := r.Polls.GC(30 * time.Minute); n > 0 {
					slog.Info("ftw-relay: poll-token GC", "removed", n)
				}
				// Evict idle signaling mailboxes so a flood of offers for random
				// site_ids can't grow relay memory; a live pair re-signals on demand.
				if n := r.Signals.GC(30 * time.Minute); n > 0 {
					slog.Info("ftw-relay: signal GC", "removed", n)
				}
				// Reap expired device-key challenge nonces (C2) so a flood of
				// GET /signal/<random>/challenge can't grow relay memory; each nonce
				// also self-expires after ~60s, so this just trims the map proactively.
				if r.Challenges != nil {
					if n := r.Challenges.GC(0); n > 0 {
						slog.Info("ftw-relay: signal-challenge GC", "removed", n)
					}
				}
				// Evict idle per-source-IP offer buckets (FIX-C) so a churn of source
				// IPs can't grow the limiter map without bound; an idle IP has
				// refilled to full anyway, so forgetting it only resets it fresh.
				if r.OfferLimit != nil {
					if n := r.OfferLimit.GC(offerBucketIdleTTL); n > 0 {
						slog.Info("ftw-relay: offer-limit GC", "removed", n)
					}
				}
				if r.ICELimit != nil {
					if n := r.ICELimit.GC(offerBucketIdleTTL); n > 0 {
						slog.Info("ftw-relay: ice-limit GC", "removed", n)
					}
				}
				// Reap expired first-enrollment bootstraps so an abandoned onboarding
				// (Pi published, browser never claimed) can't pin memory; each entry
				// also self-expires after bootstrapTTL, so this just trims proactively.
				if r.Bootstrap != nil {
					if n := r.Bootstrap.GC(); n > 0 {
						slog.Info("ftw-relay: bootstrap GC", "removed", n)
					}
				}
				// NB: wallet blobs are deliberately NOT time-GC'd. Unlike a site
				// registration (which re-registers every ~60s, so staleness means
				// "Pi gone"), a wallet directory is durable per-person and is only
				// re-written when the owner changes it — which can be weeks apart.
				// Evicting an idle blob would drop its TOFU-pinned write key, opening
				// a squat window where an attacker who knows the userHandle pins THEIR
				// key first and locks the owner out (Codex 2026-06-05, HIGH). The
				// blob store is bounded by its wallet-count cap instead; abandoned-blob
				// reclamation (a tombstone that keeps the pin) is a cutover concern.
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
func requireHomePin(homeHost, homeSite, homePubKey, homeWeb string, allowTOFU bool) error {
	if (homeHost != "" || homeSite != "") && !allowTOFU {
		if homePubKey == "" {
			return errors.New("-home-host/-home-site requires -home-pubkey to pin the home site key — refusing to run the public home route in trust-on-first-use mode. Pass the Pi's public key (it logs it at startup) via -home-pubkey, or -home-allow-tofu to override for testing")
		}
		if homeWeb == "" {
			return errors.New("-home-host/-home-site requires -home-web — the relay must serve the sign-in shell + /api/identity itself and NEVER forward an anonymous request to the Pi (forwarding would let an unauthenticated internet visitor reach the home network). Pass the path to the web/ bundle on the relay VM, or -home-allow-tofu to override for testing")
		}
	}
	return nil
}

// requireMultiTenant enforces the boot rules for the public multi-tenant home
// route. -multi-tenant REQUIRES -home-web: the tiny landing/login/loader bundle
// must be served from the relay's own disk so an anonymous GET never reaches a Pi.
// -require-device-key is optional: when off, the relay forwards signaling by
// site_id and the Pi authenticates with passkey over the E2E channel.
// -home-site/-home-pubkey are NOT required (and become no-ops) under
// multi-tenant — the destination comes from the browser's decrypted directory,
// not a relay pin. Extracted from main so the rule is unit-testable, like
// requireHomePin.
func requireMultiTenant(multiTenant, requireDeviceKey bool, homeWeb string) error {
	if !multiTenant {
		return nil
	}
	if homeWeb == "" {
		return errors.New("-multi-tenant requires -home-web — the relay must serve the public landing/login loader itself and NEVER forward an anonymous request to a Pi. Pass the path to the relay bootstrap bundle")
	}
	_ = requireDeviceKey
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
