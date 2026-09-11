package ocpp

import (
	"fmt"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A device row is a statement that this hardware is part of the site, so
// quarantine covers it: a pending charge point's identity is recorded and
// shown, and never becomes a device.
func TestIdentityIsWithheldFromPendingChargers(t *testing.T) {
	h := NewHandler(telemetry.NewStore(), 60)
	h.SetApprovedIDs([]string{"garage"})

	var reported []ChargerIdentity
	h.SetIdentityReported(func(i ChargerIdentity) {
		reported = append(reported, i)
	})

	for _, id := range []string{"garage", "intruder"} {
		s := h.state(id)
		h.mu.Lock()
		s.vendor, s.model, s.serial = "Charge Amps", "Halo", "SN-"+id
		h.mu.Unlock()
		h.noteIdentity(id)
	}

	if len(reported) != 1 || reported[0].ID != "garage" {
		t.Fatalf("identity reported for %v, want only the adopted charger", reported)
	}
	if reported[0].Serial != "SN-garage" {
		t.Errorf("serial: got %q, want SN-garage", reported[0].Serial)
	}

	ids := h.Identities()
	if len(ids) != 1 || ids[0].ID != "garage" {
		t.Fatalf("Identities returned %v, want only the adopted charger", ids)
	}
}

// Adoption happens long after a BootNotification — an operator binds a pending
// charger to a loadpoint — and the charger will not boot again for it. The
// catch-up path is Identities, so it must return a charger that booted while
// it was still pending.
func TestIdentitiesCatchUpAfterAdoption(t *testing.T) {
	h := NewHandler(telemetry.NewStore(), 60)

	s := h.state("garage")
	h.mu.Lock()
	s.vendor, s.serial = "Easee", "EH123456"
	h.mu.Unlock()
	h.noteIdentity("garage")

	if got := h.Identities(); len(got) != 0 {
		t.Fatalf("a pending charger should have no identity to register, got %v", got)
	}

	h.SetApprovedIDs([]string{"garage"})
	got := h.Identities()
	if len(got) != 1 || got[0].Serial != "EH123456" {
		t.Fatalf("after adoption: got %v, want the charger that already booted", got)
	}
}

// A charger that has connected but not booted has nothing hardware-stable to
// key on. Registering it then would create a row keyed on the name it dialled
// with, which the real serial could never replace.
func TestIdentityWaitsForBoot(t *testing.T) {
	h := NewHandler(telemetry.NewStore(), 60)
	h.SetApprovedIDs([]string{"garage"})
	h.OnConnect("garage")

	if got := h.Identities(); len(got) != 0 {
		t.Fatalf("connected but not booted should yield no identity, got %v", got)
	}
}

// 1.6 has two serial fields and shipped firmware disagrees about which to
// fill, so losing the deprecated one loses the only stable identity some
// chargers ever report.
func TestBootNotificationTakesEitherSerialField(t *testing.T) {
	tests := []struct {
		name  string
		props []func(*core.BootNotificationRequest)
		want  string
	}{
		{
			name:  "chargePointSerialNumber",
			props: []func(*core.BootNotificationRequest){func(r *core.BootNotificationRequest) { r.ChargePointSerialNumber = "CP-1" }},
			want:  "CP-1",
		},
		{
			name:  "deprecated chargeBoxSerialNumber",
			props: []func(*core.BootNotificationRequest){func(r *core.BootNotificationRequest) { r.ChargeBoxSerialNumber = "BOX-1" }},
			want:  "BOX-1",
		},
		{
			name: "both, the current field wins",
			props: []func(*core.BootNotificationRequest){func(r *core.BootNotificationRequest) {
				r.ChargePointSerialNumber = "CP-1"
				r.ChargeBoxSerialNumber = "BOX-1"
			}},
			want: "CP-1",
		},
		{name: "neither", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := "boot-" + tc.name
			port, srv := startServer(t, telemetry.NewStore(), id)
			t.Cleanup(srv.Stop)

			cp := ocpp16.NewChargePoint(id, nil, nil)
			if err := cp.Start(fmt.Sprintf("ws://127.0.0.1:%d", port)); err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(cp.Stop)

			if _, err := cp.BootNotification("Halo", "Charge Amps", tc.props...); err != nil {
				t.Fatalf("boot: %v", err)
			}

			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if v, ok := srv.Handler().Snapshot()[id]; ok && v.Vendor != "" {
					if v.Serial != tc.want {
						t.Fatalf("serial: got %q, want %q", v.Serial, tc.want)
					}
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("charger never appeared in the snapshot")
		})
	}
}
