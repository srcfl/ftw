package scanner

import (
	"context"
	"net"
	"testing"
)

// stubProbes replaces the three name probes for the duration of a test.
func stubProbes(t *testing.T, reverseMDNS func(context.Context, string) string, verify func(context.Context, string, string) string, reverseDNS func(context.Context, *net.Resolver, string) string) {
	t.Helper()
	oldMDNS, oldVerify, oldDNS := probeReverseMDNS, probeVerifyLocal, probeReverseDNS
	t.Cleanup(func() {
		probeReverseMDNS, probeVerifyLocal, probeReverseDNS = oldMDNS, oldVerify, oldDNS
	})
	probeReverseMDNS = func(ctx context.Context, ip string) string {
		if reverseMDNS == nil {
			return ""
		}
		return reverseMDNS(ctx, ip)
	}
	probeVerifyLocal = func(ctx context.Context, label, ip string) string {
		if verify == nil {
			return ""
		}
		return verify(ctx, label, ip)
	}
	probeReverseDNS = func(ctx context.Context, r *net.Resolver, ip string) string {
		if reverseDNS == nil {
			return ""
		}
		return reverseDNS(ctx, r, ip)
	}
}

// A name the device answers for itself wins outright — the router's label for
// the same lease must not displace it.
func TestLookupNamePrefersTheDeviceOwnedLocalName(t *testing.T) {
	stubProbes(t,
		func(context.Context, string) string { return "inverter.local" },
		func(context.Context, string, string) string {
			t.Fatal("forward verification should not run when reverse mDNS already answered")
			return ""
		},
		func(context.Context, *net.Resolver, string) string { return "inverter.localdomain" },
	)

	if got := lookupName(context.Background(), &net.Resolver{}, "192.168.1.141"); got != "inverter.local" {
		t.Fatalf("hostname = %q, want inverter.local", got)
	}
}

// The Sourceful Zap's real behaviour: it publishes a forward A record and no
// in-addr.arpa PTR at all, so only the router answers a reverse query. The
// label still has to reach the wizard as a ".local" name.
func TestLookupNameVerifiesTheLabelForwardWhenNoReversePTRExists(t *testing.T) {
	var askedLabel, askedIP string
	stubProbes(t,
		func(context.Context, string) string { return "" }, // no reverse mDNS record
		func(_ context.Context, label, ip string) string {
			askedLabel, askedIP = label, ip
			return label + ".local"
		},
		func(context.Context, *net.Resolver, string) string {
			return "zap-000064963cd51edc.localdomain"
		},
	)

	got := lookupName(context.Background(), &net.Resolver{}, "192.168.1.141")
	if want := "zap-000064963cd51edc.local"; got != want {
		t.Fatalf("hostname = %q, want %q", got, want)
	}
	if askedLabel != "zap-000064963cd51edc" {
		t.Errorf("verified label = %q, want the first label only", askedLabel)
	}
	if askedIP != "192.168.1.141" {
		t.Errorf("verified against %q, want the scanned address", askedIP)
	}
}

// Verification failing is not a reason to invent a name. The router's label
// is still worth showing, but it must come back unchanged so the wizard saves
// the IP instead.
func TestLookupNameKeepsTheDisplayNameWhenForwardVerificationFails(t *testing.T) {
	stubProbes(t,
		func(context.Context, string) string { return "" },
		func(context.Context, string, string) string { return "" }, // resolves elsewhere, or not at all
		func(context.Context, *net.Resolver, string) string { return "printer.localdomain" },
	)

	if got := lookupName(context.Background(), &net.Resolver{}, "192.168.1.9"); got != "printer.localdomain" {
		t.Fatalf("hostname = %q, want the unverified name passed through for display", got)
	}
}

// Nothing answered anywhere: an empty name, not a fabricated one.
func TestLookupNameReturnsEmptyWhenNothingAnswers(t *testing.T) {
	stubProbes(t, nil, nil, nil)

	if got := lookupName(context.Background(), &net.Resolver{}, "192.168.1.9"); got != "" {
		t.Fatalf("hostname = %q, want empty", got)
	}
}

func TestVerifiedLocalNameRejectsAnAnswerForADifferentAddress(t *testing.T) {
	// Lookup is not stubbed here; an unresolvable name exercises the error
	// path without touching the LAN.
	if got := verifiedLocalName(context.Background(), "", "192.168.1.141"); got != "" {
		t.Fatalf("empty label produced %q", got)
	}
	if got := verifiedLocalName(context.Background(), "device", "not-an-ip"); got != "" {
		t.Fatalf("malformed address produced %q", got)
	}
}

func TestHostLabelTakesTheFirstLabelLowercased(t *testing.T) {
	cases := map[string]string{
		"zap-0000649.localdomain.": "zap-0000649",
		"Inverter.Local":           "inverter",
		"bare":                     "bare",
		"":                         "",
	}
	for in, want := range cases {
		if got := hostLabel(in); got != want {
			t.Errorf("hostLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsLocalName(t *testing.T) {
	for _, name := range []string{"zap.local", "ZAP.LOCAL", "a.b.local."} {
		if !isLocalName(name) {
			t.Errorf("isLocalName(%q) = false", name)
		}
	}
	for _, name := range []string{"zap.localdomain", "zap.lan", "", "local"} {
		if isLocalName(name) {
			t.Errorf("isLocalName(%q) = true", name)
		}
	}
}
