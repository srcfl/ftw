package ocpp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ws"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// requestFrom builds the request the library hands checkClient: basic auth in
// the header, and the local address net/http puts on every connection context.
func requestFrom(t *testing.T, user, pass, arrivedOn string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/garage", nil)
	if user != "" || pass != "" {
		r.SetBasicAuth(user, pass)
	}
	if arrivedOn != "" {
		addr, err := net.ResolveTCPAddr("tcp", arrivedOn)
		if err != nil {
			t.Fatalf("resolve %q: %v", arrivedOn, err)
		}
		ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, addr)
		r = r.WithContext(ctx)
	}
	return r
}

func requestWithCert(t *testing.T, user, pass string, cert *x509.Certificate) *http.Request {
	t.Helper()
	r := requestFrom(t, user, pass, "")
	if cert != nil {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	}
	return r
}

func mtlsAuthorizer(secrets map[string]string) *authorizer {
	return newAuthorizer(&Config{
		Username:       "ftw",
		Password:       "shared-secret",
		ChargerSecrets: secrets,
		TLS:            &TLSConfig{CertFile: "c.pem", KeyFile: "k.pem", ClientCAFile: "ca.pem"},
	})
}

// A charger with a credential of its own cannot be impersonated by something
// holding only the shared password. That is the whole point of the feature:
// identity is client-chosen, so authenticating has never proved which device
// is on the other end.
func TestPerChargerCredentialBlocksImpersonation(t *testing.T) {
	a := newAuthorizer(&Config{
		Username:       "ftw",
		Password:       "shared-secret",
		ChargerSecrets: map[string]string{"garage": "garage-only-secret"},
	})

	t.Run("its own credential is accepted", func(t *testing.T) {
		if !a.basicAuth("garage", "garage-only-secret") {
			t.Error("basic auth rejected the charger's own credential")
		}
		if !a.checkClient("garage", requestFrom(t, "garage", "garage-only-secret", "")) {
			t.Error("checkClient rejected the charger's own credential")
		}
	})

	t.Run("the shared credential no longer buys its name", func(t *testing.T) {
		// Basic auth passes — the shared secret is real — and the identity
		// binding is what refuses.
		if !a.basicAuth("ftw", "shared-secret") {
			t.Fatal("the shared credential should still authenticate")
		}
		if a.checkClient("garage", requestFrom(t, "ftw", "shared-secret", "")) {
			t.Error("the shared password claimed a charger that has its own credential")
		}
	})

	t.Run("its own password under another username is refused", func(t *testing.T) {
		if a.checkClient("garage", requestFrom(t, "ftw", "garage-only-secret", "")) {
			t.Error("accepted the charger's password presented under another identity")
		}
	})

	t.Run("a wrong password for its own name is refused", func(t *testing.T) {
		if a.basicAuth("garage", "shared-secret") {
			t.Error("a charger with its own credential fell back to the shared one")
		}
		if a.checkClient("garage", requestFrom(t, "garage", "nope", "")) {
			t.Error("accepted a wrong password")
		}
	})

	t.Run("chargers without one keep using the shared credential", func(t *testing.T) {
		if !a.basicAuth("ftw", "shared-secret") {
			t.Error("shared credential rejected")
		}
		if !a.checkClient("carport", requestFrom(t, "ftw", "shared-secret", "")) {
			t.Error("a charger with no credential of its own should use the shared one")
		}
	})
}

// Bind is enforced at the handshake because the socket cannot be pinned: the
// library builds its listen address from the port alone.
func TestBindAddressIsEnforcedAtTheHandshake(t *testing.T) {
	tests := []struct {
		name      string
		bind      string
		arrivedOn string
		want      bool
	}{
		{name: "same address", bind: "192.168.1.10", arrivedOn: "192.168.1.10:8887", want: true},
		{name: "another interface", bind: "192.168.1.10", arrivedOn: "10.8.0.1:8887", want: false},
		{name: "loopback when bound to the LAN", bind: "192.168.1.10", arrivedOn: "127.0.0.1:8887", want: false},
		{name: "unspecified accepts anything", bind: "0.0.0.0", arrivedOn: "10.8.0.1:8887", want: true},
		{name: "empty accepts anything", bind: "", arrivedOn: "10.8.0.1:8887", want: true},
		// A dual-stack listener reports an IPv4 connection as v4-mapped v6.
		{name: "v4-mapped v6 matches its v4", bind: "192.168.1.10", arrivedOn: "[::ffff:192.168.1.10]:8887", want: true},
		// Not knowing where it landed must not lock every charger out.
		{name: "no local address is allowed", bind: "192.168.1.10", arrivedOn: "", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuthorizer(&Config{Bind: tc.bind})
			got := a.checkClient("garage", requestFrom(t, "", "", tc.arrivedOn))
			if got != tc.want {
				t.Errorf("checkClient on %s with bind %q: got %v, want %v",
					tc.arrivedOn, tc.bind, got, tc.want)
			}
		})
	}
}

