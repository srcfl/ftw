package nova

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to Nova's core-api over HTTP. Two surfaces are used:
//
//	POST /gateways/claim      — register this gateway's pubkey under an org
//	POST /devices/provision   — create device+DERs, get der_id back
//
// Both endpoints require a Bearer JWT from a human operator (root/admin/
// user identity type). That JWT is handed to the client via OperatorJWT;
// it is not stored by FTW — the CLI subcommand passes it in
// at claim/provision time and we forget it.
type Client struct {
	BaseURL     string
	HTTP        *http.Client
	OperatorJWT string
}

// NewClient returns a Client with a sensible default timeout.
func NewClient(baseURL, operatorJWT string) *Client {
	return &Client{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		HTTP:        &http.Client{Timeout: 30 * time.Second},
		OperatorJWT: operatorJWT,
	}
}

// ClaimRequest is the body of POST /gateways/claim. The Signature must be
// the 64-byte raw R||S ES256 signature hex over Message; Message must be
// formatted "claimer_id|nonce|timestamp|gateway_id" exactly (see
// novacore ownership.ClaimGateway line 233).
type ClaimRequest struct {
	GatewaySerial string `json:"gateway_serial"`
	OrgID         string `json:"org_id"`
	Signature     string `json:"signature"`
	Message       string `json:"message"`
	PublicKey     string `json:"public_key"`
}

// BuildClaimMessage formats the signed proof-of-possession message that
// Nova's claim verifier parses. Keeping it here as a single function means
// the CLI and tests can't drift out of sync with the verifier's format.
func BuildClaimMessage(claimerID, nonce, gatewaySerial string, ts time.Time) string {
	return fmt.Sprintf("%s|%s|%d|%s", claimerID, nonce, ts.Unix(), gatewaySerial)
}

// Claim posts to /gateways/claim.
func (c *Client) Claim(ctx context.Context, req ClaimRequest) error {
	body, _ := json.Marshal(req)
	return c.postJSON(ctx, "/gateways/claim", body, nil)
}

// DERDefinition mirrors Nova's body schema for DERs inside /devices/provision.
// The Type field uses NOVA's vocabulary (solar, battery, meter, ev_port)
// — the caller is responsible for translating from FTW's
// native DER type (pv, battery, meter, ev, v2x_charger) via TranslateDerTypeToLegacy.
type DERDefinition struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ProvisionRequest is the body of POST /devices/provision.
type ProvisionRequest struct {
	GatewaySerial string          `json:"gateway_serial,omitempty"`
	Identity      string          `json:"identity,omitempty"`
	HardwareID    string          `json:"hardware_id"`
	DeviceType    string          `json:"device_type"` // e.g. "inverter", "meter", "charger"
	SiteID        string          `json:"site_id"`
	Name          string          `json:"name,omitempty"`
	Manufacturer  string          `json:"manufacturer,omitempty"`
	Model         string          `json:"model,omitempty"`
	DERs          []DERDefinition `json:"ders,omitempty"`
}

// CreatedDER is one element of ProvisionResponse.DERs.
type CreatedDER struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// ProvisionResponse is the shape Nova returns from /devices/provision.
// Only the fields FTW needs are surfaced — the response has
// more metadata we don't use.
type ProvisionResponse struct {
	DeviceID   string       `json:"device_id"`
	HardwareID string       `json:"hardware_id"`
	DeviceType string       `json:"device_type"`
	SiteID     string       `json:"site_id"`
	State      string       `json:"state"`
	Created    bool         `json:"created"`
	DERs       []CreatedDER `json:"ders"`
}

// Provision posts to /devices/provision and returns the created/rebound
// device with the der_ids Nova generated. On rebind Nova returns 200
// with Created=false; on new create it returns 201.
func (c *Client) Provision(ctx context.Context, req ProvisionRequest) (*ProvisionResponse, error) {
	body, _ := json.Marshal(req)
	var resp ProvisionResponse
	if err := c.postJSON(ctx, "/devices/provision", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// TranslateDerTypeToLegacy maps FTW's native clean DER type
// (pv/ev/v2x_charger) to Nova's current vocabulary (solar/ev_port/v2x_charger).
// Meter and battery are identical in both. This is the same translation the
// wire adapter does — centralised here so the provisioning payload
// stays consistent with what the telemetry adapter produces on the
// wire. Flip to identity when Nova's unified schema lands.
func TranslateDerTypeToLegacy(t string) string {
	switch t {
	case KindPV:
		return "solar"
	case KindEV:
		return "ev_port"
	case KindV2X:
		return "v2x_charger"
	default:
		return t
	}
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte, out any) error {
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("nova: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.OperatorJWT != "" {
		req.Header.Set("Authorization", "Bearer "+c.OperatorJWT)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("nova: POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("nova: POST %s: %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("nova: decode %s: %w", path, err)
	}
	return nil
}
