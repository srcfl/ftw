package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/nova"
	"github.com/srcfl/ftw/go/internal/sitecontroller"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestSiteControllerPairingDiscoversZapWithoutProvisioning(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.RegisterDevice(state.Device{DriverName: "sourceful-zap", Make: "Sourceful", Serial: "zap-04772a97"}); err != nil {
		t.Fatal(err)
	}
	identity, err := nova.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "ftw-site-controller.key"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{State: store, SiteControllerIdentity: identity})
	req := httptest.NewRequest(http.MethodPost, "/api/site-controller/pairing", nil)
	req.Header.Set("X-FTW-Pairing-Intent", "pair")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var offer siteControllerPairingOffer
	if err := json.Unmarshal(rr.Body.Bytes(), &offer); err != nil {
		t.Fatal(err)
	}
	if offer.Pairing.Payload.AnchorGatewayID == nil || *offer.Pairing.Payload.AnchorGatewayID != "zap-04772a97" {
		t.Fatalf("anchor = %#v", offer.Pairing.Payload.AnchorGatewayID)
	}
	if len(offer.Scopes) != 3 || offer.Pairing.Payload.PublicKey != identity.PublicKeyHex() {
		t.Fatalf("unexpected pairing offer: %+v", offer)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("pairing proof must not be cached")
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("pairing proof must not be cross-origin readable")
	}
}

func TestSiteControllerSnapshotContainsNoRawTelemetryOrControlSurface(t *testing.T) {
	identity, err := nova.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "ftw-site-controller.key"))
	if err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.Update("sourceful-zap", telemetry.DerMeter, 4321, nil, nil)
	tel.DriverHealthMut("sourceful-zap").RecordSuccess()
	ctrl := &control.State{Mode: control.ModePlannerSelf, PlanStale: true}
	srv := New(&Deps{
		SiteControllerIdentity: identity,
		Tel:                    tel,
		Ctrl:                   ctrl,
		CtrlMu:                 &sync.Mutex{},
		Version:                "1.4.0-test",
	})
	req := httptest.NewRequest(http.MethodGet, "/api/site-controller/snapshot?site_id=sit-019b952c-1484-7994-83b9-f6198b192f3a", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var envelope sitecontroller.SnapshotEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Payload.Status.Mode != string(control.ModePlannerSelf) || !envelope.Payload.Status.PlanStale {
		t.Fatalf("status snapshot = %+v", envelope.Payload.Status)
	}
	if envelope.Payload.Health.DriversOK != 1 || envelope.Payload.Plan.Enabled {
		t.Fatalf("health/plan snapshot = %+v / %+v", envelope.Payload.Health, envelope.Payload.Plan)
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"grid_w", "pv_w", "bat_w", "meter_w", "control", "config", "command", "logs", "support"} {
		if strings.Contains(body, `"`+forbidden+`"`) {
			t.Errorf("read-only snapshot leaked forbidden field %q: %s", forbidden, body)
		}
	}
	if len(envelope.Signature) != 128 {
		t.Fatalf("signature length = %d", len(envelope.Signature))
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("controller snapshot must not be cross-origin readable")
	}
}

func TestSiteControllerEndpointsFailClosedWithoutIdentity(t *testing.T) {
	srv := New(&Deps{})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/site-controller/pairing"},
		{http.MethodGet, "/api/site-controller/snapshot?site_id=sit-019b952c-1484-7994-83b9-f6198b192f3a"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.method == http.MethodPost {
			req.Header.Set("X-FTW-Pairing-Intent", "pair")
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s status=%d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestSiteControllerPairingRequiresExplicitNonCORSIntent(t *testing.T) {
	identity, err := nova.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "ftw-site-controller.key"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{SiteControllerIdentity: identity})
	req := httptest.NewRequest(http.MethodPost, "/api/site-controller/pairing", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("pairing intent failure must not grant CORS")
	}
}
