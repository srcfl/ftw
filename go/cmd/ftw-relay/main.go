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
	baseDomain := flag.String("base-domain", "", "apex for subdomain-per-session routing (e.g. fortytwowatts.com); empty disables Host routing")
	flag.Parse()

	if *version {
		fmt.Println(Version)
		return
	}

	r := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      NewOwnerRegistry(),
		PollTimeout: *pollTimeout,
		BaseDomain:  *baseDomain,
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

	mode := "HTTP"
	var err error
	if *cert != "" && *key != "" {
		mode = "HTTPS"
		slog.Info("ftw-relay starting", "mode", mode, "addr", *addr, "version", Version, "base_domain", *baseDomain)
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
