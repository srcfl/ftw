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
	// PollTimeout bounds how long the host waits for a queued request
	// before re-polling. Relay-side default is 25s; the host's HTTP
	// client timeout is slightly larger so a slow relay response is
	// treated as a transport error, not a stuck loop.
	PollTimeout time.Duration
}

// NewHost constructs a Host. relayURL is the base URL (no trailing
// slash). handler receives each tunneled request as a normal
// ServeHTTP call.
func NewHost(relayURL, hostID string, handler http.Handler) *Host {
	return &Host{
		relayURL:    relayURL,
		hostID:      hostID,
		handler:     handler,
		client:      &http.Client{Timeout: 35 * time.Second},
		PollTimeout: 25 * time.Second,
	}
}

// Run blocks until ctx is cancelled. Errors are logged via slog and
// the loop continues; transient relay outages should not kill the
// host.
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
		if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
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
		h.postError(ctx, tr.ReqID, http.StatusInternalServerError, err)
		return
	}
	for k, vv := range tr.Header {
		inner.Header[k] = vv
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, inner)
	h.postResponse(ctx, tr.ReqID, TunneledResponse{
		Status: rec.Code,
		Header: rec.Result().Header,
		Body:   rec.Body.Bytes(),
	})
}

func (h *Host) postResponse(ctx context.Context, reqID string, resp TunneledResponse) {
	url := fmt.Sprintf("%s/tunnel/%s/response/%s", h.relayURL, h.hostID, reqID)
	body, err := json.Marshal(resp)
	if err != nil {
		slog.Error("tunnel marshal response", "err", err, "req_id", reqID)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Error("tunnel build post", "err", err, "req_id", reqID)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := h.client.Do(req)
	if err != nil {
		// Quietly drop on cancellation — shutdown noise, not an error.
		if ctx.Err() == nil {
			slog.Error("tunnel post response", "err", err, "req_id", reqID)
		}
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
