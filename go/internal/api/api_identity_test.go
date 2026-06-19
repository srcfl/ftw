package api

import (
	"net/http/httptest"
	"testing"
)

func TestIdentityEndpointReturnsPubKey(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.SiteIdentityPubHex = "deadbeef"
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/identity", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"public_key_hex":"deadbeef"`) || !contains(rec.Body.String(), `"algorithm":"ES256"`) {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

func TestIdentityEndpoint503WhenUnset(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/identity", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("expected 503 when identity unset, got %d", rec.Code)
	}
}
