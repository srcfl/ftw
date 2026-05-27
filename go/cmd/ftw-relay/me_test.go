package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// TestMeRegisterAndForward stands up a relay + a fake host running
// the tunnel loop, registers the site, and confirms /me/<site>/x lands
// at /x on the host.
func TestMeRegisterAndForward(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      NewOwnerRegistry(),
		PollTimeout: 1 * time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	// Local handler the host will forward to.
	hostBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Inner-Path", r.URL.Path)
		_, _ = w.Write([]byte("hello from host:" + r.URL.Path))
	})

	host := tunnel.NewHost(srv.URL, "host-owner", hostBackend)
	host.PollTimeout = 1 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

	// 1. Register the site → host mapping.
	regBody, _ := json.Marshal(meRegisterRequest{SiteID: "site-A", HostID: "host-owner"})
	regResp, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	if regResp.StatusCode != 204 {
		body, _ := io.ReadAll(regResp.Body)
		t.Fatalf("register status=%d body=%q", regResp.StatusCode, body)
	}

	// 2. /me/<site>/ → host sees /owner-access/
	r1, err := http.Get(srv.URL + "/me/site-A")
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	if r1.StatusCode != 200 {
		t.Fatalf("/me/<site> status=%d body=%q", r1.StatusCode, b1)
	}
	if !strings.Contains(string(b1), "/owner-access/") {
		t.Fatalf("expected /owner-access/ inner path, got %q", b1)
	}
	if got := r1.Header.Get("X-Inner-Path"); got != "/owner-access/" {
		t.Fatalf("inner path header = %q want /owner-access/", got)
	}

	// 3. /me/<site>/api/owner-access/whoami → host sees /api/owner-access/whoami
	r2, err := http.Get(srv.URL + "/me/site-A/api/owner-access/whoami")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("/api/owner-access/whoami status=%d body=%q", r2.StatusCode, b2)
	}
	if got := r2.Header.Get("X-Inner-Path"); got != "/api/owner-access/whoami" {
		t.Fatalf("inner path = %q", got)
	}
}

func TestMeUnknownSiteReturns503(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		PollTimeout: 100 * time.Millisecond,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/me/unknown")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
}

func TestMeRegisterRejectsEmptyFields(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	for _, body := range []string{
		`{"site_id":""}`,
		`{"host_id":"h1"}`,
		`{}`,
	} {
		resp, _ := http.Post(srv.URL+"/me/register", "application/json", strings.NewReader(body))
		if resp.StatusCode != 400 {
			t.Errorf("body=%s status=%d want 400", body, resp.StatusCode)
		}
	}
}
