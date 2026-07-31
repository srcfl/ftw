package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/homelink"
)

type homeLinkAdminStub struct {
	status       homelink.AdminStatus
	pairingCalls int
	revokeID     string
}

func (s *homeLinkAdminStub) Status(context.Context) (homelink.AdminStatus, error) {
	return s.status, nil
}

func (s *homeLinkAdminStub) CreatePairing() (homelink.PairingSetup, error) {
	s.pairingCalls++
	return homelink.PairingSetup{ID: "pairing", Secret: "secret"}, nil
}

func (s *homeLinkAdminStub) RevokeCredential(_ context.Context, credentialID string) error {
	s.revokeID = credentialID
	return nil
}

func TestHomeLinkAdminIsLANOnly(t *testing.T) {
	admin := &homeLinkAdminStub{}
	server := New(&Deps{HomeLink: admin})
	for _, methodPath := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/home-link/status"},
		{http.MethodPost, "/api/home-link/pairing"},
		{http.MethodPost, "/api/home-link/passkeys/revoke"},
	} {
		request := httptest.NewRequest(
			methodPath.method,
			"http://192.168.1.20:8080"+methodPath.path,
			strings.NewReader(`{}`),
		)
		request.RemoteAddr = "203.0.113.9:1234"
		response := httptest.NewRecorder()
		server.mux.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d, want 403", methodPath.method, methodPath.path, response.Code)
		}
	}
	if admin.pairingCalls != 0 {
		t.Fatal("remote caller reached Home Link admin")
	}
}

func TestHomeLinkAdminRejectsForwardedRequests(t *testing.T) {
	for _, header := range []string{
		"Forwarded",
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"X-Real-IP",
	} {
		t.Run(header, func(t *testing.T) {
			admin := &homeLinkAdminStub{}
			server := New(&Deps{HomeLink: admin})
			request := httptest.NewRequest(
				http.MethodPost,
				"http://192.168.1.20:8080/api/home-link/pairing",
				nil,
			)
			request.RemoteAddr = "192.168.1.30:4567"
			request.Header[header] = []string{""}
			response := httptest.NewRecorder()
			server.mux.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s = %d, want 403", header, response.Code)
			}
			if admin.pairingCalls != 0 {
				t.Fatalf("%s reached Home Link admin", header)
			}
		})
	}
}

func TestHomeLinkStatusAndPairingUseFixedLocalResponses(t *testing.T) {
	admin := &homeLinkAdminStub{status: homelink.AdminStatus{
		Enabled: true, IdentityReady: true, GatewayID: "001122334455667788",
		Credentials: []homelink.CredentialSummary{},
	}}
	server := New(&Deps{HomeLink: admin, HomeLinkEnabled: true})

	status := localHomeLinkRequest(t, server, http.MethodGet, "/api/home-link/status", "")
	if status.Code != http.StatusOK ||
		!strings.Contains(status.Body.String(), `"identity_ready":true`) {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	pairing := localHomeLinkRequest(t, server, http.MethodPost, "/api/home-link/pairing", "")
	if pairing.Code != http.StatusCreated || admin.pairingCalls != 1 {
		t.Fatalf("pairing = %d %s calls=%d", pairing.Code, pairing.Body.String(), admin.pairingCalls)
	}
}

func localHomeLinkRequest(
	t *testing.T,
	server *Server,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://192.168.1.20:8080"+path, strings.NewReader(body))
	request.RemoteAddr = "192.168.1.30:4567"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.mux.ServeHTTP(response, request)
	return response
}
