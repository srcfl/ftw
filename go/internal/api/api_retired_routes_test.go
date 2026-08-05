package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Home Link's three endpoints are gone and must stay gone. A route added back
// by accident would be a passkey admission path with no verifier behind it —
// the LAN UI would show a pairing surface that cannot pair anything. Asserting
// 404 is cheap; discovering this on a live box is not.
func TestRetiredHomeLinkRoutesAreNotServed(t *testing.T) {
	srv := New(&Deps{Version: "test", WebDir: t.TempDir()})
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/home-link/status"},
		{http.MethodPost, "/api/home-link/pairing"},
		{http.MethodPost, "/api/home-link/passkeys/revoke"},
	} {
		request := httptest.NewRequest(route.method, route.path, nil)
		request.Host = "127.0.0.1:8080"
		recorder := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", route.method, route.path, recorder.Code)
		}
	}
}
