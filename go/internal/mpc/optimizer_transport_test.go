package mpc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/optimizercontract"
)

func TestOptimizerProtocolVersionKeepsContractAlias(t *testing.T) {
	if OptimizerProtocolVersion != optimizercontract.ProtocolVersion {
		t.Fatalf("OptimizerProtocolVersion = %d, want %d", OptimizerProtocolVersion, optimizercontract.ProtocolVersion)
	}
	if OptimizerProtocolMinVersion != optimizercontract.MinProtocolVersion {
		t.Fatalf("OptimizerProtocolMinVersion = %d, want %d", OptimizerProtocolMinVersion, optimizercontract.MinProtocolVersion)
	}
	// Diagnostics read the bounds from components; the transport enforces them
	// from optimizercontract. If those ever disagree, /api/components would
	// advertise a window Core does not actually accept.
	if components.OptimizerProtocolVersion != OptimizerProtocolVersion ||
		components.OptimizerProtocolMinVersion != OptimizerProtocolMinVersion {
		t.Fatalf("components window = %d-%d, transport window = %d-%d",
			components.OptimizerProtocolMinVersion, components.OptimizerProtocolVersion,
			OptimizerProtocolMinVersion, OptimizerProtocolVersion)
	}
	if OptimizerProtocolMinVersion > OptimizerProtocolVersion {
		t.Fatalf("protocol window %d-%d is inverted", OptimizerProtocolMinVersion, OptimizerProtocolVersion)
	}
}

func TestContextGateDropsCanceledWaiter(t *testing.T) {
	gate := newContextGate()
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		close(waiting)
		errCh <- gate.acquire(ctx)
	}()
	<-waiting
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked")
	}

	gate.release()
	acquireCtx, acquireCancel := context.WithTimeout(context.Background(), time.Second)
	defer acquireCancel()
	if err := gate.acquire(acquireCtx); err != nil {
		t.Fatalf("gate stayed occupied after canceled waiter: %v", err)
	}
	gate.release()
}

