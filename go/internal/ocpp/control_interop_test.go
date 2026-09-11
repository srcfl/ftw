package ocpp

import (
	"context"
	"testing"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Both cases here were found by running the branch against Sourceful's device
// simulator rather than by reading the specification, which is why they are
// pinned: each one let FTW believe it had imposed a limit it had not.

// setCurrent11kW is the ordinary dispatch command, at a rate the fixtures'
// chargers accept.
func setCurrent11kW(t *testing.T) []byte {
	t.Helper()
	return mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     11040.0,
		"voltage":     230.0,
		"site_phases": 3,
	})
}

// The profile must not be Absolute.
//
// FTW's schedule has one period at second 0 and no end. Absolute expresses
// that only with a startSchedule timestamp; the specification says an absolute
// schedule without one is relative to the start of charging anyway, so the two
// spellings mean the same thing — but a charger that parses the missing
// timestamp strictly finds no valid start, treats the profile as not yet
// active, and answers Accepted while charging on at full rate.
func TestChargingProfileIsRelativeSoItCannotBeReadAsNotYetStarted(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage")
	t.Cleanup(srv.Stop)
	_, fake, _ := connectCharger(t, srv, port, "garage")

	if err := srv.Command(context.Background(), "garage", setCurrent11kW(t)); err != nil {
		t.Fatalf("set current: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.profiles) == 0 {
		t.Fatal("charger received no charging profile")
	}
	got := fake.profiles[len(fake.profiles)-1].ChargingProfile
	if got.ChargingProfileKind != types.ChargingProfileKindRelative {
		t.Errorf("profile kind: got %v, want Relative — Absolute needs a startSchedule FTW does not send",
			got.ChargingProfileKind)
	}
	if got.ChargingSchedule != nil && got.ChargingSchedule.StartSchedule != nil {
		t.Error("a relative schedule must carry no startSchedule")
	}
}

// OCPP 1.6 permits a TxDefaultProfile on connector 0 — it is how a profile is
// applied to every connector — but some chargers read the connector-0 rule as
// ChargePointMaxProfile-only and refuse it. Refusing means no limit at all, so
// FTW retries on the first connector rather than leaving the charger unsteered.
func TestChargerRefusingConnectorZeroIsRetriedOnConnectorOne(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage")
	t.Cleanup(srv.Stop)
	_, fake, _ := connectCharger(t, srv, port, "garage")

	fake.mu.Lock()
	fake.statusFn = func(req *smartcharging.SetChargingProfileRequest) smartcharging.ChargingProfileStatus {
		if req.ConnectorId == 0 {
			return smartcharging.ChargingProfileStatusRejected
		}
		return smartcharging.ChargingProfileStatusAccepted
	}
	fake.mu.Unlock()

	if err := srv.Command(context.Background(), "garage", setCurrent11kW(t)); err != nil {
		t.Fatalf("a charger that only accepts a real connector was left unsteered: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.profiles) != 2 {
		t.Fatalf("got %d profiles, want the charge-point-wide try then the connector retry", len(fake.profiles))
	}
	if fake.profiles[0].ConnectorId != 0 {
		t.Errorf("first attempt went to connector %d, want the charge-point-wide 0", fake.profiles[0].ConnectorId)
	}
	if fake.profiles[1].ConnectorId != 1 {
		t.Errorf("retry went to connector %d, want 1", fake.profiles[1].ConnectorId)
	}
}

// A charger that refuses both is a charger FTW cannot steer, and that has to
// surface as an error rather than a silent success.
func TestChargerRefusingEveryConnectorFails(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage")
	t.Cleanup(srv.Stop)
	_, fake, _ := connectCharger(t, srv, port, "garage")

	fake.setStatus(smartcharging.ChargingProfileStatusRejected)

	if err := srv.Command(context.Background(), "garage", setCurrent11kW(t)); err == nil {
		t.Fatal("a charger refusing every connector reported success")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.profiles) != 2 {
		t.Errorf("got %d attempts, want exactly one retry and no more", len(fake.profiles))
	}
}
