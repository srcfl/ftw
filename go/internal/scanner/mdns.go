package scanner

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

var mdnsAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// reverseMDNS sends a reverse PTR query with the QU bit set so devices reply
// directly to our ephemeral socket instead of requiring a bind to port 5353.
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
	msg := dnsmessage.Message{Questions: []dnsmessage.Question{{
		Name: name, Type: dnsmessage.TypePTR, Class: dnsmessage.Class(0x8001),
	}}}
	packed, err := msg.Pack()
	if err != nil {
		return ""
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
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
			return ""
		}
		if host := parsePTRAnswer(buf[:n], qname); host != "" {
			return host
		}
	}
}

func parsePTRAnswer(packet []byte, qname string) string {
	var p dnsmessage.Parser
	if _, err := p.Start(packet); err != nil {
		return ""
	}
	if err := p.SkipAllQuestions(); err != nil {
		return ""
	}
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
