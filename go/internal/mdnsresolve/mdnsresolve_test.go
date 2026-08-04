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

func aaaaResource(t *testing.T, name string, ip [16]byte, ttl uint32) dnsmessage.Resource {
	t.Helper()
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name: mustDNSName(t, name), Type: dnsmessage.TypeAAAA,
			Class: dnsmessage.ClassINET, TTL: ttl,
		},
		Body: &dnsmessage.AAAAResource{AAAA: ip},
	}
}

func packAnswer(t *testing.T, qname string, answers []dnsmessage.Resource) []byte {
	t.Helper()
	return packDNSMessage(t, dnsmessage.Header{Response: true, Authoritative: true},
		[]dnsmessage.Question{{Name: mustDNSName(t, qname), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}, answers)
}

func packDNSMessage(t *testing.T, header dnsmessage.Header, questions []dnsmessage.Question, answers []dnsmessage.Resource) []byte {
	t.Helper()
	msg := dnsmessage.Message{Header: header, Questions: questions, Answers: answers}
	packet, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	return packet
}

func parseTestAddrAnswer(t *testing.T, packet []byte, qname, network string) ([]netip.Addr, time.Duration, bool) {
	t.Helper()
	var sourceIP string
	if network == "udp6" {
		sourceIP = "2001:db8::1"
	} else {
		sourceIP = "192.0.2.1"
	}
	return parseAddrAnswer(packet, qname, &net.UDPAddr{
		IP:   net.ParseIP(sourceIP),
		Port: 5353,
	}, network, &net.Interface{
		Index: 1,
		Name:  "test0",
		Flags: net.FlagUp | net.FlagMulticast,
	})
}

func TestParseAddrAnswerRequiresResponseBitButNotQuestion(t *testing.T) {
	qname := "inverter.local."
	answers := []dnsmessage.Resource{aResource(t, qname, [4]byte{192, 168, 1, 42}, 60)}

	withoutQuestion := packDNSMessage(t, dnsmessage.Header{Response: true, Authoritative: true}, nil, answers)
	if _, _, ok := parseTestAddrAnswer(t, withoutQuestion, qname, "udp4"); !ok {
		t.Fatal("rejected a valid mDNS response without an echoed question")
	}

	query := packDNSMessage(t, dnsmessage.Header{Response: false}, nil, answers)
	if _, _, ok := parseTestAddrAnswer(t, query, qname, "udp4"); ok {
		t.Fatal("accepted a DNS query with answer records as an mDNS response")
	}
}

func TestParseAddrAnswer(t *testing.T) {
	qname := "inverter.local."
	packet := packAnswer(t, qname, []dnsmessage.Resource{aResource(t, qname, [4]byte{192, 168, 1, 42}, 60)})

	addrs, ttl, ok := parseTestAddrAnswer(t, packet, qname, "udp4")
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
	if _, _, ok := parseTestAddrAnswer(t, packet, "other.local.", "udp4"); ok {
		t.Fatal("accepted an answer for a different name")
	}
	// Garbage must not panic or resolve.
	if _, _, ok := parseTestAddrAnswer(t, []byte{1, 2, 3}, qname, "udp4"); ok {
		t.Fatal("accepted a malformed packet")
	}
}

func TestParseAddrAnswerValidatesSourceFamilyAndClass(t *testing.T) {
	qname := "inverter.local."
	answer := aResource(t, qname, [4]byte{192, 168, 1, 42}, 60)
	packet := packAnswer(t, qname, []dnsmessage.Resource{answer})
	iface := &net.Interface{Index: 1, Name: "test0", Flags: net.FlagUp | net.FlagMulticast}

	for _, tc := range []struct {
		name    string
		source  *net.UDPAddr
		network string
	}{
		{"nil source", nil, "udp4"},
		{"wrong source port", &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 5354}, "udp4"},
		{"wrong source family", &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 5353}, "udp4"},
		{"unspecified source", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 5353}, "udp4"},
		{"multicast source", &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: 5353}, "udp4"},
		{"wrong requested family", &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 5353}, "udp6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := parseAddrAnswer(packet, qname, tc.source, tc.network, iface); ok {
				t.Fatalf("accepted source=%v network=%s", tc.source, tc.network)
			}
		})
	}

	wrongClass := answer
	wrongClass.Header.Class = dnsmessage.ClassCHAOS
	if _, _, ok := parseTestAddrAnswer(t, packAnswer(t, qname, []dnsmessage.Resource{wrongClass}), qname, "udp4"); ok {
		t.Fatal("accepted an A answer from the wrong DNS class")
	}

	cacheFlush := answer
	cacheFlush.Header.Class |= classCacheFlush
	if _, _, ok := parseTestAddrAnswer(t, packAnswer(t, qname, []dnsmessage.Resource{cacheFlush}), qname, "udp4"); !ok {
		t.Fatal("rejected an IN answer carrying the mDNS cache-flush bit")
	}
}

