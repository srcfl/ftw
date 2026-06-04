package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// owner_signal_loop.go — the Pi side of the blind WebRTC signaling rendezvous
// (P2P-only home route, slice 4). It runs ALONGSIDE the existing owner tunnel
// long-poll (slice 6 removes the tunnel; this loop becomes the only owner
// transport). The loop:
//
//  1. long-polls GET /signal/{host_id}/offer (authenticated with the per-host
//     poll secret) for a browser's SDP offer,
//  2. answers it via p2p.Manager.Answer with FAIL-CLOSED replay headers — the
//     per-process tunnel marker is stamped so every DataChannel frame is treated
//     as REMOTE (no LAN-bypass), and NO owner cookie is injected (the channel
//     starts UNAUTHENTICATED; the browser logs in over it), then
//  3. signs the answer's DTLS fingerprint and POSTs {sdp, fp_sig, ts} to
//     POST /signal/{host_id}/answer for the browser to verify against the pinned
//     key.
//
// The relay only ever sees opaque SDP/signature blobs; the resulting DataChannel
// is DTLS-encrypted end to end.

// p2pAnswerer is the slice of p2p.Manager this loop needs. Declared as an
// interface so the wiring stays narrow and testable.
type p2pAnswerer interface {
	Answer(ctx context.Context, offerSDP string, replayHeaders http.Header) (string, error)
	SignFingerprint(answerSDP string) (sig string, tsMs int64)
}

// pollSecretSource yields the current relay-minted poll token. *tunnel.Host
// satisfies it via PollSecret(), so the signaling loop shares the same token the
// registration loop refreshes (a relay restart re-mints it).
type pollSecretSource interface {
	PollSecret() string
}

// signalAnswerWire is the answer blob parked for the browser. It mirrors the
// JSON the old POST /api/p2p/offer handler returned, so web/p2p.js's
// verifyAnswerSignature consumes it unchanged.
type signalAnswerWire struct {
	Type  string `json:"type"`
	SDP   string `json:"sdp"`
	FpSig string `json:"fp_sig"`
	Ts    int64  `json:"ts"`
}

// runOwnerSignalLoop blocks until ctx is cancelled, long-polling the relay for
// browser offers and answering them over P2P. Transient errors are logged and
// retried with a short backoff so a relay blip never kills the loop.
func runOwnerSignalLoop(ctx context.Context, relayURL, hostID, tunnelMarker string, p2p p2pAnswerer, polls pollSecretSource) {
	client := &http.Client{Timeout: 35 * time.Second}
	for {
		if ctx.Err() != nil {
			return
		}
		offerSDP, ok, err := pollSignalOffer(ctx, client, relayURL, hostID, polls.PollSecret())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("owner-access: signal offer poll failed", "err", err, "host_id", hostID)
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		if !ok {
			continue // 204 — re-poll
		}
		// Answer in its own goroutine so the loop keeps polling for the next
		// browser while this handshake (up to the ICE-gather timeout) runs.
		go handleSignalOffer(ctx, client, relayURL, hostID, tunnelMarker, offerSDP, p2p, polls)
	}
}

// pollSignalOffer issues one long-poll for a parked offer. Returns
// (sdp, true, nil) on an offer, ("", false, nil) on 204, or an error.
func pollSignalOffer(ctx context.Context, client *http.Client, relayURL, hostID, pollSecret string) (string, bool, error) {
	url := relayURL + "/signal/" + hostID + "/offer"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	if pollSecret != "" {
		req.Header.Set(tunnel.PollSecretHeader, pollSecret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return "", false, nil
	case http.StatusOK:
		var off struct {
			SDP string `json:"sdp"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&off); err != nil {
			return "", false, err
		}
		if off.SDP == "" {
			return "", false, nil
		}
		return off.SDP, true, nil
	default:
		return "", false, &signalHTTPError{status: resp.StatusCode}
	}
}

// handleSignalOffer answers one offer and parks the signed answer.
func handleSignalOffer(ctx context.Context, client *http.Client, relayURL, hostID, tunnelMarker, offerSDP string, p2p p2pAnswerer, polls pollSecretSource) {
	// FAIL-CLOSED replay headers: stamp the per-process tunnel marker so every
	// DataChannel frame is REMOTE (the gate can never grant it LAN-bypass), and
	// inject NO cookie — the channel is unauthenticated until the browser logs in
	// over it (the Bridge then captures the resulting session). This is the
	// non-negotiable safety invariant for the signaling path.
	replay := http.Header{}
	replay.Set("X-FTW-Tunnel", tunnelMarker)

	answerSDP, err := p2p.Answer(ctx, offerSDP, replay)
	if err != nil {
		slog.Warn("owner-access: p2p answer failed", "err", err, "host_id", hostID)
		return
	}
	fpSig, tsMs := p2p.SignFingerprint(answerSDP)
	body, err := json.Marshal(signalAnswerWire{Type: "answer", SDP: answerSDP, FpSig: fpSig, Ts: tsMs})
	if err != nil {
		slog.Warn("owner-access: marshal answer", "err", err)
		return
	}
	if err := postSignalAnswer(ctx, client, relayURL, hostID, polls.PollSecret(), body); err != nil {
		if ctx.Err() == nil {
			slog.Warn("owner-access: post signal answer failed", "err", err, "host_id", hostID)
		}
	}
}

func postSignalAnswer(ctx context.Context, client *http.Client, relayURL, hostID, pollSecret string, body []byte) error {
	url := relayURL + "/signal/" + hostID + "/answer"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if pollSecret != "" {
		req.Header.Set(tunnel.PollSecretHeader, pollSecret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return &signalHTTPError{status: resp.StatusCode}
	}
	return nil
}

type signalHTTPError struct{ status int }

func (e *signalHTTPError) Error() string {
	return "relay returned " + http.StatusText(e.status)
}
