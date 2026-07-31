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

// listenPacket opens the ephemeral socket a query is sent from. Replaced in
// tests. It deliberately does NOT bind port 5353: where avahi-daemon runs it
// already owns that port, and the QU bit below asks responders to reply
// directly to this socket instead.
var listenPacket = func() (*net.UDPConn, error) {
	return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
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

	var (
		addrs []netip.Addr
		ttl   time.Duration
	)
	err = exchange(ctx, packed, func(packet []byte) bool {
		got, gotTTL, ok := parseAddrAnswer(packet, name+".")
		if !ok {
			return false
		}
		addrs, ttl = got, gotTTL
		return true
	})
	if err != nil {
		return nil, 0, err
	}
	return addrs, ttl, nil
}

// exchange sends one multicast query and feeds every reply to handle until it
// accepts one or the deadline passes.
func exchange(ctx context.Context, packed []byte, handle func([]byte) bool) error {
	conn, err := listenPacket()
	if err != nil {
		return fmt.Errorf("mdns: open socket: %w", err)
	}
	defer conn.Close()

	deadline := now().Add(queryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("mdns: set deadline: %w", err)
	}
	if _, err := conn.WriteToUDP(packed, mdnsAddr); err != nil {
		return fmt.Errorf("mdns: send query: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return fmt.Errorf("mdns: no usable answer: %w", err)
		}
		if handle(buf[:n]) {
			return nil
		}
	}
}

func parseAddrAnswer(packet []byte, qname string) ([]netip.Addr, time.Duration, bool) {
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
			addrs = append(addrs, netip.AddrFrom16(r.AAAA).Unmap())
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
