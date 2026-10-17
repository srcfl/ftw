package appuplink

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/appenroll"
)

func TestDialPrefersTheConfiguredDoor(t *testing.T) {
	cloud := newFakeRelay(481234)
	defer cloud.close()
	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		http.Error(w, "relay should not be dialed", http.StatusBadGateway)
	}))
	defer fallback.Close()

	u := newCommandDoor(t, cloud.url(), wsOrigin(fallback.URL))
	conn, origin, err := u.dial(context.Background(), 481234, "abc")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if origin != cloud.url() {
		t.Fatalf("origin = %q, want %q", origin, cloud.url())
	}
	if fallbackHits.Load() != 0 {
		t.Fatalf("fallback was dialed %d times", fallbackHits.Load())
	}
	if got := u.commandHTTPOrigin(); got != deadmanOrigin(cloud.url()) {
		t.Fatalf("deadman origin = %q, want %q", got, deadmanOrigin(cloud.url()))
	}
}

func TestDialFallsBackWhenTheCloudRefuses(t *testing.T) {
	var cloudHits atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudHits.Add(1)
		http.Error(w, "command door not ready", http.StatusServiceUnavailable)
	}))
	defer cloud.Close()
	relay := newFakeRelay(481234)
	defer relay.close()

	u := newCommandDoor(t, wsOrigin(cloud.URL), relay.url())
	conn, origin, err := u.dial(context.Background(), 481234, "abc")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if origin != relay.url() {
		t.Fatalf("origin = %q, want the relay %q", origin, relay.url())
	}
	if cloudHits.Load() != 1 {
		t.Fatalf("cloud saw %d dials, want 1", cloudHits.Load())
	}
	if got := u.commandHTTPOrigin(); got != deadmanOrigin(relay.url()) {
		t.Fatalf("deadman origin = %q, want %q", got, deadmanOrigin(relay.url()))
	}
}

func TestDialReportsBothDoorsWhenNeitherAnswers(t *testing.T) {
	refuse := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}
	cloud := httptest.NewServer(http.HandlerFunc(refuse))
	defer cloud.Close()
	relay := httptest.NewServer(http.HandlerFunc(refuse))
	defer relay.Close()

	u := newCommandDoor(t, wsOrigin(cloud.URL), wsOrigin(relay.URL))
	if _, _, err := u.dial(context.Background(), 481234, "abc"); err == nil {
		t.Fatal("dial succeeded with both doors down")
	}
}

func TestExchangeStaysOnTheDoorThatAcceptedTheBox(t *testing.T) {
	var cloudHits atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudHits.Add(1)
		http.Error(w, "command door not ready", http.StatusServiceUnavailable)
	}))
	defer cloud.Close()
	relay := newFakeRelay(481234)
	defer relay.close()

	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(481234 * EpochMs)
	u, err := New(Options{
		Endpoint:        wsOrigin(cloud.URL),
		Fallback:        relay.url(),
		Enroll:          enroll,
		Handler:         newHandler,
		ExchangeHandler: newHandler,
		Logger:          slog.New(slog.DiscardHandler),
		Now:             func() time.Time { return now },
		Random:          func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = u.Run(ctx) }()

	select {
	case <-relay.exchangeJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not join the relay")
	}
	time.Sleep(50 * time.Millisecond)
	if got := cloudHits.Load(); got != 1 {
		t.Fatalf("cloud saw %d dials, want 1 (the refused box socket)", got)
	}
}

func TestAFallbackWithAPathIsRefused(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Options{
		Endpoint: CloudCommandOrigin,
		Fallback: "wss://relay.ftw.energy/v1/box",
		Enroll:   enroll,
		Handler:  newHandler,
	})
	if err == nil {
		t.Fatal("a fallback with a path was accepted")
	}
}

func newCommandDoor(t *testing.T, endpoint, fallback string) *Uplink {
	t.Helper()
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := New(Options{
		Endpoint: endpoint,
		Fallback: fallback,
		Enroll:   enroll,
		Handler:  newHandler,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func wsOrigin(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}
