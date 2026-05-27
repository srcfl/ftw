# Relay-as-tunnel Phase 1+2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `ftw-connect` binary + raw-TCP subetha relay with `relay.fortytwowatts.com` HTTPS request-response tunnel that carries both MCP (Phase 1) and web reverse-proxy (Phase 2), end-to-end testable on localhost without TLS.

**Architecture:** Two endpoints on the relay (`/tunnel/<host-id>/next` long-poll + `/tunnel/<host-id>/response/<req-id>` POST) form the byte pipe; public-facing `/h/<token>/{,mcp,web/...}` paths route through the queue. Token state machine: pending → active (after 4-digit voice-channel approval) → expired/revoked. Host-side `ftw-pair` runs a long-poll loop dispatching to local backends (MCP on :9999, web on :8080).

**Tech stack:** Go 1.26, stdlib `net/http`, `net/http/httputil` for reverse proxy, `github.com/google/uuid` for request-ids (already in go.mod), existing `ftw-pair` MCP server unchanged.

**Skipped for v1 (follow-ups):** passkey/owner remote access (Phase 3), dashboard UI integration (separate PR), HSTS preload submission (operator action). TLS termination on the relay binary is implemented (`-cert`/`-key` flags) but not enabled in tests — operator sets it up per `docs/relay-deploy.md`.

---

## File structure

**New packages and files:**

| Path | Responsibility |
|---|---|
| `go/internal/tunnel/protocol.go` | Shared wire types: `TunneledRequest`, `TunneledResponse`, `RequestID`, JSON serialization |
| `go/internal/tunnel/protocol_test.go` | Serialization roundtrip + edge cases |
| `go/internal/tunnel/queue.go` | Per-host request queue + response routing (used inside the relay process) |
| `go/internal/tunnel/queue_test.go` | Queue concurrency + timeout tests |
| `go/internal/tunnel/host.go` | Host-side long-poll loop client |
| `go/internal/tunnel/host_test.go` | Host-loop tests against `httptest` relay |
| `go/cmd/ftw-relay/main.go` | HTTP(S) server, mux, flag parsing |
| `go/cmd/ftw-relay/tokens.go` | Token state machine (pending → active → expired/revoked) + TTL reaper |
| `go/cmd/ftw-relay/tokens_test.go` | State machine tests |
| `go/cmd/ftw-relay/handlers.go` | HTTP handlers for `/tunnel/*` and `/h/*` paths |
| `go/cmd/ftw-relay/handlers_test.go` | `httptest`-based handler tests |
| `go/cmd/ftw-relay/e2e_test.go` | All-in-process e2e: relay + host + simulated friend |

**Modified:**

| Path | Change |
|---|---|
| `go/cmd/ftw-pair/subetha.go` | Replace with `tunnel.go` calling `tunnel.NewHost(...)` |
| `go/cmd/ftw-pair/main.go` | Replace relay-addr flag (TCP-style) with `-relay-url http://...` (or HTTPS) |
| `go/cmd/ftw-pair/session.go` | Register session with relay (POST `/tunnel/register`), parse returned token + URL |
| `go/cmd/ftw-pair/subetha_test.go` | Replace with `tunnel_test.go` using `httptest` relay |
| `.github/workflows/release.yml` | Add `ftw-relay-linux-{amd64,arm64}` build matrix, remove `ftw-connect-*` + `ftw-subetha-*` entries |
| `Makefile` | Add `ftw-relay` to build targets (if listed) |
| `docs/ftw-pair.md` | Replace "install ftw-connect binary" section with "open this URL" |
| `docs/relay-deploy.md` | Add `wget ftw-relay-linux-amd64` URL once first release is cut |

**Deleted (last task):**

- `go/cmd/ftw-connect/` (entire dir)
- `go/cmd/ftw-subetha/` (entire dir)
- `go/internal/subetha/` (entire dir)
- `scripts/install-ftw-connect.sh`
- `docs/subetha-deploy.md`

---

## Wire protocol (lock these now)

### Host-side long-poll endpoints (private)

**`GET /tunnel/<host-id>/next?since=<unix-ms>`**

Long-polls up to 30 s for a queued request. Response body is JSON:

```json
{
  "req_id": "8c9b1e2a-...",
  "method": "POST",
  "path": "/mcp",
  "headers": {"Content-Type": ["application/json"]},
  "body_b64": "eyJtZXRob2QiLi4u",
  "started_at_ms": 1746000000123
}
```

Returns `204 No Content` if no request arrives within 30 s (host should immediately re-poll).

**`POST /tunnel/<host-id>/response/<req-id>`**

Body is JSON:

```json
{
  "status": 200,
  "headers": {"Content-Type": ["application/json"]},
  "body_b64": "eyJyZXN1bHQiLi4u"
}
```

Returns `204 No Content` on success, `404` if `req-id` isn't pending.

**`POST /tunnel/register`**

Body: `{"host_id":"...","token":"six-word-token","ttl_ms":3600000,"approval_code":"4827","intent":"help write a driver","as":"@erikarenhill"}`.

Returns `200 OK` with `{"public_url":"http://relay/h/<token>","approval_url":"/h/<token>/approve"}`.

### Public-facing endpoints (friend)

**`GET /h/<token>`** — landing page (HTML) showing the 4-digit approval code, claimed intent + as, polling for approval state.

**`POST /h/<token>/approve`** — submit `{"code":"4827"}`. (For v1 the host posts this via dashboard later; for e2e tests we expose this directly with `?test_skip_approval=1` query param when the relay was started with `-test-mode`.)

**`POST /h/<token>/mcp` and `GET /h/<token>/mcp`** — MCP HTTP streamable transport (request-response). Active tokens only; pending → `425 Too Early`.

**`* /h/<token>/web/*`** — transparent reverse-proxy through tunnel. Body and headers preserved. Active tokens only.

**`GET /healthz`** — `200 OK` with `OK\n` body.

---

## Task 1: Protocol types and serialization

**Files:**
- Create: `go/internal/tunnel/protocol.go`
- Create: `go/internal/tunnel/protocol_test.go`

- [ ] **Step 1: Write the failing test**

```go
// go/internal/tunnel/protocol_test.go
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
```

- [ ] **Step 2: Implement**

