// Package statsrelaytest is a fake relay and gateway for tests. The gateway
// reads a report as FTW Cloud's does: one padded inner request, its decoder
// chosen by schema. It keeps the reports for the test to read.
package statsrelaytest

import (
	"bytes"
	"crypto/ecdh"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/statsrelay"
	"github.com/srcfl/ftw/go/protocol/energywire"
	"github.com/srcfl/ftw/go/protocol/fleetwire"
	"github.com/srcfl/ftw/go/protocol/ohttp"
	"github.com/srcfl/ftw/go/protocol/reportwire"
)

// Gateway is a fake FTW Cloud gateway.
type Gateway struct {
	*httptest.Server
	Key ohttp.KeyConfig

	mu     sync.Mutex
	now    time.Time
	fleet  []fleetwire.Report
	energy []energywire.Report
	last   int
}

// Relay forwards only the body and its content type, and records what it saw.
type Relay struct {
	*httptest.Server

	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
}

// New starts a gateway whose clock reads now, a relay in front of it, and a
// client posting to the relay.
func New(t testing.TB, now time.Time) (*statsrelay.Client, *Gateway, *Relay) {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := ohttp.NewGateway(ohttp.Key{ID: 7, Private: k})
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{Key: gw.Configs()[0], now: now}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		msg, sealer, err := gw.Decapsulate(body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status := g.add(msg)
		sealed, _ := sealer.Seal(reportwire.Response(status))
		w.Header().Set("Content-Type", ohttp.ResponseType)
		_, _ = w.Write(sealed)
	}))
	t.Cleanup(g.Close)

	rl := &Relay{}
	rl.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rl.mu.Lock()
		rl.bodies = append(rl.bodies, body)
		rl.headers = append(rl.headers, r.Header.Clone())
		rl.mu.Unlock()
		resp, err := http.Post(g.URL, r.Header.Get("Content-Type"), bytes.NewReader(body))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(rl.Close)
	return statsrelay.New(rl.URL, g.Key, nil), g, rl
}

func (g *Gateway) add(msg []byte) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = http.StatusBadRequest
	report, ok := reportwire.Read(msg)
	if !ok || !reportwire.Fresh(report.Day, g.now) {
		return g.last
	}
	body := report.Body
	switch reportwire.Schema(body) {
	case fleetwire.Schema:
		if r, err := fleetwire.Decode(body); err == nil {
			g.fleet, g.last = append(g.fleet, r), http.StatusNoContent
		}
	case energywire.Schema:
		if r, err := energywire.Decode(body, g.now); err == nil {
			g.energy, g.last = append(g.energy, r), http.StatusNoContent
		}
	}
	return g.last
}

// Fleet is every fleet report the gateway took.
func (g *Gateway) Fleet() []fleetwire.Report {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]fleetwire.Report(nil), g.fleet...)
}

// Energy is every energy report the gateway took.
func (g *Gateway) Energy() []energywire.Report {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]energywire.Report(nil), g.energy...)
}

// Answered is the gateway's last inner status.
func (g *Gateway) Answered() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// Bodies is every body the relay forwarded, and Headers what came with each.
func (r *Relay) Bodies() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies...)
}

func (r *Relay) Headers() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.headers...)
}
