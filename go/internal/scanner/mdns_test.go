package scanner

import (
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func mustName(t *testing.T, s string) dnsmessage.Name {
	t.Helper()
	n, err := dnsmessage.NewName(s)
	if err != nil {
		t.Fatalf("NewName(%q): %v", s, err)
	}
	return n
}

func TestParsePTRAnswer(t *testing.T) {
	qname := "15.1.168.192.in-addr.arpa."
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Questions: []dnsmessage.Question{{
			Name: mustName(t, qname), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET,
		}},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name: mustName(t, qname), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: 120,
			},
			Body: &dnsmessage.PTRResource{PTR: mustName(t, "nvr.local.")},
		}},
	}
	packed, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack: %v", err)
	}

	if got := parsePTRAnswer(packed, qname); got != "nvr.local" {
		t.Errorf("parsePTRAnswer = %q, want nvr.local", got)
	}
	// Answer for a different reverse name must not match.
	if got := parsePTRAnswer(packed, "99.1.168.192.in-addr.arpa."); got != "" {
		t.Errorf("mismatched qname returned %q, want empty", got)
	}
	// Garbage input is tolerated.
	if got := parsePTRAnswer([]byte{0x01, 0x02, 0x03}, qname); got != "" {
		t.Errorf("garbage returned %q, want empty", got)
	}
}