```go
// go/internal/tunnel/protocol.go
// Package tunnel defines the wire protocol for the relay-as-tunnel
// design (see docs/goals/relay-as-tunnel.md). The relay is a stateless
// request queue; this package serializes the tunneled HTTP request +
// response over plain JSON bodies.
package tunnel

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
)

// TunneledRequest is one tunneled HTTP request the host pulls from the
// relay's long-poll endpoint. The body is base64-encoded on the wire so
// JSON-unsafe bytes (binary MCP payloads, SSE frames in degraded mode)
// survive without escaping.
type TunneledRequest struct {
	ReqID  string      `json:"req_id"`
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Header http.Header `json:"headers,omitempty"`
	Body   []byte      `json:"-"`
	// BodyB64 is the wire form of Body. Populated by MarshalJSON, drained
	// by UnmarshalJSON. Callers should use Body.
	BodyB64 string `json:"body_b64,omitempty"`
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
	if w.BodyB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(w.BodyB64)
		if err != nil {
			return err
		}
		r.Body = decoded
	}
	return nil
}

// TunneledResponse is what the host POSTs back per req_id.
type TunneledResponse struct {
	Status  int         `json:"status"`
	Header  http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"-"`
	BodyB64 string      `json:"body_b64,omitempty"`
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
	if w.BodyB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(w.BodyB64)
		if err != nil {
			return err
		}
		r.Body = decoded
	}
	return nil
}
```

- [ ] **Step 3: Verify**

```bash
cd go && go test ./internal/tunnel/ -run TestTunneled -v
```
Expected: both tests PASS.

- [ ] **Step 4: Commit**

```bash
git add go/internal/tunnel/
git commit -m "feat(tunnel): wire protocol for request-response relay tunnel"
```

---

## Task 2: Per-host request queue

**Files:**
- Create: `go/internal/tunnel/queue.go`
- Create: `go/internal/tunnel/queue_test.go`

- [ ] **Step 1: Test — enqueue + poll roundtrip**

```go
// go/internal/tunnel/queue_test.go
package tunnel

import (
	"context"
	"testing"
	"time"
)

func TestQueueEnqueuePollResponse(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// In one goroutine, enqueue and wait for response.
	respCh := make(chan TunneledResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		req := TunneledRequest{Method: "GET", Path: "/mcp"}
		resp, err := q.Enqueue(ctx, "host-a", req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	// Poller picks up the request.
	got, err := q.Poll(ctx, "host-a", 1*time.Second)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.Method != "GET" || got.Path != "/mcp" {
		t.Fatalf("wrong request: %+v", got)
	}
	if got.ReqID == "" {
		t.Fatalf("queue must assign a req_id")
	}

	// Post response back.
	if err := q.PostResponse(got.ReqID, TunneledResponse{Status: 200, Body: []byte("hello")}); err != nil {
		t.Fatalf("post: %v", err)
	}

	select {
	case resp := <-respCh:
		if resp.Status != 200 || string(resp.Body) != "hello" {
			t.Fatalf("wrong response: %+v", resp)
		}
	case err := <-errCh:
		t.Fatalf("enqueue err: %v", err)
	case <-ctx.Done():
		t.Fatal("response never arrived")
	}
}

func TestQueuePollTimesOut(t *testing.T) {
	q := NewQueue()
	ctx := context.Background()
	_, err := q.Poll(ctx, "host-empty", 100*time.Millisecond)
	if err != ErrPollTimeout {
		t.Fatalf("want ErrPollTimeout, got %v", err)
	}
}

func TestQueuePostResponseUnknownIDFails(t *testing.T) {
	q := NewQueue()
	err := q.PostResponse("nonexistent-id", TunneledResponse{Status: 200})
	if err != ErrUnknownReqID {
		t.Fatalf("want ErrUnknownReqID, got %v", err)
	}
}

func TestQueueEnqueueRespectsContextCancel(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := q.Enqueue(ctx, "host-x", TunneledRequest{Method: "GET", Path: "/x"})
		errCh <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("enqueue did not return after cancel")
	}
}
```

- [ ] **Step 2: Implement**

```go
// go/internal/tunnel/queue.go
package tunnel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrPollTimeout — no request available within the poll deadline.
	// Hosts should re-poll immediately.
	ErrPollTimeout = errors.New("tunnel: poll timeout")
	// ErrUnknownReqID — the host POSTed a response for a request that
	// the queue doesn't know about (already responded, expired, or
	// fabricated). Indicates a host bug or a stale relay restart.
	ErrUnknownReqID = errors.New("tunnel: unknown req_id")
)

// Queue is the relay-internal request queue. One Queue per relay
// process; it indexes pending requests per host-id.
type Queue struct {
	mu      sync.Mutex
	pending map[string][]queuedRequest         // host-id → FIFO
	waiters map[string][]chan queuedRequest    // host-id → waiting pollers
	inflight map[string]chan TunneledResponse  // req_id → response sink
}

type queuedRequest struct {
	req TunneledRequest
}

// NewQueue returns an initialized Queue.
func NewQueue() *Queue {
	return &Queue{
		pending:  make(map[string][]queuedRequest),
		waiters:  make(map[string][]chan queuedRequest),
		inflight: make(map[string]chan TunneledResponse),
	}
}

// Enqueue submits a request for the given host and blocks until the
// host POSTs a response, the context is cancelled, or the request is
// purged. The req field's ReqID is overwritten with a fresh UUID.
func (q *Queue) Enqueue(ctx context.Context, hostID string, req TunneledRequest) (TunneledResponse, error) {
	req.ReqID = uuid.NewString()
	respCh := make(chan TunneledResponse, 1)

	q.mu.Lock()
	q.inflight[req.ReqID] = respCh
	// If a poller is already waiting, hand the request to them directly.
	if waiters := q.waiters[hostID]; len(waiters) > 0 {
		next := waiters[0]
		q.waiters[hostID] = waiters[1:]
		q.mu.Unlock()
		next <- queuedRequest{req: req}
	} else {
		q.pending[hostID] = append(q.pending[hostID], queuedRequest{req: req})
		q.mu.Unlock()
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		q.mu.Lock()
		delete(q.inflight, req.ReqID)
		q.mu.Unlock()
		return TunneledResponse{}, ctx.Err()
	}
}

// Poll long-polls for the next pending request for hostID, or returns
// ErrPollTimeout after the timeout. The returned request carries an
// assigned ReqID that the host must use in PostResponse.
func (q *Queue) Poll(ctx context.Context, hostID string, timeout time.Duration) (TunneledRequest, error) {
	q.mu.Lock()
	if pending := q.pending[hostID]; len(pending) > 0 {
		next := pending[0]
		q.pending[hostID] = pending[1:]
		q.mu.Unlock()
		return next.req, nil
	}
	wait := make(chan queuedRequest, 1)
	q.waiters[hostID] = append(q.waiters[hostID], wait)
	q.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case q := <-wait:
		return q.req, nil
	case <-timer.C:
		// Try to take ourselves off the waiters list. If a producer is
		// mid-handoff we'll already have received via wait; check that.
		select {
		case q := <-wait:
			return q.req, nil
		default:
			return TunneledRequest{}, ErrPollTimeout
		}
	case <-ctx.Done():
		return TunneledRequest{}, ctx.Err()
	}
}

