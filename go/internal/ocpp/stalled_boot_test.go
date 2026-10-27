package ocpp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// An identityReported callback (main.go wires it to a SQLite RegisterDevice)
// that stalls during a BootNotification must not hold the charger's slot
// lock. If it did, the SDK's single request pump would block writing to that
// charger, every other charger would lose its commands, and once the 20-slot
// request channel filled, Server.Command would block past its own deadline.
func TestStalledBootCallbackDoesNotBlockCommands(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage", "carport")
	_, _, _ = connectCharger(t, srv, port, "carport")
	garage, _, _ := connectCharger(t, srv, port, "garage")

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv.Handler().SetIdentityReported(func(ident ChargerIdentity) {
		if ident.ID != "garage" {
			return
		}
		once.Do(func() { close(entered) })
		<-release // a stalled state.db write
	})
	var releaseOnce sync.Once
	unstall := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unstall)

	go func() { _, _ = garage.BootNotification("Dawn", "Charge Amps") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second boot never reached identityReported")
	}

	cmd := []byte(`{"action":"ev_set_current","power_w":4140}`)
	// One command to the stalled charger: the pump takes it and blocks in Write.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_ = srv.Command(ctx, "garage", cmd)
	cancel()

	// A healthy charger on the same server now gets nothing through.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	err := srv.Command(ctx, "carport", cmd)
	cancel()
	if err != nil {
		t.Errorf("healthy charger lost its command while another charger's Boot callback stalled: %v", err)
	}

	// Keep sending with a short deadline, as the control loop does each tick.
	for i := 1; i <= 25; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		done := make(chan error, 1)
		go func() { done <- srv.Command(ctx, "carport", cmd) }()
		select {
		case <-done:
			cancel()
		case <-time.After(2 * time.Second):
			cancel()
			unstall()
			<-done
			t.Fatalf("command %d still blocked 2 s after its 50 ms deadline", i)
		}
	}
}
