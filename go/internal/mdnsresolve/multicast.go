package mdnsresolve

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// This file is the fallback described in the package comment: a direct RFC
// 6762 query, used only where avahi-daemon's socket cannot be reached. It is
// deliberately the second choice — see avahi.go for the first.

// mdnsAddr is the RFC 6762 IPv4 multicast group. A var, not a const, so tests
// can aim a query at a loopback responder.
var mdnsAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// mdnsAddr6 is the RFC 6762 IPv6 multicast group. The interface zone is added
// to a copy for each query because ff02::fb is link-local by definition.
var mdnsAddr6 = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}

// multicastInterfaces and listenMulticastPacket are vars so tests can select
// a stable interface and responder without touching the host LAN.
var multicastInterfaces = net.Interfaces

// listenMulticastPacket opens an ephemeral socket on one selected interface.
// It deliberately does not bind port 5353: avahi-daemon may already own it,
// and the QU bit asks responders to reply directly to this socket instead.
var listenMulticastPacket = func(network string, iface *net.Interface, group *net.UDPAddr) (*net.UDPConn, error) {
	return net.ListenMulticastUDP(network, iface, &net.UDPAddr{
		IP:   group.IP,
		Port: 0,
		Zone: group.Zone,
	})
}

// classQU is IN with the RFC 6762 unicast-response bit set.
const classQU = dnsmessage.Class(0x8001)

func queryAddrs(ctx context.Context, name string) ([]netip.Addr, time.Duration, error) {
	qname, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: bad name %q: %w", name, err)
	}
	// One packet, two questions. RFC 6762 §5.2 allows it and it saves a round
	// trip on dual-stack devices.
	msg := dnsmessage.Message{Questions: []dnsmessage.Question{
		{Name: qname, Type: dnsmessage.TypeA, Class: classQU},
		{Name: qname, Type: dnsmessage.TypeAAAA, Class: classQU},
	}}
	packed, err := msg.Pack()
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: pack query: %w", err)
	}

	lookupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		family int
		addrs  []netip.Addr
		ttl    time.Duration
		err    error
	}
	results := make(chan result, 2)
	go func() {
		addrs, ttl, err := queryIPv4(lookupCtx, packed, name)
		results <- result{family: 4, addrs: addrs, ttl: ttl, err: err}
	}()
	go func() {
		addrs, ttl, err := queryIPv6(lookupCtx, packed, name)
		results <- result{family: 6, addrs: addrs, ttl: ttl, err: err}
	}()

	var v4, v6 result
	for i := 0; i < 2; i++ {
		got := <-results
		if got.family == 4 {
			v4 = got
		} else {
			v6 = got
		}
		if got.family == 4 && got.err == nil && len(got.addrs) > 0 {
			// One family is enough to dial the device. Cancel the other
			// family, then still collect its result before returning so no
			// query goroutine outlives this lookup. Do not cancel on IPv6
			// first: IPv4 remains the preferred address family.
			cancel()
		}
	}

	addrs := appendUnique(nil, v4.addrs...)
	addrs = appendUnique(addrs, v6.addrs...)
	if len(addrs) == 0 {
		return nil, 0, fmt.Errorf("mdns: no usable answer (IPv4: %v; IPv6: %v)", v4.err, v6.err)
	}
	ttl := minAnswerTTL(v4.ttl, v6.ttl)
	return addrs, ttl, nil
}

func queryIPv4(ctx context.Context, packed []byte, name string) ([]netip.Addr, time.Duration, error) {
	ifaces, err := eligibleMulticastInterfaces()
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: list IPv4 multicast interfaces: %w", err)
	}
	return queryInterfaces(ctx, packed, name, ifaces, "udp4", mdnsAddr, "")
}

func queryIPv6(ctx context.Context, packed []byte, name string) ([]netip.Addr, time.Duration, error) {
	ifaces, err := eligibleMulticastInterfaces()
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: list IPv6 multicast interfaces: %w", err)
	}
	return queryInterfaces(ctx, packed, name, ifaces, "udp6", mdnsAddr6, "interface")
}

func eligibleMulticastInterfaces() ([]net.Interface, error) {
	ifaces, err := multicastInterfaces()
	if err != nil {
		return nil, err
	}
	eligible := make([]net.Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Index <= 0 || iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		eligible = append(eligible, iface)
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no active non-loopback multicast interface")
	}
	return eligible, nil
}

