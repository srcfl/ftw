package ocpp

// Who may speak, as whom, and from where.
//
// Three gates sit in front of a charge point, and they answer different
// questions. Basic auth proves knowledge of a secret. The identity binding
// below proves the connection is entitled to the identity it claims. The
// quarantine in handlers.go decides whether an authenticated, entitled charger
// is part of this site at all.
//
// The library hands us two callbacks. SetBasicAuthHandler sees the credential
// but not the identity; SetCheckClientHandler sees the identity and the whole
// HTTP request — including the credential, the local address the connection
// landed on, and any client certificate. Everything that needs both therefore
// happens in checkClient, which the library calls immediately after basic auth
// and before the WebSocket upgrade.

import (
	"crypto/subtle"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
)

// authorizer holds the credentials and the interface restriction for one
// listener. The zero value authorizes everything, which is what an OCPP
// section with no username configured asks for.
type authorizer struct {
	// sharedUser and sharedPass are the site-wide credential. Every charge
	// point without one of its own uses it.
	sharedUser string
	sharedPass string

	// perCharger maps a charge point identity to its own password. A
	// charger listed here must present that password AND connect under
	// that identity — see checkClient. This is what makes an adopted
	// charger un-impersonable by anything holding only the shared secret.
	// When mTLS is on, the password is an additional gate, not the only one.
	perCharger map[string]string

	// requireClientCert is set when a client CA is configured. The TLS
	// stack has already checked the CA; checkClient then requires the
	// certificate's CN or DNS SAN to be the identity in the URL.
	requireClientCert bool

	// bindIP is the address the operator asked the listener to serve on.
	// Nil, unspecified (0.0.0.0 / ::) means every interface.
	bindIP net.IP
}

// newAuthorizer builds the gate for a listener from config.
func newAuthorizer(cfg *Config) *authorizer {
	a := &authorizer{
		sharedUser: cfg.Username,
		sharedPass: cfg.Password,
	}
	if len(cfg.ChargerSecrets) > 0 {
		a.perCharger = make(map[string]string, len(cfg.ChargerSecrets))
		for id, pass := range cfg.ChargerSecrets {
			if id != "" && pass != "" {
				a.perCharger[id] = pass
			}
		}
	}
	if ip := net.ParseIP(cfg.Bind); ip != nil && !ip.IsUnspecified() {
		a.bindIP = ip
	}
	if cfg.TLS != nil && cfg.TLS.ClientCAFile != "" {
		a.requireClientCert = true
	}
	return a
}

// requiresCredential reports whether any credential is configured at all.
//
// The library treats a registered basic-auth handler as "credentials are
// mandatory" and answers 401 to a charger that sends none, so the handler must
// stay unregistered when nothing is configured — otherwise enabling OCPP with
// no username would lock out every charger instead of admitting them all.
func (a *authorizer) requiresCredential() bool {
	if a == nil {
		return false
	}
	return a.sharedUser != "" || a.sharedPass != "" || len(a.perCharger) > 0
}

// secretEqual compares in constant time so a wrong password cannot be found
// one character at a time.
func secretEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// basicAuth is the first gate: does this connection know a secret we issued?
//
// It cannot yet tell whether the credential belongs to the identity being
// claimed — the library does not pass the URL here — so it accepts any
// credential we recognise and leaves the binding to checkClient. On OCPP the
// basic-auth username is the charge point identity, which is why a per-charger
// secret is looked up by username.
func (a *authorizer) basicAuth(user, pass string) bool {
	if a == nil {
		return true
	}
	if secret, ok := a.perCharger[user]; ok {
		return secretEqual(pass, secret)
	}
	if a.sharedUser == "" && a.sharedPass == "" {
		// No credential configured. Validation refuses this for an enabled
		// server, so it is reachable only in tests.
		return true
	}
	// Both halves compare in constant time. The username is as much of a
	// secret as the password on a listener the socket layer will not pin,
	// and comparing it with == would leak it one character at a time.
	userOK := secretEqual(user, a.sharedUser)
	passOK := secretEqual(pass, a.sharedPass)
	return userOK && passOK
}