// With no credentials configured the basic-auth handler must stay unregistered
// — the library answers 401 to a charger that sends none whenever a handler
// exists, so registering it would lock out every charger instead of admitting
// them all.
func TestNoCredentialsMeansNoBasicAuthHandler(t *testing.T) {
	if newAuthorizer(&Config{}).requiresCredential() {
		t.Error("an OCPP section with no credentials should not demand one")
	}
	if !newAuthorizer(&Config{Username: "ftw", Password: "x"}).requiresCredential() {
		t.Error("a shared credential should be demanded")
	}
	if !newAuthorizer(&Config{ChargerSecrets: map[string]string{"garage": "x"}}).requiresCredential() {
		t.Error("a per-charger credential should be demanded")
	}
}

// A client CA is not enough: the certificate must name the identity in the
// URL. Otherwise any cert from that CA can claim any adopted charger that
// has no password of its own.
func TestClientCertBindsIdentity(t *testing.T) {
	a := mtlsAuthorizer(nil)
	if !a.requireClientCert {
		t.Fatal("ClientCAFile should require a client certificate")
	}
	if newAuthorizer(&Config{TLS: &TLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}}).requireClientCert {
		t.Error("TLS without a client CA must not demand a client certificate")
	}

	cn := &x509.Certificate{Subject: pkix.Name{CommonName: "garage"}}
	sanOnly := &x509.Certificate{DNSNames: []string{"garage"}}
	other := &x509.Certificate{Subject: pkix.Name{CommonName: "carport"}, DNSNames: []string{"carport"}}

	t.Run("CN matching the URL is accepted", func(t *testing.T) {
		if !a.checkClient("garage", requestWithCert(t, "ftw", "shared-secret", cn)) {
			t.Error("refused a certificate whose CN is the claimed identity")
		}
	})
	t.Run("DNS SAN matching the URL is accepted", func(t *testing.T) {
		if !a.checkClient("garage", requestWithCert(t, "ftw", "shared-secret", sanOnly)) {
			t.Error("refused a certificate whose DNS SAN is the claimed identity")
		}
	})
	t.Run("another charger's cert is refused", func(t *testing.T) {
		if a.checkClient("garage", requestWithCert(t, "ftw", "shared-secret", other)) {
			t.Error("accepted a certificate that names a different charger")
		}
	})
	t.Run("no certificate is refused", func(t *testing.T) {
		if a.checkClient("garage", requestFrom(t, "ftw", "shared-secret", "")) {
			t.Error("accepted a connection with no client certificate")
		}
	})
	t.Run("without a client CA the cert is ignored", func(t *testing.T) {
		plain := newAuthorizer(&Config{Username: "ftw", Password: "shared-secret"})
		if !plain.checkClient("garage", requestWithCert(t, "ftw", "shared-secret", other)) {
			t.Error("a client certificate must not be required when no CA is configured")
		}
	})
}

// A per-charger password stays in force when mTLS is on: matching the
// certificate is not enough to skip it, and presenting the password is not
// enough to skip the certificate.
func TestClientCertAndPasswordAreBothRequired(t *testing.T) {
	a := mtlsAuthorizer(map[string]string{"garage": "garage-only-secret"})
	own := &x509.Certificate{Subject: pkix.Name{CommonName: "garage"}}
	other := &x509.Certificate{Subject: pkix.Name{CommonName: "carport"}}

	if !a.checkClient("garage", requestWithCert(t, "garage", "garage-only-secret", own)) {
		t.Error("matching cert and password should be accepted")
	}
	if a.checkClient("garage", requestWithCert(t, "ftw", "shared-secret", own)) {
		t.Error("a matching cert used the shared password to claim a charger that has its own")
	}
	if a.checkClient("garage", requestWithCert(t, "garage", "garage-only-secret", other)) {
		t.Error("another charger's cert presented this charger's password")
	}
}

