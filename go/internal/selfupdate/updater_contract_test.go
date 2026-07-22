package selfupdate

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func serveUnixUpdater(t *testing.T, handler http.Handler) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "ftw-updater-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(path)
	})
	return path
}

// v1.3.1 exposed GET /status and accepted the same update JSON, but ignored
// unknown fields and recreated Core only. A new Core must reject that contract
// before it sends an update request or opens state with a mixed release pair.
func TestV131UpdaterIsRejectedBeforeCoreUpdate(t *testing.T) {
	var posts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"idle"}`))
	})
	mux.HandleFunc("POST /update", func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	socket := serveUnixUpdater(t, mux)

	c := New(Config{SocketPath: socket, RequiredUpdaterVersion: "v1.4.0"}, newMemStore())
	if c.Info().SidecarReady {
		t.Fatal("v1.3.1 updater must not be ready for a new Core")
	}
	err := c.Trigger(context.Background(), "update", "v1.4.1")
	if err == nil || !strings.Contains(err.Error(), ControlPlanePairCapability) {
		t.Fatalf("Trigger error = %v, want missing pair capability", err)
	}
	if got := posts.Load(); got != 0 {
		t.Fatalf("legacy updater received %d POST(s); want zero", got)
	}
}

func TestMatchingUpdaterReleaseIsReadyAndAcceptsUpdate(t *testing.T) {
	var posts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(UpdaterRuntimeInfo{
			ProtocolVersion: UpdaterProtocolVersion,
			Version:         "v1.4.0",
			Capabilities:    []string{ControlPlanePairCapability},
		})
	})
	mux.HandleFunc("POST /update", func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	socket := serveUnixUpdater(t, mux)

	c := New(Config{SocketPath: socket, RequiredUpdaterVersion: "v1.4.0"}, newMemStore())
	if !c.Info().SidecarReady {
		t.Fatal("matching updater should be ready")
	}
	if err := c.Trigger(context.Background(), "update", "v1.4.1"); err != nil {
		t.Fatal(err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST count = %d, want one", got)
	}
}

func TestUpdaterReleaseMismatchFailsClosed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(UpdaterRuntimeInfo{
			ProtocolVersion: UpdaterProtocolVersion,
			Version:         "v1.4.1",
			Capabilities:    []string{ControlPlanePairCapability},
		})
	})
	socket := serveUnixUpdater(t, mux)
	if err := RequireUpdaterRelease(context.Background(), socket, "v1.4.0"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("RequireUpdaterRelease error = %v", err)
	}
}