func TestParseAddrAnswerRejectsAnswerFromWrongFamily(t *testing.T) {
	qname := "inverter.local."
	iface := &net.Interface{Index: 1, Name: "test0", Flags: net.FlagUp | net.FlagMulticast}

	ipv6 := packDNSMessage(t, dnsmessage.Header{Response: true}, nil, []dnsmessage.Resource{
		aaaaResource(t, qname, [16]byte{0x20, 1, 0xdb, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, 60),
	})
	if _, _, ok := parseAddrAnswer(ipv6, qname, &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 5353}, "udp4", iface); ok {
		t.Fatal("accepted an IPv6 answer on the IPv4 query path")
	}

	ipv4 := packAnswer(t, qname, []dnsmessage.Resource{aResource(t, qname, [4]byte{192, 168, 1, 42}, 60)})
	if _, _, ok := parseAddrAnswer(ipv4, qname, &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 5353}, "udp6", iface); ok {
		t.Fatal("accepted an IPv4 answer on the IPv6 query path")
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
			_, ttl, ok := parseTestAddrAnswer(t, packet, qname, "udp4")
			if !ok {
				t.Fatal("answer rejected")
			}
			if ttl != c.want {
				t.Fatalf("ttl = %v, want %v", ttl, c.want)
			}
		})
	}
}

func TestParseAddrAnswerUsesSelectedInterfaceForLinkLocalIPv6(t *testing.T) {
	qname := "inverter.local."
	packet := packAnswer(t, qname, []dnsmessage.Resource{
		aaaaResource(t, qname, [16]byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, 60),
	})
	source := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 5353}
	if _, _, ok := parseAddrAnswer(packet, qname, source, "udp6", &net.Interface{
		Index: 1,
		Flags: net.FlagUp | net.FlagMulticast,
	}); ok {
		t.Fatal("accepted link-local IPv6 answer without a valid selected interface")
	}
	addrs, _, ok := parseTestAddrAnswer(t, packet, qname, "udp6")
	if !ok || len(addrs) != 1 || addrs[0].String() != "fe80::1%test0" {
		t.Fatalf("zoned answer = %v, ok=%v; want [fe80::1%%test0]", addrs, ok)
	}
}

func TestQueryIPv6UsesSelectedInterfaceForLinkLocalAnswer(t *testing.T) {
	qname := "inverter.local"
	qnameWire := mustDNSName(t, qname+".")
	queryMessage := dnsmessage.Message{Questions: []dnsmessage.Question{
		{Name: qnameWire, Type: dnsmessage.TypeAAAA, Class: classQU},
	}}
	query, err := queryMessage.Pack()
	if err != nil {
		t.Fatal(err)
	}

	responder, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer responder.Close()
	responseConn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: mdnsPort})
	if err != nil {
		t.Skipf("IPv6 mDNS response port unavailable: %v", err)
	}
	defer responseConn.Close()

	origAddr, origInterfaces, origListen := mdnsAddr6, multicastInterfaces, listenMulticastPacket
	mdnsAddr6 = responder.LocalAddr().(*net.UDPAddr)
	multicastInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 7, Name: "test0", Flags: net.FlagUp | net.FlagMulticast}}, nil
	}
	listenMulticastPacket = func(network string, iface *net.Interface, group *net.UDPAddr) (*net.UDPConn, error) {
		if network != "udp6" || iface.Name != "test0" {
			t.Fatalf("query used network=%q iface=%v", network, iface)
		}
		return net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1")})
	}
	t.Cleanup(func() {
		mdnsAddr6, multicastInterfaces, listenMulticastPacket = origAddr, origInterfaces, origListen
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		_ = responder.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := responder.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var parser dnsmessage.Parser
		header, err := parser.Start(buf[:n])
		if err != nil {
			return
		}
		question, err := parser.Question()
		if err != nil {
			return
		}
		responseMessage := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true},
			Questions: []dnsmessage.Question{question},
			Answers:   []dnsmessage.Resource{aaaaResource(t, qname+".", [16]byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, 60)},
		}
		response, err := responseMessage.Pack()
		if err == nil {
			_, _ = responseConn.WriteToUDP(response, from)
		}
	}()

	addrs, _, err := queryIPv6(context.Background(), query, qname)
	if err != nil {
		t.Fatalf("queryIPv6: %v", err)
	}
	<-done
	if len(addrs) != 1 || addrs[0].String() != "fe80::1%test0" {
		t.Fatalf("addrs = %v, want [fe80::1%%test0]", addrs)
	}
}

