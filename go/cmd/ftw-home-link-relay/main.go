package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/srcfl/ftw/go/internal/homelinkrelay"
)

const maxInviteFileBytes = 1024 * 1024

func main() {
	if err := run(); err != nil {
		slog.Error("Home Link relay stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listenAddress  = flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
		invitePath     = flag.String("invites", "", "path to the public gateway invite file")
		tlsCertificate = flag.String("tls-cert", "", "path to a PEM TLS certificate")
		tlsKey         = flag.String("tls-key", "", "path to a PEM TLS private key")
		behindTLSProxy = flag.Bool("behind-tls-proxy", false, "allow clear HTTP behind a trusted TLS proxy")
	)
	flag.Parse()
	if *invitePath == "" {
		return errors.New("-invites is required")
	}
	inviteData, err := readBoundedFile(*invitePath, maxInviteFileBytes)
	if err != nil {
		return fmt.Errorf("read relay invites: %w", err)
	}
	invites, err := homelinkrelay.ParseStaticInvites(inviteData)
	if err != nil {
		return err
	}
	relay, err := homelinkrelay.New(homelinkrelay.Options{Invites: invites})
	if err != nil {
		return err
	}
	if err := validateListen(*listenAddress, *tlsCertificate, *tlsKey, *behindTLSProxy); err != nil {
		return err
	}

	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           relay.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       65 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	errs := make(chan error, 1)
	go func() {
		switch {
		case *tlsCertificate != "":
			errs <- server.ListenAndServeTLS(*tlsCertificate, *tlsKey)
		default:
			errs <- server.ListenAndServe()
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func validateListen(address, certificate, key string, behindTLSProxy bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if (certificate == "") != (key == "") {
		return errors.New("-tls-cert and -tls-key must be set together")
	}
	if certificate != "" {
		return nil
	}
	ip := net.ParseIP(host)
	if !behindTLSProxy && (ip == nil || !ip.IsLoopback()) {
		return errors.New("clear HTTP may bind only to loopback unless -behind-tls-proxy is set")
	}
	return nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("file size or type is invalid")
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, err
	}
	extra := make([]byte, 1)
	count, err := file.Read(extra)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if count != 0 {
		return nil, errors.New("file changed while it was read")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() {
		return nil, errors.New("file changed while it was read")
	}
	return data, nil
}
