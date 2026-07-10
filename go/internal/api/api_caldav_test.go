package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/calendar"
	"github.com/frahlg/forty-two-watts/go/internal/config"
)

func caldavStatus(t *testing.T, deps *Deps) map[string]any {
	t.Helper()
	srv := New(deps)
	req := httptest.NewRequest(http.MethodGet, "/api/caldav/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status code = %d, body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return body
}

func TestCalDAVStatusDisabledWhenNil(t *testing.T) {
	body := caldavStatus(t, &Deps{Version: "test"})
	if body["enabled"] != false {
		t.Fatalf("nil CalDAV should report enabled=false, got %v", body["enabled"])
	}
}

func TestCalDAVStatusReportsSubscribeURL(t *testing.T) {
	svc := calendar.New(config.CalDAV{Enabled: true}, nil, nil, "garage")
	body := caldavStatus(t, &Deps{Version: "test", CalDAV: svc})
	if body["enabled"] != true {
		t.Fatalf("expected enabled=true, got %v", body["enabled"])
	}
	if body["subscribe_url"] == "" || body["subscribe_url"] == nil {
		t.Fatalf("expected a subscribe_url, got %v", body["subscribe_url"])
	}
	// History writer defaults ON when enabled.
	if body["history_enabled"] != true {
		t.Fatalf("expected history_enabled=true by default, got %v", body["history_enabled"])
	}
}

func TestCalDAVCredentialsArePrivateAndUncached(t *testing.T) {
	svc := calendar.New(config.CalDAV{
		Enabled:  true,
		Username: "calendar-user",
		Password: "calendar-secret",
	}, nil, nil, "garage")
	srv := New(&Deps{Version: "test", CalDAV: svc})
	req := httptest.NewRequest(http.MethodGet, "/api/caldav/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("credential response must not allow cross-origin reads, got %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["password"] != "calendar-secret" {
		t.Fatalf("credential body password = %v", body["password"])
	}
}