func TestProcessTransportRejectsCanceledContextBeforeWorkerLookup(t *testing.T) {
	transport, err := NewProcessTransport(ProcessTransportConfig{
		Command: []string{"ftw-worker-that-does-not-exist"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = transport.RoundTrip(ctx, []byte(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip error = %v, want context.Canceled", err)
	}
}

func TestProcessTransportWriteCancellationRestartsWorker(t *testing.T) {
	if len(os.Args) >= 2 && os.Args[len(os.Args)-2] == "process-write-helper" {
		marker := os.Args[len(os.Args)-1]
		first, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_ = first.Close()
			// The first worker never reads stdin. Its parent must kill it when the
			// request deadline expires and the pipe write is still blocked.
			time.Sleep(10 * time.Second)
			return
		}
		if !errors.Is(err, os.ErrExist) {
			os.Exit(2)
		}
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			_, _ = os.Stdout.WriteString(`{"ok":true}` + "\n")
		}
		return
	}

	marker := t.TempDir() + "/worker-started"
	transport, err := NewProcessTransport(ProcessTransportConfig{
		Command: []string{os.Args[0], "-test.run=TestProcessTransportWriteCancellationRestartsWorker", "--", "process-write-helper", marker},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	started := time.Now()
	_, err = transport.RoundTrip(ctx, bytes.Repeat([]byte("x"), 2<<20))
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RoundTrip error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("blocked write returned after %v, want at most 2s", elapsed)
	}

	restartCtx, restartCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer restartCancel()
	response, err := transport.RoundTrip(restartCtx, []byte(`{}`))
	if err != nil {
		t.Fatalf("RoundTrip after canceled write: %v", err)
	}
	if string(response) != `{"ok":true}` {
		t.Fatalf("RoundTrip after canceled write = %s, want healthy worker response", response)
	}
}

// An install missing the bundled worker must say that the Go planner is used,
// not surface a bare "not found in $PATH" that reads as a missing Core
// dependency. Regression guard for the masked-fallback bug.
func TestProcessTransportReportsMissingWorkerActionably(t *testing.T) {
	transport, err := NewProcessTransport(ProcessTransportConfig{
		Command: []string{"ftw-nonexistent-optimizer-worker-xyz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })

	_, err = transport.RoundTrip(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error when the optimizer worker is absent")
	}
	if !errors.Is(err, errOptimizerWorkerMissing) {
		t.Fatalf("error should wrap errOptimizerWorkerMissing, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Go planner") {
		t.Fatalf("error should say the Go planner is used, got: %v", err)
	}
}

// A worker that does not match Core is refused, and Core quietly plans on the
// Go fallback instead. The rejection string is the only thing an operator ever
// sees, so it has to name the fix. The worker ships with Core, so the fix is a
// Core update or reinstall; there is no separate optimizer update.
func TestHandshakeRejectionNamesTheRemedy(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       []string
	}{
		{
			name: "old optimizer without champion",
			line: `{"name":"ftw-solver","version":"1.2.0","protocol_version":1,"features":["ev_duty"]}`,
			want: []string{"1.2.0", "too old", "ftw update"},
		},
		{
			name: "champion missing and version unreported",
			line: `{"name":"ftw-solver","protocol_version":1,"features":[]}`,
			want: []string{"optimizer is too old", "ftw update"},
		},
		{
			name: "protocol ahead of Core",
			line: `{"name":"ftw-solver","version":"9.0.0","protocol_version":99,"features":["champion"]}`,
			want: []string{"protocol 99", "accepts 1", "ftw update"},
		},
		{
			name: "declared window sits entirely above Core",
			line: `{"name":"ftw-solver","version":"9.0.0","protocol_version":5,"protocol_min":4,"protocol_max":6,"features":["champion"]}`,
			want: []string{"protocol 4-6", "accepts 1", "ftw update"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeOptimizerHandshakeFor([]byte(tc.line), "process", "ftw-solver")
			if err == nil {
				t.Fatal("incompatible handshake must be rejected")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "Update Center") {
				t.Errorf("error %q names the retired Update Center", err)
			}
		})
	}
}

func TestHandshakeAcceptsCurrentOptimizer(t *testing.T) {
	info, err := decodeOptimizerHandshakeFor([]byte(`{"name":"ftw-solver","version":"1.3.2","protocol_version":1,"features":["champion","ev_duty"]}`), "process", "ftw-solver")
	if err != nil {
		t.Fatalf("current optimizer must be accepted: %v", err)
	}
	if info.Version != "1.3.2" || info.Transport != "process" {
		t.Fatalf("info = %+v", info)
	}
}

// The point of the window is that neither side has to move in lockstep. These
// cases are what a future protocol bump has to keep working; today every
// optimizer reports a single version, so they all collapse to "1".
func TestHandshakeAcceptsAnyOverlappingProtocolWindow(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		wantOK     bool
	}{
		{
			name:   "single version matching Core",
			line:   `{"name":"ftw-solver","version":"1.3.2","protocol_version":1,"features":["champion"]}`,
			wantOK: true,
		},
		{
			name:   "newer optimizer that still speaks Core's version",
			line:   `{"name":"ftw-solver","version":"2.0.0","protocol_version":2,"protocol_min":1,"protocol_max":2,"features":["champion"]}`,
			wantOK: true,
		},
		{
			name:   "window touching Core only at its lower bound",
			line:   `{"name":"ftw-solver","version":"3.0.0","protocol_version":3,"protocol_min":1,"protocol_max":3,"features":["champion"]}`,
			wantOK: true,
		},
		{
			name:   "window entirely above Core",
			line:   `{"name":"ftw-solver","version":"9.0.0","protocol_version":7,"protocol_min":7,"protocol_max":9,"features":["champion"]}`,
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeOptimizerHandshakeFor([]byte(tc.line), "process", "ftw-solver")
			if tc.wantOK && err != nil {
				t.Fatalf("overlapping window must be accepted: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("disjoint window must be rejected")
			}
		})
	}
}

func TestProtocolWindowDefaultsToTheReportedVersion(t *testing.T) {
	// Every optimizer released so far omits the bounds entirely.
	info := OptimizerRuntimeInfo{ProtocolVersion: 4}
	if min, max := info.protocolWindow(); min != 4 || max != 4 {
		t.Fatalf("window = %d-%d, want 4-4", min, max)
	}
	// A producer that swaps the bounds should not silently exclude itself.
	info = OptimizerRuntimeInfo{ProtocolVersion: 2, ProtocolMin: 3, ProtocolMax: 1}
	if min, max := info.protocolWindow(); min != 1 || max != 3 {
		t.Fatalf("window = %d-%d, want 1-3", min, max)
	}
}
