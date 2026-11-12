// Package statsrelay posts the box's daily reports, each alone.
//
// The energy report (package energyreport) goes straight to FTW Cloud's
// POST /v1/energy (NewDirect, DirectURL) as one padded request, unsealed
// (Fredrik, 12 November 2026). FTW Cloud sees the box's address while the
// request is open, the way any website does, and keeps neither the address
// nor the report.
//
// The sealed path (Pinned, New) seals a report with Oblivious HTTP
// (RFC 9458) to a gateway key and posts it to a relay that someone other
// than Sourceful runs. Only the fleet report (package fleetping) can take
// it, and only from a build that pins both. No build does, and FTW Cloud
// has served no gateway since 28 November 2026 (srcfl/ftw-cloud#72), so a
// build that pins one needs a new gateway first.
package statsrelay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/protocol/ohttp"
	"github.com/srcfl/ftw/go/protocol/reportwire"
)

// Compile-time settings, as in cloudlink: a relay a household could point
// elsewhere is a relay a support call could point at an attacker's. A local
// test build may set them with -ldflags -X.
var (
	// RelayURL is the fleet report's OHTTP relay, run by someone other than
	// Sourceful. Empty in every release: the fleet report then goes straight
	// to /v1/fleet. The energy report never uses it.
	RelayURL = ""
	// GatewayKey is the gateway's key configuration (RFC 9458, section 3),
	// base64url. The box encrypts only to this key and never asks the
	// service for one, so every box on a release uses the same key and a key
	// swapped in at the service reads nothing. Empty means no gateway key
	// exists for this build.
	GatewayKey = ""
	// DirectURL is FTW Cloud's route for the energy report. It takes one
	// padded request (package reportwire), unsealed.
	DirectURL = "https://cloud.ftw.energy/v1/energy"
)

// DirectType is the direct request's media type: the inner request is a
// Binary HTTP message (RFC 9292).
const DirectType = "message/bhttp"

const (
	sendTimeout      = 30 * time.Second
	maxResponseBytes = 1 << 10
)

// Client posts reports to one place: sealed to one relay under one gateway
// key, or, when direct, unsealed to FTW Cloud.
type Client struct {
	url    string
	key    ohttp.KeyConfig
	direct bool
	http   *http.Client
	now    func() time.Time
}

// Pinned is this build's client, or nil when the build names no usable relay
// and gateway key.
func Pinned() *Client {
	relay, key, ok := pinned(RelayURL, GatewayKey)
	if !ok {
		return nil
	}
	return New(relay, key, nil)
}

func pinned(relay, key string) (string, ohttp.KeyConfig, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || relay == "" || key == "" {
		return "", ohttp.KeyConfig{}, false
	}
	cfg, err := ohttp.ParseKeyConfig(raw)
	if err != nil {
		return "", ohttp.KeyConfig{}, false
	}
	u, err := url.Parse(relay)
	if err != nil {
		return "", ohttp.KeyConfig{}, false
	}
	// Host names compare without case and without a trailing dot, so FTW
	// Cloud cannot pass as its own relay under another spelling.
	host := strings.TrimSuffix(u.Hostname(), ".")
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.EqualFold(host, reportwire.Authority) {
		return "", ohttp.KeyConfig{}, false
	}
	return relay, cfg, true
}

// New is a client for one relay and key. Tests point it at a fake relay.
func New(relay string, key ohttp.KeyConfig, client *http.Client) *Client {
	return &Client{url: relay, key: key, http: httpClient(client), now: time.Now}
}

// NewDirect is a client that posts each report, unsealed, to endpoint. Tests
// point it at a fake FTW Cloud.
func NewDirect(endpoint string, client *http.Client) *Client {
	return &Client{url: endpoint, direct: true, http: httpClient(client), now: time.Now}
}

func httpClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: sendTimeout}
	} else {
		c := *client
		client = &c
	}
	// A redirect would resend the report to an address nobody chose.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

// WithClock makes c stamp reports with the day now reads. Tests pin it to
// the fake gateway's clock, so a fixed test date stays fresh.
func (c *Client) WithClock(now func() time.Time) *Client {
	c.now = now
	return c
}

// Relay is the relay's URL, for Settings, and empty when c posts directly.
func (c *Client) Relay() string {
	if c.direct {
		return ""
	}
	return c.url
}

// Endpoint is where c posts: the relay, or FTW Cloud when direct.
func (c *Client) Endpoint() string { return c.url }

// Send seals one report body and reads the gateway's answer. Only a sealed
// 204 from the gateway the box encrypted to counts as delivered.
//
// Each call is one report: it draws a fresh id, which the gateway counts
// once. Nothing retries; a retry of the same report would have to reuse its
// id, or the gateway would count it twice.
func (c *Client) Send(ctx context.Context, body []byte) error {
	id := make([]byte, reportwire.IDBytes)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	inner, ok := reportwire.Request(reportwire.Report{ID: id, Day: c.now().UTC().Format(time.DateOnly), Body: body})
	if !ok {
		return errors.New("statsrelay: the report does not fit the padded request")
	}
	if c.direct {
		return c.sendDirect(ctx, inner)
	}
	sealed, reader, err := ohttp.Encapsulate(c.key, inner)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(sealed))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ohttp.RequestType)
	// Pinned, as for the fleet report's direct path: the relay sees it, and
	// Go's default names the runtime's version.
	req.Header.Set("User-Agent", "ftw-report")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || media != ohttp.ResponseType || len(data) > maxResponseBytes {
		return fmt.Errorf("statsrelay: relay answered %d %q", resp.StatusCode, media)
	}
	msg, err := reader.Open(data)
	if err != nil {
		return err
	}
	answer, err := ohttp.ParseResponse(msg)
	if err != nil {
		return err
	}
	if answer.Status != http.StatusNoContent {
		return fmt.Errorf("statsrelay: gateway answered %d", answer.Status)
	}
	return nil
}

// sendDirect posts the inner request to FTW Cloud. Only a 204 counts as
// delivered; the answer carries nothing else the box would read.
func (c *Client) sendDirect(ctx context.Context, inner []byte) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(inner))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", DirectType)
	req.Header.Set("User-Agent", "ftw-report")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("statsrelay: FTW Cloud answered %d", resp.StatusCode)
	}
	return nil
}
