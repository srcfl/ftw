package tunnel

import (
	"strings"
	"testing"
)

func TestDtlsFingerprintSigningString(t *testing.T) {
	s := DtlsFingerprintSigningString("site:Home", "abcd01ef", 1717500000000)
	if want := "ftw-dtls-fp:v1:site:Home:abcd01ef:1717500000000"; s != want {
		t.Fatalf("got %q want %q", s, want)
	}
	if !strings.HasPrefix(s, "ftw-dtls-fp:v1:") {
		t.Fatalf("missing versioned domain prefix: %q", s)
	}
	// Domain separation: the dtls-fp and me-register strings must never coincide
	// for the same inputs — both are signed by the same key, so distinct
	// versioned prefixes are what stop a cross-protocol signature replay.
	if me := MeRegisterSigningString("site:Home", "abcd01ef", 1717500000000); me == s {
		t.Fatal("dtls-fp must differ from the me-register signing string")
	}
}

func TestNormalizeDtlsFingerprint(t *testing.T) {
	if got := NormalizeDtlsFingerprint("AB:CD:01:eF"); got != "abcd01ef" {
		t.Fatalf("colon/upper: got %q want abcd01ef", got)
	}
	if got := NormalizeDtlsFingerprint("abcd01ef"); got != "abcd01ef" {
		t.Fatalf("not idempotent: got %q", got)
	}
	if got := NormalizeDtlsFingerprint("12 : 34 :: ab"); got != "1234ab" {
		t.Fatalf("stray punctuation: got %q want 1234ab", got)
	}
}