func queryInterfaces(ctx context.Context, packed []byte, name string, ifaces []net.Interface, network string, group *net.UDPAddr, zoneMode string) ([]netip.Addr, time.Duration, error) {
	if len(ifaces) == 0 {
		return nil, 0, fmt.Errorf("no multicast interface")
	}
	type result struct {
		addrs []netip.Addr
		ttl   time.Duration
		err   error
	}
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(ifaces))
	for _, iface := range ifaces {
		iface := iface
		go func() {
			target := *group
			if zoneMode == "interface" && target.IP.IsLinkLocalMulticast() {
				target.Zone = iface.Name
			}
			_, _, err := exchange(queryCtx, packed, &target, func() (*net.UDPConn, error) {
				return listenMulticastPacket(network, &iface, &target)
			}, func(packet []byte, source *net.UDPAddr) bool {
				zone := ""
				if zoneMode == "interface" {
					zone = iface.Name
					if source != nil && source.Zone != "" {
						zone = source.Zone
					}
				}
				got, gotTTL, ok := parseAddrAnswer(packet, name+".", zone)
				if !ok {
					return false
				}
				results <- result{addrs: got, ttl: gotTTL}
				return true
			})
			if err != nil {
				results <- result{err: err}
			}
		}()
	}

	var (
		addrs    []netip.Addr
		ttl      time.Duration
		firstErr error
	)
	for i := 0; i < len(ifaces); i++ {
		got := <-results
		if got.err != nil {
			if firstErr == nil {
				firstErr = got.err
			}
			continue
		}
		addrs = appendUnique(addrs, got.addrs...)
		ttl = minAnswerTTL(ttl, got.ttl)
		if len(got.addrs) > 0 {
			cancel()
		}
	}
	if len(addrs) == 0 {
		if firstErr == nil {
			firstErr = fmt.Errorf("no usable answer")
		}
		return nil, 0, firstErr
	}
	return addrs, ttl, nil
}

// exchange sends one multicast query and feeds every reply to handle until it
// accepts one or the deadline passes. Closing the socket on context cancel is
// important: a cancelled family/interface query must not linger until the
// original read deadline.
func exchange(ctx context.Context, packed []byte, target *net.UDPAddr, open func() (*net.UDPConn, error), handle func([]byte, *net.UDPAddr) bool) ([]netip.Addr, time.Duration, error) {
	conn, err := open()
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: open socket: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	defer conn.Close()
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	deadline := now().Add(queryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, 0, fmt.Errorf("mdns: set deadline: %w", err)
	}
	if _, err := conn.WriteToUDP(packed, target); err != nil {
		return nil, 0, fmt.Errorf("mdns: send query: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		n, source, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, 0, fmt.Errorf("mdns: no usable answer: %w", err)
		}
		if handle(buf[:n], source) {
			// The callback stores the parsed answer in its closure. The
			// caller only needs the success signal here.
			return nil, 0, nil
		}
	}
}

func parseAddrAnswer(packet []byte, qname string, zones ...string) ([]netip.Addr, time.Duration, bool) {
	zone := ""
	if len(zones) > 0 {
		zone = zones[0]
	}
	var p dnsmessage.Parser
	if _, err := p.Start(packet); err != nil {
		return nil, 0, false
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, 0, false
	}
	var addrs []netip.Addr
	ttl := maxTTL
	// Labelled so a parse error inside the type switch abandons the whole
	// packet: once the parser desynchronises, every later record is suspect.
parse:
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			break parse
		}
		if !strings.EqualFold(h.Name.String(), qname) {
			if err := p.SkipAnswer(); err != nil {
				break parse
			}
			continue
		}
		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				break parse
			}
			addrs = append(addrs, netip.AddrFrom4(r.A))
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				break parse
			}
			// Unmap so a v4-mapped AAAA dials as plain IPv4.
			addr := netip.AddrFrom16(r.AAAA).Unmap()
			if addr.Is6() && addr.IsLinkLocalUnicast() {
				// A link-local address without a zone is not a safe dial
				// target: the kernel cannot know which interface to use.
				if zone == "" {
					continue
				}
				addr = addr.WithZone(zone)
			}
			addrs = append(addrs, addr)
		default:
			if err := p.SkipAnswer(); err != nil {
				break parse
			}
			continue
		}
		if d := time.Duration(h.TTL) * time.Second; d < ttl {
			ttl = d
		}
	}
	return finishAnswer(addrs, ttl)
}

func finishAnswer(addrs []netip.Addr, ttl time.Duration) ([]netip.Addr, time.Duration, bool) {
	if len(addrs) == 0 {
		return nil, 0, false
	}
	switch {
	case ttl < minTTL:
		ttl = minTTL
	case ttl > maxTTL:
		ttl = maxTTL
	}
	return addrs, ttl, true
}

func appendUnique(dst []netip.Addr, src ...netip.Addr) []netip.Addr {
	for _, candidate := range src {
		seen := false
		for _, existing := range dst {
			if existing == candidate {
				seen = true
				break
			}
		}
		if !seen {
			dst = append(dst, candidate)
		}
	}
	return dst
}

func minAnswerTTL(values ...time.Duration) time.Duration {
	var min time.Duration
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if min == 0 || value < min {
			min = value
		}
	}
	if min == 0 {
		return minTTL
	}
	return min
}
