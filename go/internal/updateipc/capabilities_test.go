package updateipc

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequireSafeUpdater(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		accepted   bool
	}{
		{"fixed", "", 200, true},
		{"old", `{}`, 404, false},
		{"missing", `{"protocol":1}`, 200, false},
		{"unsafe", `{"protocol":1,"preserve_core_on_readiness_failure":false}`, 200, false},
		{"unknown", `{"protocol":2,"preserve_core_on_readiness_failure":true}`, 200, false},
		{"malformed", `{"protocol":1,`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "ftw-ipc-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "sock")
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != CapabilitiesPath {
					t.Errorf("unexpected write request: %s %s", r.Method, r.URL.Path)
				}
				if tc.accepted {
					ServeCapabilities(w, r)
					return
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})}
			go srv.Serve(ln)
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = RequireSafeUpdater(ctx, socket)
			if (err == nil) != tc.accepted {
				t.Fatalf("accepted=%v: %v", tc.accepted, err)
			}
			if err != nil && !strings.Contains(err.Error(), "update ftw-updater first") {
				t.Fatal(err)
			}
		})
	}
}

func TestUnavailableUpdaterIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := RequireSafeUpdater(ctx, filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "Core has not opened its data") {
		t.Fatalf("unavailable updater accepted: %v", err)
	}
}
