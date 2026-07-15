package evcloud

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// TestEaseeListChargers covers the happy path end-to-end against an
// httptest.Server. Demonstrates that the client + base URL injection
// actually works.
func TestEaseeListChargers(t *testing.T) {
	var loginHits, chargerHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/login":
			loginHits++
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			if req["userName"] != "user@example.com" || req["password"] != "hunter2" {
				http.Error(w, "bad creds", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "tok-abc"})
		case "/chargers":
			chargerHits++
			if r.Header.Get("Authorization") != "Bearer tok-abc" {
				http.Error(w, "missing bearer", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`[{"id":"EH123","name":"Garage"},{"id":"EH456","name":"Driveway"}]`))
		default:
			http.Error(w, "unknown route", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	e := NewEasee().WithHTTPClient(srv.Client()).WithBaseURL(srv.URL)
	got, err := e.ListChargers(&config.EVCharger{
		Provider: "easee",
		Username: "user@example.com",
		Password: "hunter2",
	})
	if err != nil {
		t.Fatalf("ListChargers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("chargers: got %d, want 2 — %+v", len(got), got)
	}
	if got[0].ID != "EH123" || got[0].Name != "Garage" {
		t.Errorf("charger[0]: got %+v, want {EH123 Garage}", got[0])
	}
	if got[1].ID != "EH456" {
		t.Errorf("charger[1]: got %+v, want {EH456 Driveway}", got[1])
	}
	if loginHits != 1 || chargerHits != 1 {
		t.Errorf("hits: login=%d chargers=%d, want 1/1", loginHits, chargerHits)
	}
}

// TestEaseeLoginRejectsBadCreds verifies a 401 from /accounts/login
// surfaces as a status-only error. The submitted password must not
// leak into the error message even if the upstream echoes it.
func TestEaseeLoginRejectsBadCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the body back on error — this is the footgun the redact
		// comment in easee.go warns about.
		body, _ := io.ReadAll(r.Body)
		http.Error(w, "invalid login: "+string(body), http.StatusUnauthorized)
	}))
	defer srv.Close()

	e := NewEasee().WithHTTPClient(srv.Client()).WithBaseURL(srv.URL)
	_, err := e.ListChargers(&config.EVCharger{
		Provider: "easee",
		Username: "user@example.com",
		Password: "supersecret",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("password leaked into error message: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("expected 'HTTP 401' in error, got: %v", err)
	}
}

// TestEaseeTimeoutBounded is the regression test for the bug this
// refactor fixes: a server that hangs forever must not wedge the
// caller. We configure a 200 ms client timeout and make sure the
// request returns an error within a generous ceiling.
func TestEaseeTimeoutBounded(t *testing.T) {
	// srv.Close() blocks on stalled handlers, so unblock them (close
	// the channel) BEFORE srv.Close() runs. Defers are LIFO: the last
	// defer registered runs first. srv must be constructed first so we
	// can register its Close(), then register close(block) afterwards.
	srv := httptest.NewServer(nil)
	defer srv.Close()
	block := make(chan struct{})
	defer close(block)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // stall indefinitely
	})

	client := &http.Client{Timeout: 200 * time.Millisecond}
	e := NewEasee().WithHTTPClient(client).WithBaseURL(srv.URL)

	done := make(chan error, 1)
	go func() {
		_, err := e.ListChargers(&config.EVCharger{
			Provider: "easee",
			Username: "a@b",
			Password: "c",
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected timeout error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListChargers did not return within 2s despite 200ms client timeout")
	}
}