// TLS has to fail loudly. An operator who asked for wss:// and silently got
// ws:// would have no way to tell the link was never encrypted.
func TestTLSMisconfigurationRefusesToStart(t *testing.T) {
	tests := []struct {
		name string
		tls  *TLSConfig
	}{
		{name: "cert without key", tls: &TLSConfig{CertFile: "cert.pem"}},
		{name: "key without cert", tls: &TLSConfig{KeyFile: "key.pem"}},
		{name: "cert file missing", tls: &TLSConfig{CertFile: "no-such-cert.pem", KeyFile: "no-such-key.pem"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Enabled: true, Bind: "127.0.0.1", Port: freePort(t), TLS: tc.tls}
			if _, err := Start(context.Background(), cfg, telemetry.NewStore()); err == nil {
				t.Fatal("a broken TLS config started anyway, serving plaintext")
			}
		})
	}
}

func TestSchemeFollowsTLS(t *testing.T) {
	if got := (&Config{}).Scheme(); got != "ws" {
		t.Errorf("plaintext scheme: got %q, want ws", got)
	}
	cfg := &Config{TLS: &TLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}}
	if got := cfg.Scheme(); got != "wss" {
		t.Errorf("TLS scheme: got %q, want wss", got)
	}
}

// End to end over a real connection: the library must actually consult both
// gates, in the order that makes the identity binding effective.
func TestPerChargerCredentialOverTheWire(t *testing.T) {
	port := freePort(t)
	cfg := &Config{
		Enabled:            true,
		Bind:               "127.0.0.1",
		Port:               port,
		HeartbeatIntervalS: 60,
		Username:           "ftw",
		Password:           "shared-secret",
		ChargerSecrets:     map[string]string{"garage": "garage-only-secret"},
		ApprovedIDs:        []string{"garage"},
	}
	srv, err := Start(context.Background(), cfg, telemetry.NewStore())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Stop)
	waitForListener(t, port)

	connect := func(t *testing.T, id, user, pass string) error {
		t.Helper()
		client := ws.NewClient()
		client.SetBasicAuth(user, pass)
		cp := ocpp16.NewChargePoint(id, nil, client)
		err := cp.Start(fmt.Sprintf("ws://127.0.0.1:%d", port))
		if err == nil {
			t.Cleanup(cp.Stop)
		}
		return err
	}

	if err := connect(t, "garage", "garage", "garage-only-secret"); err != nil {
		t.Fatalf("the charger's own credential was refused: %v", err)
	}
	if err := connect(t, "garage-impostor", "ftw", "shared-secret"); err != nil {
		t.Fatalf("a charger without its own credential should still connect: %v", err)
	}
	// The gate this test exists for. It also catches the library detail that
	// makes it fragile: ocppj.Server.Start replaces the connection check the
	// ws.Server was given, so a gate registered on the raw server is silently
	// discarded and every impersonation attempt succeeds.
	if err := connect(t, "garage", "ftw", "shared-secret"); err == nil {
		t.Fatal("the shared password connected as a charger that has its own credential")
	}
}

