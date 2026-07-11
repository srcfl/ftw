package nova

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildClaimMessage_MatchesNovaParser(t *testing.T) {
	// Nova's ownership.ClaimGateway splits on "|" and expects exactly
	// four parts: claimer_id|nonce|timestamp|gateway_id. Any other
	// shape makes claim fail with "invalid message format".
	ts := time.Unix(1713610245, 0)
	msg := BuildClaimMessage("idt-op-123", "nonce-xyz", "f42w-gw-1", ts)
	parts := strings.Split(msg, "|")
	if len(parts) != 4 {
		t.Fatalf("got %d parts, want 4 (separator collision): %q", len(parts), msg)
	}
	if parts[0] != "idt-op-123" || parts[1] != "nonce-xyz" ||
		parts[2] != "1713610245" || parts[3] != "f42w-gw-1" {
		t.Fatalf("fields out of order: %q", msg)
	}
}

func TestClaim_PostsExpectedBody(t *testing.T) {
	var gotBody map[string]string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/gateways/claim" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "op-jwt")
	err := c.Claim(context.Background(), ClaimRequest{
		GatewaySerial: "f42w-gw-1",
		OrgID:         "org-abc",
		Signature:     "deadbeef",
		Message:       "idt-op|nonce|1|f42w-gw-1",
		PublicKey:     "aabb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer op-jwt" {
		t.Fatalf("auth header: got %q", gotAuth)
	}
	for _, k := range []string{"gateway_serial", "org_id", "signature", "message", "public_key"} {
		if gotBody[k] == "" {
			t.Fatalf("claim body missing %s: %+v", k, gotBody)
		}
	}
}

func TestProvision_ReturnsDerIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devices/provision" {
			http.Error(w, "not found", 404)
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{
			"device_id": "dev-abc",
			"hardware_id": "ferroamp:ES9234",
			"device_type": "inverter",
			"site_id": "sit-xyz",
			"state": "pending",
			"created": true,
			"ders": [
				{"id":"der-111","name":"ferroamp-battery","type":"battery"},
				{"id":"der-222","name":"ferroamp-meter","type":"meter"}
			]
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "op-jwt")
	resp, err := c.Provision(context.Background(), ProvisionRequest{
		GatewaySerial: "f42w-gw-1",
		HardwareID:    "ferroamp:ES9234",
		DeviceType:    "inverter",
		SiteID:        "sit-xyz",
		DERs: []DERDefinition{
			{Name: "ferroamp-battery", Type: "battery"},
			{Name: "ferroamp-meter", Type: "meter"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DeviceID != "dev-abc" || len(resp.DERs) != 2 {
		t.Fatalf("bad response: %+v", resp)
	}
	if resp.DERs[0].ID != "der-111" || resp.DERs[1].ID != "der-222" {
		t.Fatalf("der IDs not parsed: %+v", resp.DERs)
	}
}

func TestPostJSON_SurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "site not owned by this wallet", 403)
	}))
	defer srv.Close()
	err := NewClient(srv.URL, "op-jwt").Claim(context.Background(), ClaimRequest{GatewaySerial: "g"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 in error, got: %v", err)
	}
}

func TestTranslateDerType(t *testing.T) {
	// The provisioning payload and the telemetry adapter must agree
	// on the legacy vocabulary — otherwise we provision "pv" but
	// publish "solar", and the topic-router drops.
	cases := [][2]string{
		{"pv", "solar"},
		{"ev", "ev_port"},
		{"v2x_charger", "v2x_charger"},
		{"battery", "battery"},
		{"meter", "meter"},
	}
	for _, c := range cases {
		if got := TranslateDerTypeToLegacy(c[0]); got != c[1] {
			t.Fatalf("%s → %s, got %s", c[0], c[1], got)
		}
	}
}