func TestEligibleMulticastInterfacesRejectsUnsafeChoices(t *testing.T) {
	orig := multicastInterfaces
	multicastInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagMulticast | net.FlagLoopback},
			{Index: 2, Name: "down0", Flags: net.FlagMulticast},
			{Index: 3, Name: "unicast0", Flags: net.FlagUp},
			{Index: 4, Name: "lan0", Flags: net.FlagUp | net.FlagMulticast},
		}, nil
	}
	t.Cleanup(func() { multicastInterfaces = orig })

	got, err := eligibleMulticastInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "lan0" {
		t.Fatalf("eligible interfaces = %v, want [lan0]", got)
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
	responseConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: mdnsPort})
	if err != nil {
		_ = rc.Close()
		t.Skipf("mDNS response port unavailable: %v", err)
	}

	origAddr, origMulticast, origInterfaces := mdnsAddr, listenMulticastPacket, multicastInterfaces
	mdnsAddr = rc.LocalAddr().(*net.UDPAddr)
	multicastInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 1, Name: "test0", Flags: net.FlagUp | net.FlagMulticast}}, nil
	}
	listenMulticastPacket = func(network string, iface *net.Interface, group *net.UDPAddr) (*net.UDPConn, error) {
		if network != "udp4" {
			return nil, errors.New("IPv6 disabled in IPv4 responder test")
		}
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
		_, _ = responseConn.WriteToUDP(packed, from)
	}()

	t.Cleanup(func() {
		_ = rc.Close()
		<-done
		_ = responseConn.Close()
		mdnsAddr = origAddr
		listenMulticastPacket, multicastInterfaces = origMulticast, origInterfaces
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
	origAvail, origMulticast := avahiAvailable, listenMulticastPacket
	listenMulticastPacket = func(network string, iface *net.Interface, group *net.UDPAddr) (*net.UDPConn, error) {
		t.Error("issued an mDNS query for a host that is not a .local name")
		return nil, errors.New("should not be called")
	}
	avahiAvailable = func() bool {
		t.Error("consulted avahi for a host that is not a .local name")
		return false
	}
	t.Cleanup(func() { avahiAvailable, listenMulticastPacket = origAvail, origMulticast })

	d := Dialer{Dialer: net.Dialer{Timeout: 500 * time.Millisecond}}
	// Nothing listens on port 1; the point is that the failure comes from the
	// dial, not from resolution.
	if _, err := d.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("expected the dial to fail")
	} else if strings.Contains(err.Error(), "mDNS") {
		t.Fatalf("plain IP dial went through mDNS: %v", err)
	}
}

func TestDialerResolvesLocalNameBeforeConnecting(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			close(accepted)
			_ = conn.Close()
		}
	}()
	fakeAvahi(t, func(command, name string) string {
		if command == "RESOLVE-HOSTNAME-IPV4" && name == "inverter.local" {
			return "+ 2 0 inverter.local 127.0.0.1"
		}
		return "- 15 Timeout reached"
	})

	d := Dialer{Dialer: net.Dialer{Timeout: time.Second}}
	conn, err := d.Dial("tcp", "inverter.local:"+port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("resolved TCP endpoint was not reached")
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
