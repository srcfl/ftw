// Package tunnel defines the wire protocol for the relay-as-tunnel
// design (see docs/goals/relay-as-tunnel.md). The relay is a stateless
// request queue; this package serializes one tunneled HTTP request +
// response as JSON bodies with base64'd payloads.
package tunnel

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
)

// TunneledRequest is one tunneled HTTP request the host pulls from the
// relay's long-poll endpoint. Body is raw bytes in memory; on the wire
// it is base64'd so JSON-unsafe bytes (binary MCP payloads, gzip'd
// dashboard responses) survive without escaping.
type TunneledRequest struct {
	ReqID  string      `json:"req_id"`
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Header http.Header `json:"headers,omitempty"`
	Body   []byte      `json:"-"`
}

type tunneledRequestWire struct {
	ReqID   string      `json:"req_id"`
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Header  http.Header `json:"headers,omitempty"`
	BodyB64 string      `json:"body_b64,omitempty"`
}

func (r TunneledRequest) MarshalJSON() ([]byte, error) {
	w := tunneledRequestWire{
		ReqID:  r.ReqID,
		Method: r.Method,
		Path:   r.Path,
		Header: r.Header,
	}
	if len(r.Body) > 0 {
		w.BodyB64 = base64.StdEncoding.EncodeToString(r.Body)
	}
	return json.Marshal(w)
}

func (r *TunneledRequest) UnmarshalJSON(b []byte) error {
	var w tunneledRequestWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	r.ReqID = w.ReqID
	r.Method = w.Method
	r.Path = w.Path
	r.Header = w.Header
	if w.BodyB64 == "" {
		r.Body = nil
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(w.BodyB64)
	if err != nil {
		return err
	}
	r.Body = decoded
	return nil
}

// TunneledResponse is what the host POSTs back per req_id.
type TunneledResponse struct {
	Status int         `json:"status"`
	Header http.Header `json:"headers,omitempty"`
	Body   []byte      `json:"-"`
}

type tunneledResponseWire struct {
	Status  int         `json:"status"`
	Header  http.Header `json:"headers,omitempty"`
	BodyB64 string      `json:"body_b64,omitempty"`
}

func (r TunneledResponse) MarshalJSON() ([]byte, error) {
	w := tunneledResponseWire{
		Status: r.Status,
		Header: r.Header,
	}
	if len(r.Body) > 0 {
		w.BodyB64 = base64.StdEncoding.EncodeToString(r.Body)
	}
	return json.Marshal(w)
}

func (r *TunneledResponse) UnmarshalJSON(b []byte) error {
	var w tunneledResponseWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	r.Status = w.Status
	r.Header = w.Header
	if w.BodyB64 == "" {
		r.Body = nil
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(w.BodyB64)
	if err != nil {
		return err
	}
	r.Body = decoded
	return nil
}
