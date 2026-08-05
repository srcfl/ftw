package mpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

func TestUnixTransportHandshakeAndRoundTrip(t *testing.T) {
	path := fmt.Sprintf("/tmp/ftw-opt-%d.sock", time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(path) })
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			scanner := bufio.NewScanner(conn)
			if scanner.Scan() {
				var request map[string]any
				_ = json.Unmarshal(scanner.Bytes(), &request)
				if request["type"] == "handshake" {
					_, _ = conn.Write([]byte(`{"name":"ftw-optimizer","version":"1.2.3","protocol_version":1,"features":["champion"]}` + "\n"))
				} else {
					_, _ = conn.Write([]byte(`{"ok":true}` + "\n"))
				}
			}
			_ = conn.Close()
		}
	}()

	transport := NewUnixTransport(path)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := transport.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "1.2.3" || info.ProtocolVersion != 1 || info.Transport != "unix" {
		t.Fatalf("unexpected handshake: %+v", info)
	}
	response, err := transport.RoundTrip(ctx, []byte(`{"schema_version":1}`))
	if err != nil || string(response) != `{"ok":true}` {
		t.Fatalf("round trip = %s, %v", response, err)
	}
}

func TestProcessTransportHealthPerformsCompatibleHandshake(t *testing.T) {
	if len(os.Args) > 0 && os.Args[len(os.Args)-1] == "process-health-helper" {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			_, _ = os.Stdout.WriteString(`{"name":"ftw-optimizer","version":"test","protocol_version":1,"features":["champion"]}` + "\n")
		}
		return
	}
	transport, err := NewProcessTransport(ProcessTransportConfig{
		Command: []string{os.Args[0], "-test.run=TestProcessTransportHealthPerformsCompatibleHandshake", "--", "process-health-helper"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := transport.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "ftw-optimizer" || info.Version != "test" || info.Transport != "process" {
		t.Fatalf("unexpected process handshake: %+v", info)
	}
}

func TestProcessTransportHealthReportsMissingWorker(t *testing.T) {
	transport, err := NewProcessTransport(ProcessTransportConfig{
		Command: []string{"/definitely/missing/ftw-optimizer-python"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// The interpreter is resolved before fork/exec, so an absent worker yields
	// the actionable errOptimizerWorkerMissing (which names the ftw-optimizer
	// sidecar) instead of a bare "start optimizer ... not found" that reads as a
	// missing core dependency.
	_, err = transport.Health(ctx)
	if err == nil || !errors.Is(err, errOptimizerWorkerMissing) {
		t.Fatalf("Health error = %v, want errOptimizerWorkerMissing", err)
	}
	if !strings.Contains(err.Error(), "ftw-optimizer sidecar") {
		t.Fatalf("Health error should name the ftw-optimizer sidecar, got: %v", err)
	}
}

func TestProcessTransportHealthRejectsIncompatibleHandshake(t *testing.T) {
	if len(os.Args) > 0 && os.Args[len(os.Args)-1] == "process-incompatible-helper" {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			_, _ = os.Stdout.WriteString(`{"name":"ftw-optimizer","version":"test","protocol_version":2,"features":["champion"]}` + "\n")
		}
		return
	}
	transport, err := NewProcessTransport(ProcessTransportConfig{
		Command: []string{os.Args[0], "-test.run=TestProcessTransportHealthRejectsIncompatibleHandshake", "--", "process-incompatible-helper"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = transport.Health(ctx)
	if err == nil || !strings.Contains(err.Error(), "protocol 2") {
		t.Fatalf("Health error = %v, want protocol mismatch", err)
	}
	// An operator reads this string in a badge tooltip, so it has to name the
	// fix. Core updates never move Optimizer, and Optimizer's own healthcheck
	// calls itself healthy — nothing else tells them what to do.
	if !strings.Contains(err.Error(), "update Optimizer in Update Center") {
		t.Errorf("handshake rejection must name the remedy, got %q", err)
	}
}

type fakeTransport struct {
	healthErr    error
	roundTripErr error
	reply        []byte
	calls        int
}

func (f *fakeTransport) RoundTrip(context.Context, []byte) ([]byte, error) {
	f.calls++
	return f.reply, f.roundTripErr
}
func (f *fakeTransport) Health(context.Context) (OptimizerRuntimeInfo, error) {
	return OptimizerRuntimeInfo{ProtocolVersion: 1, Features: []string{"champion"}}, f.healthErr
}

func TestAutoTransportFallsBackWhenFeatureIsMissing(t *testing.T) {
	primary := &fakeTransport{reply: []byte(`{"primary":true}`)}
	fallback := &fakeTransport{reply: []byte(`{"fallback":true}`)}
	transport := NewAutoTransport(primary, fallback)
	payload := []byte(`{"settings":{"scenario_policy":"multistage"}}`)
	response, err := transport.RoundTrip(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != `{"fallback":true}` || primary.calls != 0 || fallback.calls != 1 {
		t.Fatalf("response=%s primary=%d fallback=%d", response, primary.calls, fallback.calls)
	}
}
func (f *fakeTransport) Close() error { return nil }

// A containerized core has no bundled Python interpreter, so the process
// worker can never start. The error must name the remedy (run the sidecar),
// not surface a bare "python3: not found in $PATH" that reads as a missing
// core dependency. Regression guard for the masked-fallback bug.
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
		t.Fatal("expected an error when the optimizer worker interpreter is absent")
	}
	if !errors.Is(err, errOptimizerWorkerMissing) {
		t.Fatalf("error should wrap errOptimizerWorkerMissing, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ftw-optimizer sidecar") {
		t.Fatalf("error should point operators at the ftw-optimizer sidecar, got: %v", err)
	}
}

// With the `auto` transport (sidecar primary + process fallback), a down
// sidecar on a core build that ships no Python must still surface the
// actionable errOptimizerWorkerMissing through the auto layer — otherwise
// service.go's FallbackReason would report a bare exec error. This pins the
// missing-worker path all the way to the operator-facing reason string.
func TestAutoTransportSurfacesMissingWorkerFromProcessFallback(t *testing.T) {
	sidecar := &fakeTransport{healthErr: errors.New("dial optimizer socket: no such file")}
	worker, err := NewProcessTransport(ProcessTransportConfig{
		Command: []string{"ftw-nonexistent-optimizer-worker-xyz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close() })

	transport := NewAutoTransport(sidecar, worker)
	_, err = transport.RoundTrip(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error when the sidecar is down and no worker interpreter exists")
	}
	if !errors.Is(err, errOptimizerWorkerMissing) {
		t.Fatalf("auto transport should surface errOptimizerWorkerMissing from the process fallback, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ftw-optimizer sidecar") {
		t.Fatalf("fallback reason should name the ftw-optimizer sidecar, got: %v", err)
	}
}

func TestAutoTransportFallsBackWhenSidecarUnhealthy(t *testing.T) {
	primary := &fakeTransport{healthErr: errors.New("socket down")}
	fallback := &fakeTransport{reply: []byte(`{"fallback":true}`)}
	transport := NewAutoTransport(primary, fallback)
	response, err := transport.RoundTrip(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != `{"fallback":true}` || primary.calls != 0 || fallback.calls != 1 {
		t.Fatalf("response=%s primary=%d fallback=%d", response, primary.calls, fallback.calls)
	}
}

func TestAutoTransportReportsSidecarFailureBeforeProcessFailure(t *testing.T) {
	sidecarErr := errors.New("connection closed")
	processErr := errors.New(`start optimizer "python3": executable file not found`)
	primary := &fakeTransport{healthErr: sidecarErr}
	fallback := &fakeTransport{roundTripErr: processErr}
	transport := NewAutoTransport(primary, fallback)

	_, err := transport.RoundTrip(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("RoundTrip succeeded, want both transport failures")
	}
	if !errors.Is(err, sidecarErr) || !errors.Is(err, processErr) {
		t.Fatalf("RoundTrip error does not unwrap both failures: %v", err)
	}
	message := err.Error()
	sidecarAt := strings.Index(message, sidecarErr.Error())
	processAt := strings.Index(message, processErr.Error())
	if sidecarAt < 0 || processAt < 0 || sidecarAt >= processAt {
		t.Fatalf("RoundTrip error = %q, want sidecar cause before process failure", message)
	}
}

func TestAutoTransportReportsRequestFailureBeforeProcessFailure(t *testing.T) {
	sidecarErr := errors.New("connection closed")
	processErr := errors.New("process unavailable")
	primary := &fakeTransport{roundTripErr: sidecarErr}
	fallback := &fakeTransport{roundTripErr: processErr}
	transport := NewAutoTransport(primary, fallback)

	_, err := transport.RoundTrip(context.Background(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "optimizer sidecar failed: connection closed") {
		t.Fatalf("RoundTrip error = %v, want failed sidecar request as primary cause", err)
	}
}

func TestAutoTransportHealthReportsBothFailures(t *testing.T) {
	sidecarErr := errors.New("socket unavailable")
	processErr := errors.New("python unavailable")
	transport := NewAutoTransport(
		&fakeTransport{healthErr: sidecarErr},
		&fakeTransport{healthErr: processErr},
	)

	_, err := transport.Health(context.Background())
	if err == nil || !errors.Is(err, sidecarErr) || !errors.Is(err, processErr) {
		t.Fatalf("Health error = %v, want both failures", err)
	}
	if strings.Index(err.Error(), sidecarErr.Error()) >= strings.Index(err.Error(), processErr.Error()) {
		t.Fatalf("Health error = %q, want sidecar cause first", err)
	}
}

// The field failure mode: an Optimizer image older than #563 has no champion
// solver. Its own healthcheck predates the requirement, so Docker and the
// updater both call it healthy while Core quietly refuses to use it and plans
// on the Go fallback instead. The rejection string is the only thing an
// operator ever sees, so it has to name the fix.
func TestHandshakeRejectionNamesTheRemedy(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       []string
	}{
		{
			name: "old optimizer without champion",
			line: `{"name":"ftw-optimizer","version":"1.2.0","protocol_version":1,"features":["recourse"]}`,
			want: []string{"1.2.0", "too old", "update Optimizer in Update Center"},
		},
		{
			name: "champion missing and version unreported",
			line: `{"name":"ftw-optimizer","protocol_version":1,"features":[]}`,
			want: []string{"optimizer is too old", "update Optimizer in Update Center"},
		},
		{
			name: "protocol ahead of Core",
			line: `{"name":"ftw-optimizer","version":"9.0.0","protocol_version":99,"features":["champion"]}`,
			want: []string{"protocol 99", "accepts 1", "update Optimizer in Update Center"},
		},
		{
			name: "declared window sits entirely above Core",
			line: `{"name":"ftw-optimizer","version":"9.0.0","protocol_version":5,"protocol_min":4,"protocol_max":6,"features":["champion"]}`,
			want: []string{"protocol 4-6", "accepts 1", "update Optimizer in Update Center"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeOptimizerHandshake([]byte(tc.line), "unix")
			if err == nil {
				t.Fatal("incompatible handshake must be rejected")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestHandshakeAcceptsCurrentOptimizer(t *testing.T) {
	info, err := decodeOptimizerHandshake([]byte(`{"name":"ftw-optimizer","version":"1.3.2","protocol_version":1,"features":["champion","recourse"]}`), "unix")
	if err != nil {
		t.Fatalf("current optimizer must be accepted: %v", err)
	}
	if info.Version != "1.3.2" || info.Transport != "unix" {
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
			line:   `{"name":"ftw-optimizer","version":"1.3.2","protocol_version":1,"features":["champion"]}`,
			wantOK: true,
		},
		{
			name:   "newer optimizer that still speaks Core's version",
			line:   `{"name":"ftw-optimizer","version":"2.0.0","protocol_version":2,"protocol_min":1,"protocol_max":2,"features":["champion"]}`,
			wantOK: true,
		},
		{
			name:   "window touching Core only at its lower bound",
			line:   `{"name":"ftw-optimizer","version":"3.0.0","protocol_version":3,"protocol_min":1,"protocol_max":3,"features":["champion"]}`,
			wantOK: true,
		},
		{
			name:   "window entirely above Core",
			line:   `{"name":"ftw-optimizer","version":"9.0.0","protocol_version":7,"protocol_min":7,"protocol_max":9,"features":["champion"]}`,
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeOptimizerHandshake([]byte(tc.line), "unix")
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
