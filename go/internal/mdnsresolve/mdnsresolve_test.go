package mdnsresolve

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestIsLocal(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"inverter.local", true},
		{"INVERTER.LOCAL", true},
		{"inverter.local.", true},
		{"zap.local", true},
		// A literal address must never trigger a lookup.
		{"192.168.1.5", false},
		{"::1", false},
		{"example.com", false},
		{"localhost", false},
		{"local", false},
		{"notlocal", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsLocal(c.host); got != c.want {
			t.Errorf("IsLocal(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func mustDNSName(t *testing.T, s string) dnsmessage.Name {
	t.Helper()
	n, err := dnsmessage.NewName(s)
	if err != nil {
		t.Fatalf("NewName(%q): %v", s, err)
	}
	return n
}

func aResource(t *testing.T, name string, ip [4]byte, ttl uint32) dnsmessage.Resource {
	t.Helper()
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name: mustDNSName(t, name), Type: dnsmessage.TypeA,
			Class: dnsmessage.ClassINET, TTL: ttl,
		},
		Body: &dnsmessage.AResource{A: ip},
	}
}

func packAnswer(t *testing.T, qname string, answers []dnsmessage.Resource) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true, Authoritative: true},
		Questions: []dnsmessage.Question{{Name: mustDNSName(t, qname), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
		Answers:   answers,
	}
	packet, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	return packet
}

func TestParseAddrAnswer(t *testing.T) {
	qname := "inverter.local."
	packet := packAnswer(t, qname, []dnsmessage.Resource{aResource(t, qname, [4]byte{192, 168, 1, 42}, 60)})

	addrs, ttl, ok := parseAddrAnswer(packet, qname)
	if !ok {
		t.Fatal("parseAddrAnswer did not accept a valid answer")
	}
	if len(addrs) != 1 || addrs[0].String() != "192.168.1.42" {
		t.Fatalf("addrs = %v, want [192.168.1.42]", addrs)
	}
	if ttl != 60*time.Second {
		t.Fatalf("ttl = %v, want 60s", ttl)
	}

	// An answer for a different name must be ignored.
	if _, _, ok := parseAddrAnswer(packet, "other.local."); ok {
		t.Fatal("accepted an answer for a different name")
	}
	// Garbage must not panic or resolve.
	if _, _, ok := parseAddrAnswer([]byte{1, 2, 3}, qname); ok {
		t.Fatal("accepted a malformed packet")
	}
}

func TestParseAddrAnswerClampsTTL(t *testing.T) {
	qname := "inverter.local."
	for _, c := range []struct {
		name string
		ttl  uint32
		want time.Duration
	}{
		// A device advertising a 1 s TTL must not make every Modbus reconnect
		// re-query the LAN.
		{"below floor", 1, minTTL},
		// A very long TTL must not outlive a DHCP move.
		{"above ceiling", 86400, maxTTL},
		{"inside range", 90, 90 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			packet := packAnswer(t, qname, []dnsmessage.Resource{aResource(t, qname, [4]byte{10, 0, 0, 1}, c.ttl)})
			_, ttl, ok := parseAddrAnswer(packet, qname)
			if !ok {
				t.Fatal("answer rejected")
			}
			if ttl != c.want {
				t.Fatalf("ttl = %v, want %v", ttl, c.want)
			}
		})
	}
}

// disableAvahi forces the fallback path. Without it these tests would behave
// differently on a developer machine that happens to run avahi-daemon.
func disableAvahi(t *testing.T) {
	t.Helper()
	orig := avahiAvailable
	avahiAvailable = func() bool { return false }
	t.Cleanup(func() { avahiAvailable = orig })
}

