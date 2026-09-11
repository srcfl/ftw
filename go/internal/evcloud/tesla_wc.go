package evcloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// teslaWCDefaultTimeout bounds the wizard probe so a missing LAN host
// cannot hold the request goroutine. 15 s matches Easee/Zaptec.
const teslaWCDefaultTimeout = 15 * time.Second

func init() { Register("tesla-wc", NewTeslaWC()) }

// TeslaWC implements Provider for a Tesla Wall Connector Gen 3 on the
// local network. The box exposes an undocumented but widely documented
// HTTP API (GET /api/1/vitals, /api/1/version) with no authentication.
// Current limits are not writable on the wall connector; control stays
// with tesla_vehicle when a Tesla is plugged in.
type TeslaWC struct {
	client  *http.Client
	baseURL string // test override; production reads cfg.HTTP.BaseURL
}

// NewTeslaWC builds a Tesla Wall Connector provider with the standard timeout.
func NewTeslaWC() *TeslaWC {
	return &TeslaWC{client: &http.Client{Timeout: teslaWCDefaultTimeout}}
}

// WithHTTPClient returns a copy using the supplied client.
func (t *TeslaWC) WithHTTPClient(c *http.Client) *TeslaWC {
	cp := *t
	cp.client = c
	return &cp
}

// WithBaseURL returns a copy pointed at the given origin (no trailing slash).
func (t *TeslaWC) WithBaseURL(u string) *TeslaWC {
	cp := *t
	cp.baseURL = strings.TrimRight(u, "/")
	return &cp
}

// Describe is the wizard's hook for a local HTTP form (host, no auth).
func (t *TeslaWC) Describe() Descriptor {
	return Descriptor{
		Name:      "tesla-wc",
		Label:     "Tesla Wall Connector",
		Transport: TransportHTTP,
		NeedsAuth: false,
		LuaDriver: "drivers/tesla_wall_connector.lua",
	}
}

// ListChargers probes the configured LAN origin and returns the wall
// connector's serial from GET /api/1/version. A vitals-only box that
// answers without a version document still lists as one charger.
func (t *TeslaWC) ListChargers(cfg *config.EVCharger) ([]Charger, error) {
	if cfg == nil {
		return nil, errors.New("tesla-wc: nil config")
	}
	base := t.baseURL
	if cfg.HTTP != nil && cfg.HTTP.BaseURL != "" {
		base = teslaWCNormalizeBase(cfg.HTTP.BaseURL)
	}
	if base == "" {
		return nil, errors.New("tesla-wc: http.base_url required")
	}

	serial, name, err := t.readIdentity(base)
	if err != nil {
		return nil, err
	}
	if serial == "" {
		serial = "tesla-wc"
	}
	if name == "" {
		name = "Tesla Wall Connector"
	}
	return []Charger{{ID: serial, Name: name}}, nil
}

func (t *TeslaWC) readIdentity(base string) (serial, name string, err error) {
	ver, verr := t.getJSON(base + "/api/1/version")
	if verr == nil {
		serial = stringField(ver, "serial_number", "serialNumber")
		part := stringField(ver, "part_number", "partNumber")
		if part != "" {
			name = "Tesla Wall Connector " + part
		}
		if serial != "" {
			return serial, name, nil
		}
	}
	// Version is missing on some firmware; vitals still proves the box.
	if _, vitalsErr := t.getJSON(base + "/api/1/vitals"); vitalsErr != nil {
		if verr != nil {
			return "", "", fmt.Errorf("tesla-wc: version: %w", verr)
		}
		return "", "", fmt.Errorf("tesla-wc: vitals: %w", vitalsErr)
	}
	return serial, name, nil
}

func (t *TeslaWC) getJSON(url string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(teslaWCRepairJSON(raw), &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// teslaWCRepairJSON matches the Lua driver's repair_json: some Gen 3
// firmware emits a bare `nan` that encoding/json rejects. The wizard
// probe and the runtime driver have to accept the same body.
var teslaWCNaN = regexp.MustCompile(`(?i)\bnan\b`)

func teslaWCRepairJSON(raw []byte) []byte {
	if !teslaWCNaN.Match(raw) {
		return raw
	}
	return teslaWCNaN.ReplaceAll(raw, []byte("null"))
}

func teslaWCNormalizeBase(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return s
}

func stringField(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}
