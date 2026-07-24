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

	"github.com/srcfl/ftw/go/internal/optimizercontract"
)

func TestOptimizerProtocolVersionKeepsContractAlias(t *testing.T) {
	if OptimizerProtocolVersion != optimizercontract.ProtocolVersion {
		t.Fatalf("OptimizerProtocolVersion = %d, want %d", OptimizerProtocolVersion, optimizercontract.ProtocolVersion)
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
	if _, err := transport.Health(ctx); err == nil || !strings.Contains(err.Error(), "start optimizer") {
		t.Fatalf("Health error = %v, want worker start failure", err)
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
	if _, err := transport.Health(ctx); err == nil || !strings.Contains(err.Error(), "protocol version 2") {
		t.Fatalf("Health error = %v, want protocol mismatch", err)
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
