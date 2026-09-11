package evcloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// zaptecDefaultBaseURL is the Zaptec cloud API origin. Split out so
// tests can inject an httptest.Server URL via WithBaseURL. OAuth lives
// at {base}/oauth/token; chargers at {base}/api/chargers.
const zaptecDefaultBaseURL = "https://api.zaptec.com"

// zaptecDefaultTimeout bounds every HTTP call so a stalled TCP
// connection to api.zaptec.com can't tie up the HTTP handler goroutine
// indefinitely. 15 s matches Easee and the Lua HTTP capability.
const zaptecDefaultTimeout = 15 * time.Second

func init() { Register("zaptec", NewZaptec()) }

// Zaptec implements Provider for the Zaptec Cloud API (Go, Go 2, Pro).
// The HTTP client and base URL are injectable so tests can point at an
// httptest.Server and production code can plug in a custom transport
// without touching this package.
type Zaptec struct {
	client  *http.Client
	baseURL string
}

// NewZaptec builds a Zaptec provider pointed at the production API with
// the standard 15 s timeout.
func NewZaptec() *Zaptec {
	return &Zaptec{
		client:  &http.Client{Timeout: zaptecDefaultTimeout},
		baseURL: zaptecDefaultBaseURL,
	}
}

// WithHTTPClient returns a copy of z using the supplied client. Intended
// for tests (inject a client whose Transport points at httptest.Server)
// and for wiring transports with custom round-trippers.
func (z *Zaptec) WithHTTPClient(c *http.Client) *Zaptec {
	cp := *z
	cp.client = c
	return &cp
}

// WithBaseURL returns a copy of z pointed at the given base URL (no
// trailing slash). Paired with WithHTTPClient for httptest wiring.
func (z *Zaptec) WithBaseURL(u string) *Zaptec {
	cp := *z
	cp.baseURL = u
	return &cp
}

// Describe is the wizard's hook for rendering a Zaptec-flavored form
// (HTTP transport, "Email" as the username label).
func (z *Zaptec) Describe() Descriptor {
	return Descriptor{
		Name:          "zaptec",
		Label:         "Zaptec",
		Transport:     TransportHTTP,
		NeedsAuth:     true,
		UsernameLabel: "Email",
		LuaDriver:     "drivers/zaptec_cloud.lua",
	}
}

// ListChargers logs in with the credentials from cfg and returns the
// chargers on the account. cfg.HTTP.BaseURL overrides the default base
// URL when set, which is mostly useful for staging or self-hosted
// reverse proxies.
func (z *Zaptec) ListChargers(cfg *config.EVCharger) ([]Charger, error) {
	if cfg == nil {
		return nil, errors.New("zaptec: nil config")
	}
	if cfg.Username == "" {
		return nil, errors.New("zaptec: username required")
	}
	if cfg.Password == "" {
		return nil, errors.New("zaptec: password required")
	}
	client := z
	if cfg.HTTP != nil && cfg.HTTP.BaseURL != "" {
		client = z.WithBaseURL(cfg.HTTP.BaseURL)
	}
	token, err := client.login(cfg.Username, cfg.Password)
	if err != nil {
		return nil, err
	}
	return client.listChargers(token)
}

func (z *Zaptec) login(email, password string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", email)
	form.Set("password", password)
	req, err := http.NewRequest("POST", z.baseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := z.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		// Status-only message — the body on a 4xx can echo the submitted
		// credentials, and "invalid email or password" is the only
		// actionable info we can surface anyway.
		return "", fmt.Errorf("login: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("login: no token in response")
	}
	return tok.AccessToken, nil
}

func (z *Zaptec) listChargers(token string) ([]Charger, error) {
	req, err := http.NewRequest("GET", z.baseURL+"/api/chargers", nil)
	if err != nil {
		return nil, fmt.Errorf("chargers: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := z.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chargers request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("chargers: HTTP %d", resp.StatusCode)
	}
	var page struct {
		Data []struct {
			ID   string `json:"Id"`
			Name string `json:"Name"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("chargers: decode: %w", err)
	}
	out := make([]Charger, len(page.Data))
	for i, ch := range page.Data {
		out[i] = Charger{ID: ch.ID, Name: ch.Name}
	}
	return out, nil
}
