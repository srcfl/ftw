package scanner

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// mdnsAddr is the IPv4 multicast group + port for mDNS (RFC 6762).
var mdnsAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// reverseMDNS asks the LAN, over multicast mDNS, for the hostname of ip via a
// reverse PTR query (d.c.b.a.in-addr.arpa). Many home-energy devices publish
// no unicast reverse-DNS record but do answer mDNS, so this fills the gap that
// net.LookupAddr leaves empty. Returns "" (no error) when nothing answers.
//
// The query sets the mDNS "unicast response" (QU) bit so the responder replies
// straight to our ephemeral socket — that means we don't have to bind :5353,
// which is already held by the OS mDNS responder on most machines.
func reverseMDNS(ctx context.Context, ip string) string {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return ""
	}
	qname := fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa.", v4[3], v4[2], v4[1], v4[0])
	name, err := dnsmessage.NewName(qname)
	if err != nil {
		return ""
	}

	const qClassUnicastIN = 0x8000 | 0x0001 // QU bit | IN
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypePTR,
			Class: dnsmessage.Class(qClassUnicastIN),
		}},
	}
	packed, err := msg.Pack()
	if err != nil {
		return ""
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return ""
	}
	defer conn.Close()

	deadline := time.Now().Add(900 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.WriteToUDP(packed, mdnsAddr); err != nil {
		return ""
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "" // deadline / read error → give up
		}
		if host := parsePTRAnswer(buf[:n], qname); host != "" {
			return host
		}
	}
}

// parsePTRAnswer walks an mDNS response and returns the first PTR target whose
// question name matches qname, with the trailing dot stripped. "" if none.
func parsePTRAnswer(packet []byte, qname string) string {
	var p dnsmessage.Parser
	if _, err := p.Start(packet); err != nil {
		return ""
	}
	_ = p.SkipAllQuestions()
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			return ""
		}
		if h.Type == dnsmessage.TypePTR && strings.EqualFold(h.Name.String(), qname) {
			ptr, err := p.PTRResource()
			if err != nil {
				return ""
			}
			return strings.TrimSuffix(ptr.PTR.String(), ".")
		}
		if err := p.SkipAnswer(); err != nil {
			return ""
		}
	}
}
