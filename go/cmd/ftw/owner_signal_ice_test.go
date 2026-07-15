package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/p2p"
)

// fakeICESetter records SetICEServers pushes so the refresh helper can be tested
// without standing up a full p2p.Manager.
type fakeICESetter struct {
	calls [][]p2p.ICEServer
}

func (f *fakeICESetter) SetICEServers(ice []p2p.ICEServer) { f.calls = append(f.calls, ice) }

func TestRefreshSignalICE_SetsServersOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/signal/ice" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ice_servers":[{"urls":["stun:s.example:3478"]},{"urls":["turn:t.example:3478?transport=udp"],"username":"u","credential":"c"}]}`))
	}))
	defer srv.Close()

	setter := &fakeICESetter{}
	refreshSignalICE(context.Background(), srv.Client(), srv.URL, "host-1", setter)

	if len(setter.calls) != 1 {
		t.Fatalf("SetICEServers calls = %d, want 1", len(setter.calls))
	}
	got := setter.calls[0]
	if len(got) != 2 {
		t.Fatalf("ice servers = %d, want 2", len(got))
	}
	if got[1].Username != "u" || got[1].Credential != "c" || len(got[1].URLs) != 1 {
		t.Fatalf("unexpected TURN entry: %+v", got[1])
	}
}

func TestRefreshSignalICE_LeavesPriorOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	setter := &fakeICESetter{}
	refreshSignalICE(context.Background(), srv.Client(), srv.URL, "host-1", setter)

	if len(setter.calls) != 0 {
		t.Fatalf("SetICEServers must NOT be called on a non-200 response; got %d calls", len(setter.calls))
	}
}
