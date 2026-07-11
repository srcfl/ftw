package scanner

import (
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func mustDNSName(t *testing.T, value string) dnsmessage.Name {
	t.Helper()
	name, err := dnsmessage.NewName(value)
	if err != nil {
		t.Fatalf("NewName(%q): %v", value, err)
	}
	return name
}

func TestParsePTRAnswer(t *testing.T) {
	qname := "15.1.168.192.in-addr.arpa."
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true},
		Questions: []dnsmessage.Question{{Name: mustDNSName(t, qname), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: mustDNSName(t, qname), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: 120},
			Body:   &dnsmessage.PTRResource{PTR: mustDNSName(t, "inverter.local.")},
		}},
	}
	packet, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if got := parsePTRAnswer(packet, qname); got != "inverter.local" {
		t.Fatalf("parsePTRAnswer = %q, want inverter.local", got)
	}
	if got := parsePTRAnswer(packet, "99.1.168.192.in-addr.arpa."); got != "" {
		t.Fatalf("mismatched answer = %q, want empty", got)
	}
	if got := parsePTRAnswer([]byte{1, 2, 3}, qname); got != "" {
		t.Fatalf("garbage answer = %q, want empty", got)
	}
}
