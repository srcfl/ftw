package ocpp

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// While the request pump is stalled, the control loop keeps commanding each
// charger with a short deadline. Those commands must not leave a send behind
// each, or the charger is replayed every obsolete limit once the pump clears,
// one round trip each, before the current one. A fuse cut would wait behind
// them.
func TestStalledPumpDoesNotReplayObsoleteProfiles(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage", "carport")
	_, carport, _ := connectCharger(t, srv, port, "carport")
	_, _, _ = connectCharger(t, srv, port, "garage")

	// Hold garage's slot lock, as a stalled handler did before E2. The pump
	// blocks writing garage's next request, and stops for every charger.
	alias, err := srv.sockets.currentID("garage")
	if err != nil {
		t.Fatal(err)
	}
	holding := make(chan struct{})
	release := make(chan struct{})
	released := make(chan struct{})
	go func() {
		defer close(released)
		_, _ = boundCall(srv.sockets, alias, func(string) (bool, error) {
			close(holding)
			<-release
			return true, nil
		})
	}()
	<-holding
	unstall := func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-released
	}
	t.Cleanup(unstall)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_ = srv.Command(ctx, "garage", []byte(`{"action":"ev_set_current","power_w":4140}`))
	cancel()

	before := carport.count()
	base := runtime.NumGoroutine()
	const ticks = 40
	for i := 0; i < ticks; i++ {
		w := 11040
		if i%2 == 1 {
			w = 9000
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := srv.Command(ctx, "carport", []byte(fmt.Sprintf(`{"action":"ev_set_current","power_w":%d}`, w)))
		cancel()
		// No verdict, not a refusal: core must not count the charger as
		// refusing because another charger stalled the pump.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("tick %d: err = %v, want context.DeadlineExceeded", i, err)
		}
	}
	if grew := runtime.NumGoroutine() - base; grew > 5 {
		t.Errorf("goroutines grew by %d during the stall, want at most one send per charger", grew)
	}

	unstall()
	// The fuse cut that follows the stall. 4140 W over three 230 V phases
	// is the 6 A minimum.
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Command(ctx, "carport", []byte(`{"action":"ev_set_current","power_w":4140}`)); err != nil {
		t.Fatalf("command after the stall: %v", err)
	}
	if got := carport.lastLimit(t); got != 6 {
		t.Fatalf("carport ends at %.1f A, want 6 A", got)
	}
	// One profile from the stall reached the charger, then the current one.
	if n := carport.count() - before; n > 2 {
		t.Fatalf("carport received %d profiles for %d stalled ticks and one live command, want at most 2", n, ticks)
	}
}
