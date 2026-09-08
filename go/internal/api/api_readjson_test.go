package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func jsonBodyRequest(t *testing.T, raw []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/api/test", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestReadJSONAcceptsBodyJustUnderCap(t *testing.T) {
	pad := strings.Repeat("a", maxJSONBody)
	var raw []byte
	for {
		raw, _ = json.Marshal(map[string]string{"x": pad})
		if len(raw) < maxJSONBody {
			break
		}
		pad = pad[:len(pad)-1]
		if pad == "" {
			t.Fatal("could not build a JSON body under the cap")
		}
	}
	var got map[string]string
	if err := readJSON(jsonBodyRequest(t, raw), &got); err != nil {
		t.Fatalf("body of %d bytes: %v", len(raw), err)
	}
	if got["x"] != pad {
		t.Fatal("decoded payload did not match")
	}
}

func TestReadJSONRejectsBodyJustOverCap(t *testing.T) {
	raw := append(append([]byte(`{"x":"`), bytes.Repeat([]byte("a"), maxJSONBody)...), `"}`...)
	if len(raw) <= maxJSONBody {
		t.Fatalf("fixture is %d bytes, want over %d", len(raw), maxJSONBody)
	}
	var got map[string]string
	err := readJSON(jsonBodyRequest(t, raw), &got)
	if err == nil {
		t.Fatal("oversized JSON was accepted")
	}
	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) {
		t.Fatalf("got %v (%T), want MaxBytesError", err, err)
	}
	if got != nil {
		t.Fatal("unmarshaled a truncated prefix")
	}
}

func TestReadJSONFailsOnMaxBytesReaderError(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/api/test", strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Body = http.MaxBytesReader(nil, req.Body, 4)
	var got map[string]any
	gotErr := readJSON(req, &got)
	if gotErr == nil {
		t.Fatal("capped reader was accepted")
	}
	var tooBig *http.MaxBytesError
	if !errors.As(gotErr, &tooBig) {
		t.Fatalf("got %v (%T), want MaxBytesError", gotErr, gotErr)
	}
}