// PostResponse delivers a host's response to the waiting Enqueue call.
// Returns ErrUnknownReqID if the req_id isn't pending.
func (q *Queue) PostResponse(reqID string, resp TunneledResponse) error {
	q.mu.Lock()
	respCh, ok := q.inflight[reqID]
	if ok {
		delete(q.inflight, reqID)
	}
	q.mu.Unlock()
	if !ok {
		return ErrUnknownReqID
	}
	respCh <- resp
	return nil
}
```

- [ ] **Step 3: Verify**

```bash
cd go && go test ./internal/tunnel/ -v -run Queue
```
Expected: all 4 queue tests PASS.

- [ ] **Step 4: Commit**

```bash
git add go/internal/tunnel/queue.go go/internal/tunnel/queue_test.go
git commit -m "feat(tunnel): per-host request queue with long-poll + response routing"
```

---

## Task 3: Host-side long-poll client

**Files:**
- Create: `go/internal/tunnel/host.go`
- Create: `go/internal/tunnel/host_test.go`

- [ ] **Step 1: Test using httptest relay**

```go
// go/internal/tunnel/host_test.go
package tunnel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostLoopDispatchesToHandler(t *testing.T) {
	q := NewQueue()
	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel/host-a/next", func(w http.ResponseWriter, r *http.Request) {
		req, err := q.Poll(r.Context(), "host-a", 5*time.Second)
		if err == ErrPollTimeout {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = writeJSON(w, req)
	})
	mux.HandleFunc("/tunnel/host-a/response/", func(w http.ResponseWriter, r *http.Request) {
		reqID := strings.TrimPrefix(r.URL.Path, "/tunnel/host-a/response/")
		var resp TunneledResponse
		if err := readJSON(r.Body, &resp); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := q.PostResponse(reqID, resp); err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		w.WriteHeader(204)
	})

	relay := httptest.NewServer(mux)
	defer relay.Close()

	var handled atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		w.Header().Set("X-Echo", "yes")
		_, _ = w.Write([]byte("pong:" + r.URL.Path))
	})

	host := NewHost(relay.URL, "host-a", handler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

	// Enqueue a synthetic friend request; host loop should dispatch it.
	resp, err := q.Enqueue(context.Background(), "host-a", TunneledRequest{
		Method: "GET",
		Path:   "/mcp",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if resp.Status != 200 || !strings.Contains(string(resp.Body), "pong:/mcp") {
		t.Fatalf("bad response: %+v body=%q", resp, resp.Body)
	}
	if resp.Header.Get("X-Echo") != "yes" {
		t.Fatalf("header lost: %v", resp.Header)
	}
	if handled.Load() != 1 {
		t.Fatalf("handler called %d times", handled.Load())
	}
}
```

- [ ] **Step 2: Implement (host.go) + tiny json helpers**

```go
// go/internal/tunnel/host.go
package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"
)

// Host runs a long-poll loop against a relay, dispatching tunneled
// requests to the supplied http.Handler.
type Host struct {
	relayURL string
	hostID   string
	handler  http.Handler
	client   *http.Client
	// PollTimeout is how long each GET /next blocks. Relay-side default
	// is 30s; we ask for slightly less to avoid races. Tests can shrink.
	PollTimeout time.Duration
}

// NewHost constructs a Host. relayURL is the base URL (no trailing
// slash). handler is the local backend that receives tunneled requests.
func NewHost(relayURL, hostID string, handler http.Handler) *Host {
	return &Host{
		relayURL:    relayURL,
		hostID:      hostID,
		handler:     handler,
		client:      &http.Client{Timeout: 35 * time.Second},
		PollTimeout: 25 * time.Second,
	}
}

// Run blocks until ctx is cancelled. Errors are logged via slog and the
// loop continues; transient relay outages should not stop the host.
func (h *Host) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := h.pollOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("tunnel poll failed", "err", err, "host_id", h.hostID)
			// Backoff to avoid hot-spinning on a broken relay.
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (h *Host) pollOnce(ctx context.Context) error {
	url := fmt.Sprintf("%s/tunnel/%s/next", h.relayURL, h.hostID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil // no request available, loop again
	case http.StatusOK:
		var tr TunneledRequest
		if err := readJSON(resp.Body, &tr); err != nil {
			return fmt.Errorf("decode tunneled request: %w", err)
		}
		go h.handle(ctx, tr)
		return nil
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("relay returned %d: %s", resp.StatusCode, body)
	}
}

func (h *Host) handle(ctx context.Context, tr TunneledRequest) {
	inner, err := http.NewRequestWithContext(ctx, tr.Method, tr.Path, bytes.NewReader(tr.Body))
	if err != nil {
		h.postError(ctx, tr.ReqID, 500, err)
		return
	}
	for k, vv := range tr.Header {
		inner.Header[k] = vv
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, inner)
	resp := TunneledResponse{
		Status: rec.Code,
		Header: rec.Result().Header,
		Body:   rec.Body.Bytes(),
	}
	h.postResponse(ctx, tr.ReqID, resp)
}

func (h *Host) postResponse(ctx context.Context, reqID string, resp TunneledResponse) {
	url := fmt.Sprintf("%s/tunnel/%s/response/%s", h.relayURL, h.hostID, reqID)
	body, err := json.Marshal(resp)
	if err != nil {
		slog.Error("tunnel marshal response", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Error("tunnel build post", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := h.client.Do(req)
	if err != nil {
		slog.Error("tunnel post response", "err", err)
		return
	}
	r.Body.Close()
}

func (h *Host) postError(ctx context.Context, reqID string, status int, err error) {
	h.postResponse(ctx, reqID, TunneledResponse{
		Status: status,
		Header: http.Header{"Content-Type": []string{"text/plain"}},
		Body:   []byte(err.Error()),
	})
}

// json helpers — shared with the queue's HTTP wiring.
func writeJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}
func readJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
```

- [ ] **Step 3: Verify**

```bash
cd go && go test ./internal/tunnel/ -v -run Host
```
Expected: PASS within 1s.

- [ ] **Step 4: Commit**

```bash
git add go/internal/tunnel/host.go go/internal/tunnel/host_test.go
git commit -m "feat(tunnel): host-side long-poll client dispatching to local http.Handler"
```

---

## Task 4: Token state machine

**Files:**
- Create: `go/cmd/ftw-relay/tokens.go`
- Create: `go/cmd/ftw-relay/tokens_test.go`

- [ ] **Step 1: Tests covering all transitions**

```go
// go/cmd/ftw-relay/tokens_test.go
package main

import (
	"testing"
	"time"
)

func TestTokenRegisterStartsPending(t *testing.T) {
	r := NewTokenRegistry()
	tok, err := r.Register(TokenRegistration{
		HostID:       "host-a",
		Token:        "garage-coffee-river-bicycle-window-cat",
		TTL:          1 * time.Hour,
		ApprovalCode: "4827",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if tok.State() != TokenPending {
		t.Fatalf("expected pending, got %v", tok.State())
	}
	if tok.ApprovalCode() != "4827" {
		t.Fatalf("approval code lost")
	}
}

func TestTokenApproveActivates(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: time.Hour, ApprovalCode: "1234"})
	if err := r.Approve("tok1", "1234"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	tok, _ := r.Get("tok1")
	if tok.State() != TokenActive {
		t.Fatalf("expected active, got %v", tok.State())
	}
}

func TestTokenApproveWrongCodeFails(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: time.Hour, ApprovalCode: "1234"})
	if err := r.Approve("tok1", "9999"); err != ErrBadApprovalCode {
		t.Fatalf("want ErrBadApprovalCode, got %v", err)
	}
	tok, _ := r.Get("tok1")
	if tok.State() != TokenPending {
		t.Fatalf("wrong code must not activate")
	}
}

func TestTokenExpiresOnTTL(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: 30 * time.Millisecond, ApprovalCode: "1234"})
	time.Sleep(80 * time.Millisecond)
	tok, _ := r.Get("tok1")
	if tok.State() != TokenExpired {
		t.Fatalf("expected expired, got %v", tok.State())
	}
}