// checkClient is the second gate, and the one that closes impersonation.
//
// A charge point picks its own identity — it is the last segment of the URL it
// dialled — so "it authenticated" has never proved which device it is.
//
// When a client CA is configured, the connection must present a certificate
// whose CN or DNS SAN is exactly that identity. The TLS stack already checked
// the CA; this is what stops any cert from that CA claiming any name.
//
// Where a charger has its own credential, this also requires the connection
// to present that exact credential under that exact identity: the shared
// password no longer buys an attacker an adopted charger's name, only a
// pending row. A per-charger password is an additional gate, not a substitute
// for the certificate binding.
//
// It also enforces the configured bind address. The library builds its listen
// address from the port alone, so the socket itself is unavoidably on every
// interface; refusing the handshake here is what actually stops a charger from
// talking to us over one the operator did not offer. A port scan still sees an
// open port — this is an access control, not a smaller attack surface.
func (a *authorizer) checkClient(id string, r *http.Request) bool {
	if a == nil {
		return true
	}
	if !a.allowedLocalAddr(r) {
		slog.Warn("ocpp: refused a charge point that arrived on an address the server is not offered on",
			"charger", id, "bind", a.bindIP.String(), "arrived_on", localAddr(r))
		return false
	}
	if a.requireClientCert && !clientCertIdentifies(id, r) {
		return false
	}
	secret, hasOwn := a.perCharger[id]
	if !hasOwn {
		return true
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != id || !secretEqual(pass, secret) {
		slog.Warn("ocpp: refused a connection claiming a charger that has its own credential",
			"charger", id, "presented_user", user)
		return false
	}
	return true
}

// clientCertIdentifies reports whether the verified leaf certificate names
// this charge-point identity. The TLS handshake has already required a cert
// from the configured CA; matching CN or DNS SAN is what binds that cert to
// the URL. Logs and returns false when no cert is present or none of its
// names is the identity.
func clientCertIdentifies(id string, r *http.Request) bool {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		slog.Warn("ocpp: refused a charge point that presented no client certificate",
			"charger", id)
		return false
	}
	cert := r.TLS.PeerCertificates[0]
	if certNames(cert, id) {
		return true
	}
	slog.Warn("ocpp: refused a charge point whose client certificate does not name the identity it claimed",
		"charger", id, "cert_cn", cert.Subject.CommonName)
	return false
}

// certNames reports whether the certificate's CN or a DNS SAN is exactly id.
func certNames(cert *x509.Certificate, id string) bool {
	if cert == nil || id == "" {
		return false
	}
	if cert.Subject.CommonName == id {
		return true
	}
	for _, name := range cert.DNSNames {
		if name == id {
			return true
		}
	}
	return false
}

// allowedLocalAddr reports whether the connection landed on the interface the
// operator asked for. Always true when no specific bind address is set.
func (a *authorizer) allowedLocalAddr(r *http.Request) bool {
	if a.bindIP == nil {
		return true
	}
	addr := localAddr(r)
	if addr == "" {
		// No local address on the request context means we cannot tell, and
		// refusing every connection is worse than the status quo ante.
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	// Equal treats an IPv4-mapped IPv6 address as the IPv4 it maps to, which
	// is how a dual-stack listener reports an IPv4 connection.
	return a.bindIP.Equal(ip)
}

// localAddr is the address on this host that the connection arrived at, which
// net/http puts on every request's context.
func localAddr(r *http.Request) string {
	if r == nil {
		return ""
	}
	v := r.Context().Value(http.LocalAddrContextKey)
	addr, ok := v.(net.Addr)
	if !ok || addr == nil {
		return ""
	}
	return addr.String()
}
