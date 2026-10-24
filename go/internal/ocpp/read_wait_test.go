package ocpp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// An idle charger that sends only heartbeats must keep its socket. The
// library's own limit was 60 s, the same as a 60 s heartbeat, so the socket
// was closed as the heartbeat arrived. The limit now follows the heartbeat
// interval: with a 1 s heartbeat a silent charger is closed after about 5 s,
// not after 60 s.
func TestSilentChargerSocketFollowsHeartbeat(t *testing.T) {
	port := freePort(t)
	cfg := &Config{Enabled: true, Bind: "127.0.0.1", Port: port, HeartbeatIntervalS: 1, ApprovedIDs: []string{"garage"}}
	srv, err := Start(context.Background(), cfg, telemetry.NewStore())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Stop)
	waitForListener(t, port)

	dialer := websocket.Dialer{Subprotocols: []string{"ocpp1.6"}, HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d%sgarage", port, srv.Path()), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	start := time.Now()
	_ = conn.SetReadDeadline(start.Add(12 * time.Second))
	for {
		// Core may probe the charger; a silent charger never answers.
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	if got := time.Since(start); got < 4*time.Second || got > 8*time.Second {
		t.Fatalf("silent charger socket closed after %v, want about 5 s (five 1 s heartbeats)", got.Round(100*time.Millisecond))
	}
	if got := socketReadWait(30); got != 150*time.Second {
		t.Fatalf("default 30 s heartbeat gives %v, want 150 s", got)
	}
}