func TestTokenRevokeImmediate(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: time.Hour, ApprovalCode: "1234"})
	_ = r.Approve("tok1", "1234")
	r.Revoke("tok1")
	tok, _ := r.Get("tok1")
	if tok.State() != TokenRevoked {
		t.Fatalf("expected revoked, got %v", tok.State())
	}
}

func TestTokenRegisterRejectsDuplicate(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "dup", TTL: time.Hour, ApprovalCode: "1"})
	if _, err := r.Register(TokenRegistration{HostID: "host-b", Token: "dup", TTL: time.Hour, ApprovalCode: "2"}); err != ErrTokenExists {
		t.Fatalf("want ErrTokenExists, got %v", err)
	}
}

func TestTokenApprovalRateLimit(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: time.Hour, ApprovalCode: "1234"})
	for i := 0; i < MaxApprovalAttempts; i++ {
		if err := r.Approve("tok1", "9999"); err != ErrBadApprovalCode {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Next attempt — even with correct code — must fail.
	if err := r.Approve("tok1", "1234"); err != ErrApprovalLocked {
		t.Fatalf("want ErrApprovalLocked, got %v", err)
	}
}
```

- [ ] **Step 2: Implement**

```go
// go/cmd/ftw-relay/tokens.go
package main

import (
	"errors"
	"sync"
	"time"
)

// MaxApprovalAttempts gates the 4-digit code matching. After this many
// wrong attempts on a single token, even the correct code is rejected
// for the rest of the TTL. 10⁴ codes / 5 attempts ≈ 1-in-2000 spam
// success rate before the operator notices.
const MaxApprovalAttempts = 5

var (
	ErrTokenExists       = errors.New("token already registered")
	ErrTokenNotFound     = errors.New("token not found")
	ErrTokenNotPending   = errors.New("token not in pending state")
	ErrBadApprovalCode   = errors.New("bad approval code")
	ErrApprovalLocked    = errors.New("approval locked after too many bad attempts")
)

type TokenState int

const (
	TokenPending TokenState = iota
	TokenActive
	TokenExpired
	TokenRevoked
)

func (s TokenState) String() string {
	switch s {
	case TokenPending:
		return "pending"
	case TokenActive:
		return "active"
	case TokenExpired:
		return "expired"
	case TokenRevoked:
		return "revoked"
	}
	return "unknown"
}

type TokenRegistration struct {
	HostID       string
	Token        string
	TTL          time.Duration
	ApprovalCode string
	Intent       string
	As           string
}

type Token struct {
	mu               sync.Mutex
	hostID           string
	token            string
	approvalCode     string
	intent, as       string
	createdAt        time.Time
	expiresAt        time.Time
	state            TokenState
	approvalAttempts int
}

func (t *Token) State() TokenState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == TokenPending || t.state == TokenActive {
		if time.Now().After(t.expiresAt) {
			t.state = TokenExpired
		}
	}
	return t.state
}

func (t *Token) HostID() string       { return t.hostID }
func (t *Token) ApprovalCode() string { return t.approvalCode }
func (t *Token) Intent() string       { return t.intent }
func (t *Token) As() string           { return t.as }
func (t *Token) ExpiresAt() time.Time { return t.expiresAt }

type TokenRegistry struct {
	mu     sync.Mutex
	tokens map[string]*Token
}

func NewTokenRegistry() *TokenRegistry {
	return &TokenRegistry{tokens: make(map[string]*Token)}
}

func (r *TokenRegistry) Register(reg TokenRegistration) (*Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tokens[reg.Token]; ok {
		return nil, ErrTokenExists
	}
	t := &Token{
		hostID:       reg.HostID,
		token:        reg.Token,
		approvalCode: reg.ApprovalCode,
		intent:       reg.Intent,
		as:           reg.As,
		createdAt:    time.Now(),
		expiresAt:    time.Now().Add(reg.TTL),
		state:        TokenPending,
	}
	r.tokens[reg.Token] = t
	return t, nil
}

func (r *TokenRegistry) Get(token string) (*Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[token]
	if !ok {
		return nil, ErrTokenNotFound
	}
	return t, nil
}

func (r *TokenRegistry) Approve(token, code string) error {
	t, err := r.Get(token)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != TokenPending {
		return ErrTokenNotPending
	}
	if t.approvalAttempts >= MaxApprovalAttempts {
		return ErrApprovalLocked
	}
	if code != t.approvalCode {
		t.approvalAttempts++
		return ErrBadApprovalCode
	}
	t.state = TokenActive
	return nil
}

func (r *TokenRegistry) Revoke(token string) {
	t, err := r.Get(token)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = TokenRevoked
}
```

- [ ] **Step 3: Verify**

```bash
cd go && go test ./cmd/ftw-relay/ -v -run Token
```
Expected: all 7 PASS.

- [ ] **Step 4: Commit**

```bash
git add go/cmd/ftw-relay/tokens.go go/cmd/ftw-relay/tokens_test.go
git commit -m "feat(relay): token state machine with TTL, approval, rate-limiting"
```

---

## Task 5: Relay HTTP handlers — /healthz + /tunnel/*

**Files:**
- Create: `go/cmd/ftw-relay/handlers.go`
- Create: `go/cmd/ftw-relay/handlers_test.go`

- [ ] **Step 1: Test healthz + the long-poll wiring**

```go
// go/cmd/ftw-relay/handlers_test.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

