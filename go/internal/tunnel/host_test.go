package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHostLoopDispatchesToHandler wires a fresh queue behind an
// httptest server, runs a Host, enqueues a request, and confirms the
// handler ran and the response flowed back.
func TestHostLoopDispatchesToHandler(t *testing.T) {
	q := NewQueue()
	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel/host-a/next", func(w http.ResponseWriter, r *http.Request) {
		req, err := q.Poll(r.Context(), "host-a", 1*time.Second)
		if errors.Is(err, ErrPollTimeout) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(req)
	})
	mux.HandleFunc("/tunnel/host-a/response/", func(w http.ResponseWriter, r *http.Request) {
		reqID := strings.TrimPrefix(r.URL.Path, "/tunnel/host-a/response/")
		var resp TunneledResponse
		if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
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
	host.PollTimeout = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

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
