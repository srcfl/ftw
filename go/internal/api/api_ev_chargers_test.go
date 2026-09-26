package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// evProbeServer saves an EV cloud password the way Settings does and stands
// up an Easee-shaped host that records every password it is sent.
func evProbeServer(t *testing.T) (*Server, *httptest.Server, func() []string) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig(evPasswordKey, "owner-easee-secret"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/login":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]string
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			seen = append(seen, body["password"])
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "tok"})
		case "/chargers":
			_, _ = w.Write([]byte(`[{"id":"EH123"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(host.Close)

	srv := New(&Deps{
		State:  st,
		Cfg:    &config.Config{EVCharger: &config.EVCharger{Provider: "easee", Username: "owner@example.com"}},
		CfgMu:  &sync.RWMutex{},
		WebDir: t.TempDir(),
	})
	passwords := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
	return srv, host, passwords
}

func postEVChargers(srv *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/ev/chargers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleEVChargers(rr, req)
	return rr
}

// A caller who names its own base_url and leaves the password out must not
// be handed the saved account password: that is a login POST to a host the
// caller controls.
func TestEVChargersProbeKeepsSavedPasswordFromUnsavedHost(t *testing.T) {
	srv, attacker, passwords := evProbeServer(t)

	rr := postEVChargers(srv, `{"provider":"easee","username":"x","http":{"base_url":"`+attacker.URL+`"}}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "password required") {
		t.Fatalf("status = %d body = %s, want 400 password required", rr.Code, rr.Body.String())
	}
	if seen := passwords(); len(seen) != 0 {
		t.Fatalf("saved password was sent to a caller-chosen host: %q", seen)
	}

	// A password in the body is the caller's own and may go anywhere.
	rr = postEVChargers(srv, `{"provider":"easee","username":"x","password":"typed","http":{"base_url":"`+attacker.URL+`"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("typed password status = %d body = %s", rr.Code, rr.Body.String())
	}
	if seen := passwords(); len(seen) != 1 || seen[0] != "typed" {
		t.Fatalf("host saw %q, want only the typed password", seen)
	}
}

// Refreshing the picker against the base_url the operator saved still reuses
// the saved password, as it did before.
func TestEVChargersProbeReusesSavedPasswordForSavedHost(t *testing.T) {
	srv, host, passwords := evProbeServer(t)
	srv.deps.Cfg.EVCharger.HTTP = &config.EVChargerHTTP{BaseURL: host.URL}

	rr := postEVChargers(srv, `{"provider":"easee","username":"owner@example.com","http":{"base_url":"`+host.URL+`"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	if seen := passwords(); len(seen) != 1 || seen[0] != "owner-easee-secret" {
		t.Fatalf("host saw %q, want the saved password once", seen)
	}
}

func TestStoredEVPasswordAllowedForProviderDefault(t *testing.T) {
	srv, _, _ := evProbeServer(t)
	if !srv.storedEVPasswordAllowed(&config.EVCharger{Provider: "easee"}) {
		t.Fatal("the provider's default endpoint must keep the saved-password refresh")
	}
	if !srv.storedEVPasswordAllowed(&config.EVCharger{Provider: "easee", HTTP: &config.EVChargerHTTP{}}) {
		t.Fatal("an empty base_url is the provider default")
	}
}
