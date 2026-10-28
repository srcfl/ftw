package ocpp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

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

// A vehicleIdentified callback that stalls must not stop commands to its own
// charger, and a callback that itself commands that charger must not
// deadlock on the slot lock.
func TestStalledVehicleCallbackCanCommandItsCharger(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage")
	garage, fake, _ := connectCharger(t, srv, port, "garage")

	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	reentry := make(chan error, 1)
	srv.Handler().SetVehicleIdentified(func(chargerID, vehicleID, source string) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		reentry <- srv.Command(ctx, "garage", []byte(`{"action":"ev_set_current","power_w":4140}`))
		cancel()
		<-release
	})
	var releaseOnce sync.Once
	unstall := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unstall)

	go func() { _, _ = garage.StartTransaction(1, "RFID-1", 0, types.NewDateTime(time.Now())) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("vehicleIdentified never fired")
	}
	select {
	case err := <-reentry:
		if err != nil {
			t.Fatalf("command from inside the callback failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command from inside the callback deadlocked")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Command(ctx, "garage", []byte(`{"action":"ev_set_current","power_w":2300}`)); err != nil {
		t.Fatalf("command to the charger whose callback stalls failed: %v", err)
	}
	if n := fake.count(); n < 2 {
		t.Fatalf("charger received %d profiles, want at least 2", n)
	}
	unstall()
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback ran %d times, want 1", got)
	}
}

// Work queued through later from another goroutine while a boundCall holds
// the slot runs once, after the lock is free. With no boundCall it runs at
// once, and the queue keeps no key behind.
func TestLaterRunsAfterTheSlotIsFree(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage")
	_, _, _ = connectCharger(t, srv, port, "garage")
	alias, err := srv.sockets.currentID("garage")
	if err != nil {
		t.Fatal(err)
	}
	holding := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _ = boundCall(srv.sockets, alias, func(string) (bool, error) {
			close(holding)
			<-release
			return true, nil
		})
		close(done)
	}()
	<-holding
	var ran atomic.Int32
	lockFree := make(chan bool, 1)
	b := srv.sockets.lookup(alias)
	srv.sockets.later("garage", func() {
		ran.Add(1)
		ok := b.slot.mu.TryLock()
		if ok {
			b.slot.mu.Unlock()
		}
		lockFree <- ok
	})
	if ran.Load() != 0 {
		t.Fatal("later ran while the slot was held")
	}
	close(release)
	<-done
	select {
	case ok := <-lockFree:
		if !ok {
			t.Fatal("queued work ran with the slot lock held")
		}
	case <-time.After(time.Second):
		t.Fatal("queued work was lost")
	}
	if got := ran.Load(); got != 1 {
		t.Fatalf("queued work ran %d times, want 1", got)
	}

	var now atomic.Bool
	srv.sockets.later("garage", func() { now.Store(true) })
	if !now.Load() {
		t.Fatal("later did not run at once with no boundCall active")
	}
	srv.sockets.mu.Lock()
	n := len(srv.sockets.after)
	srv.sockets.mu.Unlock()
	if n != 0 {
		t.Fatalf("after map kept %d keys", n)
	}
}
