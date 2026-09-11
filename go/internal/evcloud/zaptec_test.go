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

// TestZaptecListChargers covers the happy path end-to-end against an
// httptest.Server. Demonstrates that the client + base URL injection
// actually works, and that the OAuth password grant is form-encoded.
func TestZaptecListChargers(t *testing.T) {
	var loginHits, chargerHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			loginHits++
			if r.Method != http.MethodPost {
				t.Errorf("login method: got %s, want POST", r.Method)
			}
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
				t.Errorf("login Content-Type: got %q, want form-urlencoded", ct)
			}
			body, _ := io.ReadAll(r.Body)
			form := string(body)
			if !strings.Contains(form, "grant_type=password") {
				t.Errorf("login body missing grant_type=password: %q", form)
			}
			if !strings.Contains(form, "username=user%40example.com") && !strings.Contains(form, "username=user@example.com") {
				t.Errorf("login body missing username: %q", form)
			}
			if !strings.Contains(form, "password=hunter2") {
				t.Errorf("login body missing password: %q", form)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-abc"})
		case "/api/chargers":
			chargerHits++
			if r.Header.Get("Authorization") != "Bearer tok-abc" {
				http.Error(w, "missing bearer", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"Data":[{"Id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","Name":"Garage"},{"Id":"ffffffff-0000-1111-2222-333333333333","Name":"Driveway"}]}`))
		default:
			http.Error(w, "unknown route", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	z := NewZaptec().WithHTTPClient(srv.Client()).WithBaseURL(srv.URL)
	got, err := z.ListChargers(&config.EVCharger{
		Provider: "zaptec",
		Username: "user@example.com",
		Password: "hunter2",
	})
	if err != nil {
		t.Fatalf("ListChargers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("chargers: got %d, want 2 — %+v", len(got), got)
	}
	if got[0].ID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" || got[0].Name != "Garage" {
		t.Errorf("charger[0]: got %+v, want Garage UUID", got[0])
	}
	if got[1].Name != "Driveway" {
		t.Errorf("charger[1]: got %+v, want Driveway", got[1])
	}
	if loginHits != 1 || chargerHits != 1 {
		t.Errorf("hits: login=%d chargers=%d, want 1/1", loginHits, chargerHits)
	}
}

// TestZaptecLoginRejectsBadCreds verifies a 401 from /oauth/token
// surfaces as a status-only error. The submitted password must not
// leak into the error message even if the upstream echoes it.
func TestZaptecLoginRejectsBadCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		http.Error(w, "invalid login: "+string(body), http.StatusUnauthorized)
	}))
	defer srv.Close()

	z := NewZaptec().WithHTTPClient(srv.Client()).WithBaseURL(srv.URL)
	_, err := z.ListChargers(&config.EVCharger{
		Provider: "zaptec",
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

// TestZaptecTimeoutBounded is the regression test for a hung cloud:
// a server that hangs forever must not wedge the caller. We configure
// a 200 ms client timeout and make sure the request returns an error
// within a generous ceiling.
func TestZaptecTimeoutBounded(t *testing.T) {
	srv := httptest.NewServer(nil)
	defer srv.Close()
	block := make(chan struct{})
	defer close(block)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	})

	client := &http.Client{Timeout: 200 * time.Millisecond}
	z := NewZaptec().WithHTTPClient(client).WithBaseURL(srv.URL)

	done := make(chan error, 1)
	go func() {
		_, err := z.ListChargers(&config.EVCharger{
			Provider: "zaptec",
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

func TestZaptecDescribe(t *testing.T) {
	d := NewZaptec().Describe()
	if d.Name != "zaptec" || d.Label != "Zaptec" {
		t.Errorf("name/label: %+v", d)
	}
	if d.Transport != TransportHTTP {
		t.Errorf("transport: %s, want http", d.Transport)
	}
	if !d.NeedsAuth || d.UsernameLabel != "Email" {
		t.Errorf("auth form: %+v", d)
	}
	if d.LuaDriver != "drivers/zaptec_cloud.lua" {
		t.Errorf("lua driver hint: %q", d.LuaDriver)
	}
}
