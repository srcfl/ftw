package drivers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// silentServer accepts connections and never answers.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	return "ws://" + ln.Addr().String() + "/hub"
}

// A server that accepts the connection and never answers the upgrade holds
// Open for the dial timeout and no longer: Open runs inside driver_poll, and
// the driver's commands wait for the poll.
func TestGorillaWSOpenGivesUpOnASilentServer(t *testing.T) {
	t.Parallel()
	url := silentServer(t)
	ws := NewGorillaWS("silent")
	start := time.Now()
	err := ws.Open(context.Background(), url, nil)
	took := time.Since(start)
	if err == nil {
		ws.Close()
		t.Fatal("Open succeeded against a server that never answered")
	}
	if took < wsDialTimeout-time.Second || took > wsDialTimeout+2*time.Second {
		t.Fatalf("Open gave up after %v, want about %v: %v", took, wsDialTimeout, err)
	}
	if ws.IsOpen() {
		t.Fatal("a failed Open left the capability open")
	}
}

// A default or a stop cancels the driver call's context; a dial in that call
// ends with it instead of running out the timeout.
func TestGorillaWSOpenEndsWithItsContext(t *testing.T) {
	t.Parallel()
	url := silentServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	err := NewGorillaWS("cancelled").Open(ctx, url, nil)
	if err == nil {
		t.Fatal("Open succeeded after its context ended")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Open ran %v after its context ended", took)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled dial reported %v, not the cancel", err)
	}
}

// A refused upgrade names its HTTP status, so a driver can tell a bad token
// from a wrong path, and never the body, which may echo what it sent.
func TestGorillaWSOpenNamesTheRefusedStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "token secret-echo rejected", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	err := NewGorillaWS("refused").Open(context.Background(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/hub", map[string]string{"Authorization": "Bearer secret-echo"})
	if err == nil {
		t.Fatal("Open succeeded against a refusing server")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error does not name the status: %v", err)
	}
	if strings.Contains(err.Error(), "secret-echo") {
		t.Fatalf("error carries the response body: %v", err)
	}
}
