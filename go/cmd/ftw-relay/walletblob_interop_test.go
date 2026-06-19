package main

import (
	"encoding/base64"
	"testing"
)

// TestWalletBlobInteropWithWebClient locks the JS<->Go interop of the blob
// write-auth. The fixture below was produced by the BROWSER crypto in
// web/owner-access/instance-sync.js (Ed25519 sign over the canonical
// blobWriteMessage). If Go's blobWriteMessage ever drifts from the JS one — a
// different field order, delimiter, encoding, or hash — this Ed25519 signature
// stops verifying and the test fails. Regenerate the fixture from the web with the
// one-liner in the commit message if the canonical message format is changed on
// purpose.
func TestWalletBlobInteropWithWebClient(t *testing.T) {
	const (
		fixtureW       = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO_-"
		fixtureVersion = 1
		ctB64          = "aW50ZXJvcC1jaXBoZXJ0ZXh0LWJsb2I="
		nonceB64       = "AQIDBAUGBwgJCgsM"
		pubB64         = "Gmr6dWWprzvyZ20EfEgYFoJJRHGNZRHeM3JFwAlOG8I="
		sigB64         = "Caf5dYXL+GPxGH/Ap7B8ILdo67tf3IHIoKF8O+e90H3kZMsihlhDNJGgMotw/DYcXYGgezyakVm5u6+U3pOiDg=="
	)
	dec := func(s string) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("decode %q: %v", s, err)
		}
		return b
	}
	s := newTestBlobStore(t)
	err := s.Put(fixtureW, dec(ctB64), dec(nonceB64), dec(pubB64), dec(sigB64), fixtureVersion)
	if err != nil {
		t.Fatalf("web-generated signed PUT rejected by the Go store: %v\n"+
			"This means blobWriteMessage differs between web/owner-access/instance-sync.js and go/cmd/ftw-relay/walletblob.go.", err)
	}
}
