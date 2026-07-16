package mpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

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

type fakeTransport struct {
	healthErr error
	reply     []byte
	calls     int
}

func (f *fakeTransport) RoundTrip(context.Context, []byte) ([]byte, error) {
	f.calls++
	return f.reply, nil
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
