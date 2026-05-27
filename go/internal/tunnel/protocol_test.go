package tunnel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestTunneledRequestRoundtrip(t *testing.T) {
	want := TunneledRequest{
		ReqID:  "8c9b1e2a-de33-4a4f-b5e0-fce21caee98e",
		Method: "POST",
		Path:   "/mcp",
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(`{"method":"initialize"}`),
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got TunneledRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ReqID != want.ReqID || got.Method != want.Method || got.Path != want.Path {
		t.Fatalf("scalar mismatch: %+v vs %+v", got, want)
	}
	if !bytes.Equal(got.Body, want.Body) {
		t.Fatalf("body mismatch: %q vs %q", got.Body, want.Body)
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("header lost: %v", got.Header)
	}
}

func TestTunneledResponseRoundtripWithEmptyBody(t *testing.T) {
	want := TunneledResponse{Status: 204, Header: http.Header{}, Body: nil}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got TunneledResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != 204 {
		t.Fatalf("status: %d", got.Status)
	}
	if len(got.Body) != 0 {
		t.Fatalf("body should be empty: %v", got.Body)
	}
}

func TestTunneledRequestBinarySafe(t *testing.T) {
	// Bytes that would explode in raw-string JSON: 0x00, 0xff, raw quotes,
	// control chars. Base64 must survive all of them.
	body := []byte{0x00, 0x01, 0x02, 0xff, '"', '\\', '\n', 0x7f}
	tr := TunneledRequest{Method: "POST", Path: "/mcp", Body: body}
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got TunneledRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(got.Body, body) {
		t.Fatalf("binary body lost: %v vs %v", got.Body, body)
	}
}