func newTestRelay() *Relay {
	return &Relay{
		Queue:  tunnel.NewQueue(),
		Tokens: NewTokenRegistry(),
	}
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(newTestRelay().Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestTunnelNextLongPollsAndDelivers(t *testing.T) {
	r := newTestRelay()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	// Enqueue a request asynchronously.
	respCh := make(chan tunnel.TunneledResponse, 1)
	go func() {
		resp, err := r.Queue.Enqueue(context.Background(), "host-a", tunnel.TunneledRequest{Method: "GET", Path: "/mcp"})
		if err == nil {
			respCh <- resp
		}
	}()

	// Poll picks it up.
	resp, err := http.Get(srv.URL + "/tunnel/host-a/next")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var tr tunnel.TunneledRequest
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if tr.Path != "/mcp" {
		t.Fatalf("wrong path: %q", tr.Path)
	}

	// Post response back.
	body, _ := json.Marshal(tunnel.TunneledResponse{Status: 200, Body: []byte("ok")})
	postResp, err := http.Post(srv.URL+"/tunnel/host-a/response/"+tr.ReqID, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if postResp.StatusCode != 204 {
		t.Fatalf("post status %d", postResp.StatusCode)
	}

	select {
	case got := <-respCh:
		if got.Status != 200 || string(got.Body) != "ok" {
			t.Fatalf("wrong response: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("response never made it back to enqueuer")
	}
}

func TestTunnelNextTimesOutWith204(t *testing.T) {
	r := newTestRelay()
	r.PollTimeout = 50 * time.Millisecond
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/tunnel/host-empty/next")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestRegisterEndpoint(t *testing.T) {
	r := newTestRelay()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body := strings.NewReader(`{"host_id":"host-a","token":"tok1","ttl_ms":3600000,"approval_code":"4827","intent":"help","as":"@erik"}`)
	resp, err := http.Post(srv.URL+"/tunnel/register", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.PublicURL, "/h/tok1") {
		t.Fatalf("bad public_url: %q", out.PublicURL)
	}
}
```

- [ ] **Step 2: Implement**

```go
// go/cmd/ftw-relay/handlers.go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// Relay is the in-memory state for one running relay process.
type Relay struct {
	Queue       *tunnel.Queue
	Tokens      *TokenRegistry
	PollTimeout time.Duration // 0 → 25s default
}

type registerRequest struct {
	HostID       string `json:"host_id"`
	Token        string `json:"token"`
	TTLMs        int64  `json:"ttl_ms"`
	ApprovalCode string `json:"approval_code"`
	Intent       string `json:"intent,omitempty"`
	As           string `json:"as,omitempty"`
}

type registerResponse struct {
	PublicURL   string `json:"public_url"`
	ApprovalURL string `json:"approval_url"`
}

// Handler returns the mux for this relay.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", r.healthz)
	mux.HandleFunc("GET /tunnel/{host_id}/next", r.tunnelNext)
	mux.HandleFunc("POST /tunnel/{host_id}/response/{req_id}", r.tunnelResponse)
	mux.HandleFunc("POST /tunnel/register", r.tunnelRegister)
	mux.HandleFunc("/h/{token}/mcp", r.publicMCP)
	mux.HandleFunc("POST /h/{token}/approve", r.publicApprove)
	mux.HandleFunc("GET /h/{token}", r.publicLanding)
	mux.HandleFunc("/h/{token}/web/", r.publicWeb)
	return mux
}

func (r *Relay) pollTimeout() time.Duration {
	if r.PollTimeout > 0 {
		return r.PollTimeout
	}
	return 25 * time.Second
}

func (r *Relay) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Write([]byte("OK\n"))
}

func (r *Relay) tunnelNext(w http.ResponseWriter, req *http.Request) {
	hostID := req.PathValue("host_id")
	tr, err := r.Queue.Poll(req.Context(), hostID, r.pollTimeout())
	if errors.Is(err, tunnel.ErrPollTimeout) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tr)
}

func (r *Relay) tunnelResponse(w http.ResponseWriter, req *http.Request) {
	reqID := req.PathValue("req_id")
	var resp tunnel.TunneledResponse
	if err := json.NewDecoder(req.Body).Decode(&resp); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.Queue.PostResponse(reqID, resp); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Relay) tunnelRegister(w http.ResponseWriter, req *http.Request) {
	var reg registerRequest
	if err := json.NewDecoder(req.Body).Decode(&reg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err := r.Tokens.Register(TokenRegistration{
		HostID:       reg.HostID,
		Token:        reg.Token,
		TTL:          time.Duration(reg.TTLMs) * time.Millisecond,
		ApprovalCode: reg.ApprovalCode,
		Intent:       reg.Intent,
		As:           reg.As,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(registerResponse{
		PublicURL:   fmt.Sprintf("/h/%s", reg.Token),
		ApprovalURL: fmt.Sprintf("/h/%s/approve", reg.Token),
	})
}

func (r *Relay) publicLanding(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	t, err := r.Tokens.Get(tok)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, landingHTML, t.As(), t.Intent(), t.ApprovalCode(), t.State())
}

const landingHTML = `<!doctype html>
<html><head><title>forty-two-watts pair session</title>
<style>body{font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222}
code{background:#f4f4f4;padding:.2rem .4rem;border-radius:.2rem}
.code{font-size:3rem;letter-spacing:.3em;text-align:center;padding:2rem;background:#fffbea;border:1px solid #f0c040;border-radius:.5rem;margin:2rem 0}</style>
</head><body>
<h1>Connect to a forty-two-watts instance</h1>
<p>Identity: <code>%s</code></p>
<p>Intent: %s</p>
<p>Tell the host this code over voice (phone, Signal call, etc.):</p>
<div class="code">%s</div>
<p>State: <code>%s</code>. This page will not refresh automatically.</p>
</body></html>`

func (r *Relay) publicApprove(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.Tokens.Approve(tok, body.Code); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Relay) publicMCP(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	r.tunnelPublic(w, req, tok, "/mcp")
}

func (r *Relay) publicWeb(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	stripped := strings.TrimPrefix(req.URL.Path, "/h/"+tok+"/web")
	if stripped == "" {
		stripped = "/"
	}
	r.tunnelPublic(w, req, tok, stripped)
}

func (r *Relay) tunnelPublic(w http.ResponseWriter, req *http.Request, tok, innerPath string) {
	t, err := r.Tokens.Get(tok)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	switch t.State() {
	case TokenPending:
		http.Error(w, "session pending host approval", http.StatusTooEarly)
		return
	case TokenExpired, TokenRevoked:
		http.Error(w, "session expired", http.StatusGone)
		return
	}
	body, _ := readAll(req.Body)
	resp, err := r.Queue.Enqueue(req.Context(), t.HostID(), tunnel.TunneledRequest{
		Method: req.Method,
		Path:   innerPath,
		Header: req.Header,
		Body:   body,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	w.WriteHeader(resp.Status)
	w.Write(resp.Body)
}
```

Add `readAll` helper at the bottom of the file:

```go
import "io"

func readAll(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	return io.ReadAll(r)
}
```

- [ ] **Step 3: Verify**

```bash
cd go && go test ./cmd/ftw-relay/ -v
```
Expected: all handler tests PASS.

- [ ] **Step 4: Commit**

```bash
git add go/cmd/ftw-relay/handlers.go go/cmd/ftw-relay/handlers_test.go
git commit -m "feat(relay): HTTP handlers for tunnel and public token routes"
```

---

## Task 6: Relay binary + main entry point

**Files:**
- Create: `go/cmd/ftw-relay/main.go`

- [ ] **Step 1: Test the http+https flag wiring**

Add to `handlers_test.go`:

```go
func TestRelayMainServesHTTPWhenNoCertSpecified(t *testing.T) {
	// We don't actually start `main`; we just exercise the run loop with
	// httptest-style flags. This test is a smoke check that the binary
	// builds and the Run() helper composes the relay correctly.
	r := &Relay{Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry()}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Implement main**

```go
// go/cmd/ftw-relay/main.go
// ftw-relay — HTTPS request-response tunnel for relay.fortytwowatts.com.
//
// See docs/goals/relay-as-tunnel.md for the design and docs/relay-deploy.md
// for operator setup (Cloudflare Origin Cert + systemd).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

var Version = "dev"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	addr := flag.String("addr", ":7378", "listen address")
	cert := flag.String("cert", "", "TLS cert path (HTTPS mode if set)")
	key := flag.String("key", "", "TLS key path (HTTPS mode if set)")
	pollTimeout := flag.Duration("poll-timeout", 25*time.Second, "long-poll deadline per /tunnel/<host>/next call")
	flag.Parse()

	if *version {
		fmt.Println(Version)
		return
	}

	r := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		PollTimeout: *pollTimeout,
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           r.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	mode := "HTTP"
	var err error
	if *cert != "" && *key != "" {
		mode = "HTTPS"
		err = srv.ListenAndServeTLS(*cert, *key)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		slog.Error("ftw-relay server", "mode", mode, "err", err)
		os.Exit(1)
	}
	slog.Info("ftw-relay shut down cleanly", "mode", mode, "addr", *addr)
}
```

- [ ] **Step 3: Verify**

```bash
cd go && go build ./cmd/ftw-relay && go test ./cmd/ftw-relay/ -v && rm ftw-relay
```
Expected: build OK, tests PASS.

- [ ] **Step 4: Commit**

```bash
git add go/cmd/ftw-relay/main.go go/cmd/ftw-relay/handlers_test.go
git commit -m "feat(relay): main entry point with HTTP and HTTPS modes"
```

---

## Task 7: End-to-end test (relay + host + simulated friend)

**Files:**
- Create: `go/cmd/ftw-relay/e2e_test.go`

- [ ] **Step 1: Write the e2e test**

```go
// go/cmd/ftw-relay/e2e_test.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// TestE2EHostAndFriendRoundtripThroughRelay is the canary: register a
// token, approve it, fire an MCP-style POST from the friend side, get
// the host's response back. All in-process, no network beyond loopback.
func TestE2EHostAndFriendRoundtripThroughRelay(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		PollTimeout: 1 * time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	// Local MCP-like backend the host will forward to.
	mcpBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"echo":` + string(body) + `}`))
	})

	host := tunnel.NewHost(srv.URL, "host-a", mcpBackend)
	host.PollTimeout = 1 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

	// 1. Register
	regBody, _ := json.Marshal(registerRequest{
		HostID: "host-a", Token: "tok1", TTLMs: 60_000, ApprovalCode: "4827",
	})
	regResp, err := http.Post(srv.URL+"/tunnel/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	if regResp.StatusCode != 200 {
		t.Fatalf("register status %d", regResp.StatusCode)
	}

	// 2. Friend hits /h/tok1/mcp before approval → 425.
	pre, err := http.Post(srv.URL+"/h/tok1/mcp", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if pre.StatusCode != http.StatusTooEarly {
		t.Fatalf("expected 425 before approval, got %d", pre.StatusCode)
	}

	// 3. Approve.
	apv, err := http.Post(srv.URL+"/h/tok1/approve", "application/json", strings.NewReader(`{"code":"4827"}`))
	if err != nil {
		t.Fatal(err)
	}
	if apv.StatusCode != http.StatusNoContent {
		t.Fatalf("approve status %d", apv.StatusCode)
	}

	// 4. Friend POSTs MCP request.
	post, err := http.Post(srv.URL+"/h/tok1/mcp", "application/json", strings.NewReader(`{"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer post.Body.Close()
	if post.StatusCode != 200 {
		body, _ := io.ReadAll(post.Body)
		t.Fatalf("mcp status %d: %s", post.StatusCode, body)
	}
	body, _ := io.ReadAll(post.Body)
	if !strings.Contains(string(body), `"echo":{"method":"ping"}`) {
		t.Fatalf("response did not contain echo: %q", body)
	}
}

func TestE2EWebReverseProxy(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		PollTimeout: 1 * time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	// Local "dashboard" backend.
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Dashboard", "yes")
		switch r.URL.Path {
		case "/":
			w.Write([]byte("home"))
		case "/api/status":
			w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	})

	host := tunnel.NewHost(srv.URL, "host-b", dashboard)
	host.PollTimeout = 1 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

	// Register + approve.
	regBody, _ := json.Marshal(registerRequest{HostID: "host-b", Token: "tok2", TTLMs: 60_000, ApprovalCode: "1234"})
	_, _ = http.Post(srv.URL+"/tunnel/register", "application/json", bytes.NewReader(regBody))
	_, _ = http.Post(srv.URL+"/h/tok2/approve", "application/json", strings.NewReader(`{"code":"1234"}`))

	// Friend opens /h/tok2/web/ → backend sees /.
	r1, err := http.Get(srv.URL + "/h/tok2/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Body.Close()
	b1, _ := io.ReadAll(r1.Body)
	if string(b1) != "home" {
		t.Fatalf("/ → %q", b1)
	}
	if r1.Header.Get("X-Dashboard") != "yes" {
		t.Fatalf("header lost: %v", r1.Header)
	}

	// Friend opens /h/tok2/web/api/status → backend sees /api/status.
	r2, err := http.Get(srv.URL + "/h/tok2/web/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	b2, _ := io.ReadAll(r2.Body)
	if !strings.Contains(string(b2), `"ok":true`) {
		t.Fatalf("/api/status → %q", b2)
	}
}
```

- [ ] **Step 2: Verify**

```bash
cd go && go test ./cmd/ftw-relay/ -v -run E2E -timeout 30s
```
Expected: both E2E tests PASS in <5s.

- [ ] **Step 3: Commit**

```bash
git add go/cmd/ftw-relay/e2e_test.go
git commit -m "test(relay): in-process e2e covering MCP roundtrip + web reverse-proxy"
```

---

## Task 8: Rewire ftw-pair to use tunnel.Host

**Files:**
- Modify: `go/cmd/ftw-pair/main.go` — change `relayAddrFlag` to URL, register session
- Modify: `go/cmd/ftw-pair/subetha.go` → delete, replace with `tunnel.go`
- Create: `go/cmd/ftw-pair/tunnel.go`
- Modify: `go/cmd/ftw-pair/subetha_test.go` → delete, replace with `tunnel_test.go`
- Create: `go/cmd/ftw-pair/tunnel_test.go`

- [ ] **Step 1: Delete the old subetha shim**

```bash
git rm go/cmd/ftw-pair/subetha.go go/cmd/ftw-pair/subetha_test.go
```

- [ ] **Step 2: Create the new tunnel glue**

```go
// go/cmd/ftw-pair/tunnel.go
// Replaces subetha.go. ftw-pair now exposes its MCP server (and the
// main dashboard at :8080) to a friend's browser/Claude Code via the
// request-response relay at relay.fortytwowatts.com (or a local relay
// for development).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// TunnelHandle wraps a tunnel.Host with the metadata the rest of
// ftw-pair needs (public URL, approval code).
type TunnelHandle struct {
	Host         *tunnel.Host
	Token        string
	ApprovalCode string
	PublicURL    string
	HostID       string
}

// StartTunnelHost registers a token with the relay, returns a TunnelHandle
// whose Host.Run can be called from a goroutine. mcpAddr is the local
// MCP server's address; apiBase is the dashboard URL (http://localhost:8080).
func StartTunnelHost(ctx context.Context, mcpAddr, apiBase string, ttl time.Duration, intent, as string) (*TunnelHandle, error) {
	relayURL := strings.TrimRight(*relayAddrFlag, "/")
	hostID := randomHostID()
	token := genWordToken()
	code := genApprovalCode()

	regBody, _ := json.Marshal(map[string]any{
		"host_id":       hostID,
		"token":         token,
		"ttl_ms":        ttl.Milliseconds(),
		"approval_code": code,
		"intent":        intent,
		"as":            as,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/tunnel/register", bytes.NewReader(regBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register with relay: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register status %d: %s", resp.StatusCode, body)
	}

	// Router: dispatch /mcp to the local MCP server, /web/* (already
	// stripped by the relay) to the dashboard.
	apiBaseURL, err := url.Parse(apiBase)
	if err != nil {
		return nil, fmt.Errorf("parse apiBase: %w", err)
	}
	mcpURL, err := url.Parse("http://" + strings.TrimPrefix(mcpAddr, "http://"))
	if err != nil {
		return nil, fmt.Errorf("parse mcpAddr: %w", err)
	}
	mcpProxy := httputil.NewSingleHostReverseProxy(mcpURL)
	dashProxy := httputil.NewSingleHostReverseProxy(apiBaseURL)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpProxy)
	mux.Handle("/", dashProxy)

	host := tunnel.NewHost(relayURL, hostID, mux)

	return &TunnelHandle{
		Host:         host,
		Token:        token,
		ApprovalCode: code,
		PublicURL:    fmt.Sprintf("%s/h/%s", relayURL, token),
		HostID:       hostID,
	}, nil
}
```

Plus stub helpers (genWordToken, genApprovalCode, randomHostID) in a new file `go/cmd/ftw-pair/idgen.go`:

```go
// go/cmd/ftw-pair/idgen.go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
)

// Tiny BIP39-ish wordlist — 64 short words, 6 words ≈ 36 bits. Plenty
// for routing keys when the access gate is host approval. The actual
// BIP39 wordlist (2048 words → 66 bits) can be swapped in later from
// go/internal/subetha/dictionary.go if we keep it after migration.
var wordlist = []string{
	"alpha", "amber", "arrow", "atom", "axis", "bay", "berry", "beam",
	"belt", "boat", "bolt", "brave", "brook", "buzz", "calm", "candle",
	"cap", "cliff", "cloud", "code", "comet", "core", "cosy", "cube",
	"daisy", "dawn", "delta", "drift", "echo", "ember", "fern", "field",
	"flame", "flax", "fjord", "forge", "frost", "garnet", "gem", "glade",
	"glow", "gray", "grove", "harbor", "haven", "hill", "honey", "iris",
	"ivory", "jade", "jet", "key", "knot", "lake", "lark", "leaf",
	"linen", "loft", "lyric", "marble", "meadow", "mint", "moss", "north",
}

func pickWord() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(wordlist))))
	return wordlist[n.Int64()]
}

func genWordToken() string {
	parts := make([]string, 6)
	for i := range parts {
		parts[i] = pickWord()
	}
	return parts[0] + "-" + parts[1] + "-" + parts[2] + "-" + parts[3] + "-" + parts[4] + "-" + parts[5]
}

func genApprovalCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(10000))
	return fmt.Sprintf("%04d", n.Int64())
}

func randomHostID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "host-" + hex.EncodeToString(b)
}
```

- [ ] **Step 3: Update main.go's flag**

In `go/cmd/ftw-pair/main.go` find:

```go
// relayAddrFlag is a package-level flag so subetha.go can read it.
// Default: FTW_PAIR_RELAY env var, then subetha.fortytwowatts.com:7777.
var relayAddrFlag *string
```

Replace the surrounding flag declaration block (locate in the `main` body where the `flag.String` calls live; should be near the top with the others):

```go
	relayDefault := "https://relay.fortytwowatts.com"
	if env := os.Getenv("FTW_PAIR_RELAY"); env != "" {
		relayDefault = env
	}
	relayAddrFlag = flag.String("relay", relayDefault, "Relay base URL (e.g. http://localhost:7378 for local dev, https://relay.fortytwowatts.com in prod)")
```

(Delete any existing relayAddrFlag declaration earlier in the file.)

- [ ] **Step 4: Find and update the StartSubethaHost call site**

`grep -n "StartSubethaHost" go/cmd/ftw-pair/*.go`. Replace each call:

Before:
```go
sub, err := StartSubethaHost(ctx, *addr)
```

After:
```go
sub, err := StartTunnelHost(ctx, *addr, *apiBase, *ttl, *intent, *as)
```

(The flag names `*intent` and `*as` already exist; check `main.go` for their declarations.)

Replace references to `sub.Code` → `sub.Token`, `sub.RelayPublicURL()` → `sub.PublicURL`, etc. — grep + adjust.

- [ ] **Step 5: New tunnel_test.go covering integration with httptest relay**

```go
// go/cmd/ftw-pair/tunnel_test.go
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStartTunnelHostRegistersAndForwards stands up a fake relay that
// behaves like ftw-relay (just enough — register, then a Get /tunnel/.../next
// stub). The handle's PublicURL should be derivable.
func TestStartTunnelHostRegistersAndForwards(t *testing.T) {
	relay := newFakeRelay(t)
	defer relay.Close()
	oldRelay := *relayAddrFlag
	*relayAddrFlag = relay.URL
	defer func() { *relayAddrFlag = oldRelay }()

	// Local MCP backend the host will forward to.
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("MCP-OK"))
	}))
	defer mcpSrv.Close()
	mcpAddr := strings.TrimPrefix(mcpSrv.URL, "http://")

	// Stub dashboard.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DASH-OK"))
	}))
	defer apiSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, err := StartTunnelHost(ctx, mcpAddr, apiSrv.URL, time.Minute, "test", "@bot")
	if err != nil {
		t.Fatalf("start tunnel host: %v", err)
	}
	if !strings.HasPrefix(handle.PublicURL, relay.URL+"/h/") {
		t.Fatalf("bad public URL: %s", handle.PublicURL)
	}
	if handle.ApprovalCode == "" || len(handle.ApprovalCode) != 4 {
		t.Fatalf("bad approval code: %q", handle.ApprovalCode)
	}
	if handle.Token == "" {
		t.Fatal("token empty")
	}
}

// newFakeRelay returns a stripped-down relay sufficient for testing
// the host's register-and-forward path. It deliberately does NOT pull
// in cmd/ftw-relay (circular import); it implements the register
// endpoint inline.
func newFakeRelay(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tunnel/register", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"host_id"`) {
			http.Error(w, "missing host_id", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"public_url":"/h/x","approval_url":"/h/x/approve"}`))
	})
	mux.HandleFunc("GET /tunnel/{host_id}/next", func(w http.ResponseWriter, r *http.Request) {
		// Always time out so the host loop doesn't busy-spin.
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}
```

- [ ] **Step 6: Verify**

```bash
cd go && go build ./cmd/ftw-pair && go test ./cmd/ftw-pair/ -v -run Tunnel -timeout 10s && rm ftw-pair
```
Expected: build OK, tunnel tests PASS. Existing tests (audit, session, etc.) should still pass too — run `go test ./cmd/ftw-pair/ -timeout 30s` if unsure.

- [ ] **Step 7: Commit**

```bash
git add go/cmd/ftw-pair/
git commit -m "feat(pair): replace subetha shim with relay-tunnel host loop"
```

---

## Task 9: Delete old code

**Files:**
- Delete: `go/cmd/ftw-connect/` (entire dir)
- Delete: `go/cmd/ftw-subetha/` (entire dir)
- Delete: `go/internal/subetha/` (entire dir)
- Delete: `scripts/install-ftw-connect.sh`
- Delete: `docs/subetha-deploy.md`
- Modify: `.github/workflows/release.yml` — remove ftw-connect + ftw-subetha matrix entries

- [ ] **Step 1: Delete**

```bash
git rm -r go/cmd/ftw-connect/ go/cmd/ftw-subetha/ go/internal/subetha/ scripts/install-ftw-connect.sh docs/subetha-deploy.md
```

- [ ] **Step 2: Verify the codebase still builds + tests pass**

```bash
cd go && go vet ./... && go test ./... -timeout 60s
```
Expected: green. If a test imports `go/internal/subetha`, find it and refactor or delete. (Pair tests had subetha_test.go which we already removed in Task 8.)

- [ ] **Step 3: Strip ftw-connect and ftw-subetha job sections from release.yml**

Open `.github/workflows/release.yml`. Locate the two job blocks:
- `ftw-connect:` (~line 157-218)
- `ftw-subetha:` (~line 220-270)

Delete both entire job blocks. Update the `discord:` job's `needs:` array to remove `ftw-connect` and `ftw-subetha`. Add an `ftw-relay:` job mirroring the binaries block but for `./cmd/ftw-relay`:

```yaml
  ftw-relay:
    name: build + upload ftw-relay binaries
    runs-on: ubuntu-latest
    needs: release
    if: needs.release.outputs.new_release_published == 'true'
    permissions:
      contents: write
    strategy:
      fail-fast: false
      matrix:
        include:
          - goos: linux
            goarch: amd64
            ext: ""
          - goos: linux
            goarch: arm64
            ext: ""
    steps:
      - name: Checkout tag
        uses: actions/checkout@v5
        with:
          ref: v${{ needs.release.outputs.new_release_version }}
      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.26'
          cache-dependency-path: go/go.sum
      - name: Build ftw-relay
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: "0"
          VERSION: v${{ needs.release.outputs.new_release_version }}
        working-directory: go
        run: |
          mkdir -p ../bin
          go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
            -o "../bin/ftw-relay-${GOOS}-${GOARCH}${{ matrix.ext }}" \
            ./cmd/ftw-relay
      - name: Upload to release
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          TAG: v${{ needs.release.outputs.new_release_version }}
          ASSET: bin/ftw-relay-${{ matrix.goos }}-${{ matrix.goarch }}${{ matrix.ext }}
        run: gh release upload "${TAG}" "${ASSET}" --clobber
```

Update `discord:` `needs:` to `[release, binaries, ftw-relay, docker]`.

- [ ] **Step 4: Verify the YAML lints**

```bash
yamllint .github/workflows/release.yml 2>/dev/null || python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))"
```
Expected: no errors. (yamllint optional; the python check just verifies it parses.)

- [ ] **Step 5: Commit**

```bash
git add -u go/cmd/ go/internal/ scripts/ docs/ .github/workflows/release.yml
git commit -m "chore(relay): delete ftw-connect, ftw-subetha, internal/subetha and install script"
```

---

## Task 10: Update docs

**Files:**
- Modify: `docs/ftw-pair.md` — strip ftw-connect install section, replace with URL flow
- Modify: `docs/relay-deploy.md` — link to release binary
- Modify: `CLAUDE.md` — update the package table

- [ ] **Step 1: Rewrite the friend section in docs/ftw-pair.md**

Locate the `## On the friend` section in `docs/ftw-pair.md`. Replace its contents (everything until the next `## ` heading) with:

```markdown
## On the friend

No install. The host shares a URL of the form:

```
https://relay.fortytwowatts.com/h/garage-coffee-river-bicycle-window-cat
```

Open it in any modern browser. The page displays a 4-digit code and waits.
Tell the host the code over a voice channel (phone, Signal call, etc.).
Once the host clicks **Allow** with the matching code, the session
activates and the page reveals:

1. A one-liner for Claude Code:
   ```bash
   claude mcp add ftw-friend --transport http \
     https://relay.fortytwowatts.com/h/<token>/mcp
   ```

2. A web-dashboard URL for the browser:
   ```
   https://relay.fortytwowatts.com/h/<token>/web/
   ```

Both URLs work for the rest of the TTL (default 4h). The host can revoke
at any time from the dashboard.
```

- [ ] **Step 2: Update the package table in CLAUDE.md**

Replace the line:

```
| `go/cmd/ftw-subetha` | Standalone relay server — matches two peers on a token and pipes encrypted bytes (`docs/pair-relay-deploy.md`) |
```

with:

```
| `go/cmd/ftw-relay`   | HTTPS request-response relay for the pair flow (`docs/relay-deploy.md`) |
```

Also remove `go/cmd/ftw-connect` and `go/internal/subetha` lines, and add:

```
| `go/internal/tunnel` | Request-response tunnel protocol shared by ftw-relay and ftw-pair |
```

- [ ] **Step 3: Commit**

```bash
git add docs/ftw-pair.md CLAUDE.md
git commit -m "docs(relay): rewrite friend section + refresh package table"
```

---

## Task 11: Final verification + PR

- [ ] **Step 1: Full test suite**

```bash
make verify
```
Expected: vet clean, all tests pass, build clean.

- [ ] **Step 2: Smoke-test the binaries**

```bash
cd go && go build -o /tmp/ftw-relay ./cmd/ftw-relay
/tmp/ftw-relay -addr :7378 &
RELAY_PID=$!
sleep 1
curl -fsS http://localhost:7378/healthz
kill $RELAY_PID
rm /tmp/ftw-relay
```
Expected: `OK` printed.

- [ ] **Step 3: Push and open PR**

```bash
git push -u origin feat/relay-as-tunnel-phase-1
gh pr create --title "feat(relay): HTTPS request-response tunnel, replaces ftw-connect" --body "..."
```

PR body should include:
- Summary of changes (3 bullets)
- Link to `docs/goals/relay-as-tunnel.md`
- "How to test locally" with 4 commands
- "Deploy instructions" → `docs/relay-deploy.md`

---

## How an operator runs e2e locally after merge

```bash
# Terminal 1 — relay
go run ./cmd/ftw-relay -addr :7378

# Terminal 2 — host (assumes a forty-two-watts main service on :8080)
go run ./cmd/ftw-pair start -relay http://localhost:7378 \
  -intent "smoke test" -ttl 1h

# Terminal 2 prints the URL + approval code, e.g.:
#   URL:  http://localhost:7378/h/alpha-amber-arrow-atom-axis-bay
#   CODE: 4827

# Terminal 3 — friend
open http://localhost:7378/h/alpha-amber-arrow-atom-axis-bay
# Page shows the 4827 code. Operator (terminal 2) types or POSTs:
curl -X POST http://localhost:7378/h/alpha-amber-arrow-atom-axis-bay/approve \
  -d '{"code":"4827"}' -H 'Content-Type: application/json'

# Friend's browser refreshes → sees web dashboard via:
open http://localhost:7378/h/alpha-amber-arrow-atom-axis-bay/web/

# Friend's Claude Code uses:
claude mcp add ftw-friend --transport http \
  http://localhost:7378/h/alpha-amber-arrow-atom-axis-bay/mcp
```

After this works locally, the operator runs `docs/relay-deploy.md` to put the
binary on the AWS VM with the Cloudflare Origin Cert. No code changes needed
for the prod cutover.