// Two client certificates from the same CA: connecting as the other
// charger's id must fail. The TLS stack only checks the CA; the identity
// bind in checkClient is what stops the swap.
func TestClientCertCannotClaimAnotherIdentity(t *testing.T) {
	bundle := issueMTLSBundle(t)
	port := freePort(t)
	cfg := &Config{
		Enabled:            true,
		Bind:               "127.0.0.1",
		Port:               port,
		HeartbeatIntervalS: 60,
		Username:           "ftw",
		Password:           "shared-secret",
		TLS: &TLSConfig{
			CertFile:     bundle.serverCertFile,
			KeyFile:      bundle.serverKeyFile,
			ClientCAFile: bundle.clientCAFile,
		},
	}
	srv, err := Start(context.Background(), cfg, telemetry.NewStore())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Stop)
	waitForListener(t, port)

	connect := func(t *testing.T, id string, clientCert tls.Certificate) error {
		t.Helper()
		tlsCfg := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      bundle.serverPool,
			Certificates: []tls.Certificate{clientCert},
			ServerName:   "127.0.0.1",
		}
		client := ws.NewTLSClient(tlsCfg)
		client.SetBasicAuth("ftw", "shared-secret")
		cp := ocpp16.NewChargePoint(id, nil, client)
		err := cp.Start(fmt.Sprintf("wss://127.0.0.1:%d", port))
		if err == nil {
			// Drop the socket before the next attempt so a refused swap
			// cannot be mistaken for the library's duplicate-id reject.
			cp.Stop()
		}
		return err
	}

	if err := connect(t, "carport", bundle.garage); err == nil {
		t.Fatal("garage's certificate connected as carport")
	}
	if err := connect(t, "garage", bundle.carport); err == nil {
		t.Fatal("carport's certificate connected as garage")
	}
	if err := connect(t, "garage", bundle.garage); err != nil {
		t.Fatalf("garage's own certificate was refused: %v", err)
	}
	if err := connect(t, "carport", bundle.carport); err != nil {
		t.Fatalf("carport's own certificate was refused: %v", err)
	}
}

type mtlsBundle struct {
	serverCertFile string
	serverKeyFile  string
	clientCAFile   string
	serverPool     *x509.CertPool
	garage         tls.Certificate
	carport        tls.Certificate
}

type issuedCert struct {
	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey
}

func issueMTLSBundle(t *testing.T) mtlsBundle {
	t.Helper()
	dir := t.TempDir()

	server := selfSigned(t, &x509.Certificate{
		SerialNumber:          nextSerial(t),
		Subject:               pkix.Name{CommonName: "ocpp-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	})
	ca := selfSigned(t, &x509.Certificate{
		SerialNumber:          nextSerial(t),
		Subject:               pkix.Name{CommonName: "ocpp-test-client-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	})
	garage := signedBy(t, clientTmpl(t, "garage"), ca)
	carport := signedBy(t, clientTmpl(t, "carport"), ca)

	serverCertFile := filepath.Join(dir, "server.crt")
	serverKeyFile := filepath.Join(dir, "server.key")
	clientCAFile := filepath.Join(dir, "client-ca.crt")
	writePEM(t, serverCertFile, "CERTIFICATE", server.der)
	writePEM(t, serverKeyFile, "EC PRIVATE KEY", marshalKey(t, server.key))
	writePEM(t, clientCAFile, "CERTIFICATE", ca.der)

	pool := x509.NewCertPool()
	pool.AddCert(server.cert)
	return mtlsBundle{
		serverCertFile: serverCertFile,
		serverKeyFile:  serverKeyFile,
		clientCAFile:   clientCAFile,
		serverPool:     pool,
		garage:         tlsCert(garage),
		carport:        tlsCert(carport),
	}
}

func clientTmpl(t *testing.T, id string) *x509.Certificate {
	t.Helper()
	return &x509.Certificate{
		SerialNumber:          nextSerial(t),
		Subject:               pkix.Name{CommonName: id},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{id},
	}
}

func selfSigned(t *testing.T, tmpl *x509.Certificate) issuedCert {
	t.Helper()
	key := mustKey(t)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return issuedCert{cert: cert, der: der, key: key}
}

func signedBy(t *testing.T, tmpl *x509.Certificate, ca issuedCert) issuedCert {
	t.Helper()
	key := mustKey(t)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return issuedCert{cert: cert, der: der, key: key}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func nextSerial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	if n.Sign() == 0 {
		return big.NewInt(1)
	}
	return n
}

func marshalKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func tlsCert(issued issuedCert) tls.Certificate {
	return tls.Certificate{
		Certificate: [][]byte{issued.der},
		PrivateKey:  issued.key,
		Leaf:        issued.cert,
	}
}

// waitForListener blocks until the port accepts, so a client never races the
// listener goroutine.
func waitForListener(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not bind on port %d within deadline", port)
}
