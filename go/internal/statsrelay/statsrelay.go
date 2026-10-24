// Package statsrelay is how the box's anonymous daily reports leave it once a
// release names a relay: each report alone, sealed with Oblivious HTTP
// (RFC 9458) to FTW Cloud's gateway key and posted to a relay that someone
// other than Sourceful runs. The relay sees the box's address but not the
// report; FTW Cloud reads the report but not the address.
//
// Two reports use it, the fleet report (package fleetping) and the energy
// report (package energyreport), each at its own random time and never in
// one message.
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
	// RelayURL is the OHTTP relay, run by someone other than Sourceful.
	// Empty until that relay exists: the energy report is then not sent,
	// and the fleet report goes straight to FTW Cloud as before.
	RelayURL = ""
	// GatewayKey is FTW Cloud's gateway key configuration (RFC 9458,
	// section 3), base64url, as
	// https://cloud.ftw.energy/.well-known/ohttp-gateway serves it. The box
	// encrypts only to this key and never asks the service for one, so every
	// box on a release uses the same key and a key swapped in at the service
	// reads nothing. Empty means no gateway key exists for this build.
	GatewayKey = ""
)

const (
	sendTimeout      = 30 * time.Second
	maxResponseBytes = 1 << 10
)

// Client posts sealed reports to one relay under one gateway key.
type Client struct {
	relay string
	key   ohttp.KeyConfig
	http  *http.Client
	now   func() time.Time
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
	if client == nil {
		client = &http.Client{Timeout: sendTimeout}
	} else {
		c := *client
		client = &c
	}
	// A redirect would resend the sealed report to an address nobody chose.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{relay: relay, key: key, http: client, now: time.Now}
}

// Relay is the relay's URL, for Settings.
func (c *Client) Relay() string { return c.relay }

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
	sealed, reader, err := ohttp.Encapsulate(c.key, inner)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.relay, bytes.NewReader(sealed))
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
