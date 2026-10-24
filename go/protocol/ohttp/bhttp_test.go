package ohttp

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestBinaryHTTPRoundTripsWithPadding(t *testing.T) {
	req := Request{Method: "POST", Scheme: "https", Authority: "cloud.ftw.energy", Path: "/v1/x",
		Fields: []Field{{"content-type", "application/json"}}, Content: bytes.Repeat([]byte{'a'}, 100)}
	b := req.Marshal(256)
	if len(b) != 256 {
		t.Fatalf("padded to %d", len(b))
	}
	got, err := ParseRequest(b)
	if err != nil || got.Method != req.Method || got.Path != req.Path || len(got.Fields) != 1 || got.Fields[0] != req.Fields[0] || !bytes.Equal(got.Content, req.Content) {
		t.Fatalf("%+v %v", got, err)
	}
	// Content longer than one byte's varint, and no padding asked for.
	long := Request{Method: "POST", Scheme: "https", Authority: "a", Path: "/", Content: make([]byte, 20000)}
	if got, err := ParseRequest(long.Marshal(0)); err != nil || len(got.Content) != 20000 {
		t.Fatalf("long content %d %v", len(got.Content), err)
	}

	resp := Response{Status: 204}.Marshal(16)
	if r, err := ParseResponse(resp); err != nil || r.Status != 204 || len(resp) != 16 {
		t.Fatalf("response %+v %v", r, err)
	}
}

func TestBinaryHTTPRefusesMalformedMessages(t *testing.T) {
	good := Request{Method: "POST", Scheme: "https", Authority: "a", Path: "/", Content: []byte("x")}.Marshal(0)
	withTrailer := append(bytes.Clone(good[:len(good)-1]), 4, 1, 'a', 1, 'b')
	for name, b := range map[string][]byte{
		"empty":         nil,
		"response":      Response{Status: 200}.Marshal(0),
		"indeterminate": {2},
		"cut control":   good[:4],
		"cut content":   good[:len(good)-2],
		"bad padding":   append(bytes.Clone(good), 0, 1),
		"trailers":      withTrailer,
		"long length":   {0, 0x3f},
	} {
		if _, err := ParseRequest(b); err == nil {
			t.Fatalf("%s parsed", name)
		}
	}
	for name, b := range map[string][]byte{
		"informational": unhexB("014064"),
		"status 600":    unhexB("014258"),
		"request":       good,
	} {
		if _, err := ParseResponse(b); err == nil {
			t.Fatalf("%s parsed", name)
		}
	}
}

func unhexB(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}