// startResponder points the package at a loopback UDP socket that answers one
// query, so the real send/parse path is exercised without touching the LAN.
func startResponder(t *testing.T, answers []dnsmessage.Resource) {
	t.Helper()
	rc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen responder: %v", err)
	}

	origAddr, origListen := mdnsAddr, listenPacket
	mdnsAddr = rc.LocalAddr().(*net.UDPAddr)
	listenPacket = func() (*net.UDPConn, error) {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		_ = rc.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := rc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if answers == nil {
			return // silent responder: exercises the negative path
		}
		var p dnsmessage.Parser
		hdr, err := p.Start(buf[:n])
		if err != nil {
			return
		}
		q, err := p.Question()
		if err != nil {
			return
		}
		resp := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true},
			Questions: []dnsmessage.Question{q},
			Answers:   answers,
		}
		packed, err := resp.Pack()
		if err != nil {
			return
		}
		_, _ = rc.WriteToUDP(packed, from)
	}()

	t.Cleanup(func() {
		_ = rc.Close()
		<-done
		mdnsAddr, listenPacket = origAddr, origListen
		Flush()
	})
}

func TestLookupResolvesLocalName(t *testing.T) {
	Flush()
	disableAvahi(t)
	startResponder(t, []dnsmessage.Resource{aResource(t, "inverter.local.", [4]byte{192, 168, 1, 42}, 60)})

	addrs, err := Lookup(context.Background(), "inverter.local")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != netip.MustParseAddr("192.168.1.42") {
		t.Fatalf("addrs = %v, want [192.168.1.42]", addrs)
	}

	// The answer must now be cached: a second call cannot need the responder,
	// which has already stopped.
	again, err := Lookup(context.Background(), "INVERTER.local")
	if err != nil {
		t.Fatalf("cached Lookup: %v", err)
	}
	if len(again) != 1 || again[0] != addrs[0] {
		t.Fatalf("cached addrs = %v, want %v", again, addrs)
	}
}

func TestLookupCachesNegativeAnswer(t *testing.T) {
	Flush()
	disableAvahi(t)
	startResponder(t, nil) // never answers

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Lookup(ctx, "missing.local"); err == nil {
		t.Fatal("expected a lookup failure when nothing answers")
	}

	addrs, ok := cacheLookup("missing.local")
	if !ok {
		t.Fatal("a failed lookup should be negatively cached")
	}
	if len(addrs) != 0 {
		t.Fatalf("negative cache holds %v, want no addresses", addrs)
	}
}

func TestCacheExpires(t *testing.T) {
	Flush()
	base := time.Now()
	orig := now
	now = func() time.Time { return base }
	t.Cleanup(func() { now = orig; Flush() })

	cacheStore("inverter.local", []netip.Addr{netip.MustParseAddr("192.168.1.9")}, 30*time.Second)
	if _, ok := cacheLookup("inverter.local"); !ok {
		t.Fatal("entry should be live immediately after store")
	}

	now = func() time.Time { return base.Add(31 * time.Second) }
	if _, ok := cacheLookup("inverter.local"); ok {
		t.Fatal("entry should have expired")
	}
}

func TestDialerSkipsResolutionForPlainHosts(t *testing.T) {
	origListen, origAvail := listenPacket, avahiAvailable
	listenPacket = func() (*net.UDPConn, error) {
		t.Error("issued an mDNS query for a host that is not a .local name")
		return nil, errors.New("should not be called")
	}
	avahiAvailable = func() bool {
		t.Error("consulted avahi for a host that is not a .local name")
		return false
	}
	t.Cleanup(func() { listenPacket, avahiAvailable = origListen, origAvail })

	d := Dialer{Dialer: net.Dialer{Timeout: 500 * time.Millisecond}}
	// Nothing listens on port 1; the point is that the failure comes from the
	// dial, not from resolution.
	if _, err := d.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("expected the dial to fail")
	} else if strings.Contains(err.Error(), "mDNS") {
		t.Fatalf("plain IP dial went through mDNS: %v", err)
	}
}

func TestDialerReportsResolutionFailure(t *testing.T) {
	Flush()
	disableAvahi(t)
	startResponder(t, nil) // never answers

	d := Dialer{Dialer: net.Dialer{Timeout: 100 * time.Millisecond}}
	_, err := d.Dial("tcp", "missing.local:502")
	if err == nil {
		t.Fatal("expected a failure")
	}
	// The error must name the mechanism — an operator reading the log has to be
	// able to tell resolution apart from an unreachable device.
	if !strings.Contains(err.Error(), "mDNS") {
		t.Fatalf("error %q does not mention mDNS", err)
	}
}
