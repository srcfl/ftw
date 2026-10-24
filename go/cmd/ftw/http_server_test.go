package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPIHTTPServerSetsRequestTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 15*time.Second {
		t.Errorf("ReadTimeout = %v, want 15s", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 2*time.Minute {
		t.Errorf("WriteTimeout = %v, want 2m", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v, want 60s", srv.IdleTimeout)
	}
}

func TestHomeAssistantBundleUsesAnIngressOnlyListener(t *testing.T) {
	t.Setenv("FTW_BUNDLE", "home_assistant_addon")
	t.Setenv("FTW_HA_INGRESS", "1")
	server := newHTTPServer(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	if server.Addr != ":18099" {
		t.Fatal(server.Addr)
	}
	for _, path := range []string{"/", "/setup", "/api/config"} {
		req := httptest.NewRequest("GET", "http://ftw.local"+path, nil)
		req.RemoteAddr = "192.168.1.10:1234"
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatal("LAN UI exposed", path, w.Code)
		}
	}
	t.Setenv("FTW_BUNDLE", "")
	if newHTTPServer(":8080", http.NotFoundHandler()).Addr != ":8080" {
		t.Fatal("ordinary install changed")
	}
}
