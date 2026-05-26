package evcloud

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

// easeeDefaultBaseURL is the Easee cloud API base URL. Split out so
// tests can inject an httptest.Server URL via NewEasee.
const easeeDefaultBaseURL = "https://api.easee.com/api"

// easeeDefaultTimeout bounds every HTTP call so a stalled TCP
// connection to api.easee.com can't tie up the HTTP handler goroutine
// indefinitely. 15 s matches what the rest of the codebase uses for
// external HTTP calls (see e.g. drivers/lua.go HTTP capability).
const easeeDefaultTimeout = 15 * time.Second

func init() { Register("easee", NewEasee()) }

// Easee implements Provider for the Easee Cloud API. The HTTP client
// and base URL are injectable so tests can point at an httptest.Server
// and production code can plug in a custom transport (retries, tracing,
// etc.) without touching this package.
type Easee struct {
	client  *http.Client
	baseURL string
}

// NewEasee builds an Easee provider pointed at the production API with
// the standard 15 s timeout.
func NewEasee() *Easee {
	return &Easee{
		client:  &http.Client{Timeout: easeeDefaultTimeout},
		baseURL: easeeDefaultBaseURL,
	}
}

// WithHTTPClient returns a copy of e using the supplied client. Intended
// for tests (inject a client whose Transport points at httptest.Server)
// and for wiring transports with custom round-trippers.
func (e *Easee) WithHTTPClient(c *http.Client) *Easee {
	cp := *e
	cp.client = c
	return &cp
}

// WithBaseURL returns a copy of e pointed at the given base URL (no
// trailing slash). Paired with WithHTTPClient for httptest wiring.
func (e *Easee) WithBaseURL(u string) *Easee {
	cp := *e
	cp.baseURL = u
	return &cp
}

// Describe is the wizard's hook for rendering an Easee-flavored form
// (HTTP transport, "Email" as the username label).
func (e *Easee) Describe() Descriptor {
	return Descriptor{
		Name:          "easee",
		Label:         "Easee",
		Transport:     TransportHTTP,
		NeedsAuth:     true,
		UsernameLabel: "Email",
		LuaDriver:     "drivers/easee_cloud.lua",
	}
}

// ListChargers logs in with the credentials from cfg and returns the
// chargers on the account. cfg.HTTP.BaseURL overrides the default base
// URL when set, which is mostly useful for staging or self-hosted
// reverse proxies.
func (e *Easee) ListChargers(cfg *config.EVCharger) ([]Charger, error) {
	if cfg == nil {
		return nil, errors.New("easee: nil config")
	}
	if cfg.Username == "" {
		return nil, errors.New("easee: username required")
	}
	if cfg.Password == "" {
		return nil, errors.New("easee: password required")
	}
	client := e
	if cfg.HTTP != nil && cfg.HTTP.BaseURL != "" {
		client = e.WithBaseURL(cfg.HTTP.BaseURL)
	}
	token, err := client.login(cfg.Username, cfg.Password)
	if err != nil {
		return nil, err
	}
	return client.listChargers(token)
}

func (e *Easee) login(email, password string) (string, error) {
	body, err := json.Marshal(map[string]string{"userName": email, "password": password})
	if err != nil {
		return "", fmt.Errorf("login: marshal: %w", err)
	}
	req, err := http.NewRequest("POST", e.baseURL+"/accounts/login", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
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
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("login: no token in response")
	}
	return tok.AccessToken, nil
}

func (e *Easee) listChargers(token string) ([]Charger, error) {
	req, err := http.NewRequest("GET", e.baseURL+"/chargers", nil)
	if err != nil {
		return nil, fmt.Errorf("chargers: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chargers request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("chargers: HTTP %d", resp.StatusCode)
	}
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("chargers: decode: %w", err)
	}
	out := make([]Charger, len(list))
	for i, ch := range list {
		out[i] = Charger{ID: ch.ID, Name: ch.Name}
	}
	return out, nil
}
