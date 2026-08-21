package ocpp

import (
	"fmt"
	"sync"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestProfilesIncludeSmartCharging(t *testing.T) {
	cases := map[string]bool{
		"Core,FirmwareManagement,SmartCharging":                                    true,
		"Core, LocalAuthListManagement, Reservation, SmartCharging, RemoteTrigger": true,
		"core,smartcharging":                    true, // vendors vary in case
		"Core,FirmwareManagement,RemoteTrigger": false,
		"":                                      false,
		"SmartChargingProfileMax":               false, // must not substring-match
	}
	for csv, want := range cases {
		if got := profilesIncludeSmartCharging(csv); got != want {
			t.Errorf("profilesIncludeSmartCharging(%q) = %v, want %v", csv, got, want)
		}
	}
}

// capabilityCP is a charge point that answers GetConfiguration with whatever
// feature profiles the test wants — including "no answer at all", which is
// what a charger that does not implement the key looks like on the wire.
type capabilityCP struct {
	mu       sync.Mutex
	profiles string
	omitKey  bool
	asked    int
}

func (c *capabilityCP) OnGetConfiguration(req *core.GetConfigurationRequest) (*core.GetConfigurationConfirmation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked++
	if c.omitKey {
		return core.NewGetConfigurationConfirmation(nil), nil
	}
	v := c.profiles
	return core.NewGetConfigurationConfirmation([]core.ConfigurationKey{
		{Key: "SupportedFeatureProfiles", Readonly: true, Value: &v},
	}), nil
}

func (c *capabilityCP) askedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

// The rest of core.ChargePointHandler, unused by these tests.
func (c *capabilityCP) OnChangeAvailability(*core.ChangeAvailabilityRequest) (*core.ChangeAvailabilityConfirmation, error) {
	return core.NewChangeAvailabilityConfirmation(core.AvailabilityStatusAccepted), nil
}
func (c *capabilityCP) OnChangeConfiguration(*core.ChangeConfigurationRequest) (*core.ChangeConfigurationConfirmation, error) {
	return core.NewChangeConfigurationConfirmation(core.ConfigurationStatusAccepted), nil
}
func (c *capabilityCP) OnClearCache(*core.ClearCacheRequest) (*core.ClearCacheConfirmation, error) {
	return core.NewClearCacheConfirmation(core.ClearCacheStatusAccepted), nil
}
func (c *capabilityCP) OnDataTransfer(*core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	return core.NewDataTransferConfirmation(core.DataTransferStatusAccepted), nil
}
func (c *capabilityCP) OnRemoteStartTransaction(*core.RemoteStartTransactionRequest) (*core.RemoteStartTransactionConfirmation, error) {
	return core.NewRemoteStartTransactionConfirmation(types.RemoteStartStopStatusAccepted), nil
}
func (c *capabilityCP) OnRemoteStopTransaction(*core.RemoteStopTransactionRequest) (*core.RemoteStopTransactionConfirmation, error) {
	return core.NewRemoteStopTransactionConfirmation(types.RemoteStartStopStatusAccepted), nil
}
func (c *capabilityCP) OnReset(*core.ResetRequest) (*core.ResetConfirmation, error) {
	return core.NewResetConfirmation(core.ResetStatusAccepted), nil
}
func (c *capabilityCP) OnUnlockConnector(*core.UnlockConnectorRequest) (*core.UnlockConnectorConfirmation, error) {
	return core.NewUnlockConnectorConfirmation(core.UnlockStatusUnlocked), nil
}

// waitSteerable polls the snapshot until the capability verdict lands.
func waitSteerable(t *testing.T, srv *Server, id string) ChargerView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v := srv.Handler().Snapshot()[id]
		if v.Steerable != nil {
			return v
		}
		time.Sleep(25 * time.Millisecond)
	}
	return srv.Handler().Snapshot()[id]
}

func connectCapabilityCP(t *testing.T, port int, id string, cp *capabilityCP) ocpp16.ChargePoint {
	t.Helper()
	client := ocpp16.NewChargePoint(id, nil, nil)
	client.SetCoreHandler(cp)
	if err := client.Start(fmt.Sprintf("ws://127.0.0.1:%d", port)); err != nil {
		t.Fatalf("connect: %v", err)
	}
	var once sync.Once
	t.Cleanup(func() { once.Do(client.Stop) })
	if _, err := client.BootNotification("Dawn", "Charge Amps"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	return client
}

// A charger advertising SmartCharging is recorded as steerable, and the probe
// stops asking once it has an answer.
func TestCapabilityProbeRecordsSmartCharging(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "steerable-cp")
	defer srv.Stop()

	cp := &capabilityCP{profiles: "Core,FirmwareManagement,SmartCharging"}
	connectCapabilityCP(t, port, "steerable-cp", cp)

	v := waitSteerable(t, srv, "steerable-cp")
	if v.Steerable == nil || !*v.Steerable {
		t.Fatalf("expected steerable=true, got %+v", v)
	}
	if v.FeatureProfiles != "Core,FirmwareManagement,SmartCharging" {
		t.Errorf("raw profiles not recorded: %+v", v)
	}
	// Connect and boot both call maybeProbeCapability milliseconds apart.
	// The in-flight marker must collapse that into a single request, and
	// the stored answer must stop any later one.
	if n := cp.askedCount(); n != 1 {
		t.Errorf("connect+boot should ask exactly once, asked %d times", n)
	}
	srv.Handler().maybeProbeCapability("steerable-cp")
	time.Sleep(200 * time.Millisecond)
	if n := cp.askedCount(); n != 1 {
		t.Errorf("probe re-asked after an answer was stored: %d", n)
	}
}

// A probe still in flight when the socket drops must not wedge the marker:
// the reconnect is exactly when the charger should be asked again.
func TestCapabilityProbeMarkerClearedOnDisconnect(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "flappy")
	defer srv.Stop()

	h := srv.Handler()
	// Simulate "probe fired, no answer yet" without a live charge point.
	h.mu.Lock()
	h.probing["flappy"] = true
	h.mu.Unlock()

	h.OnDisconnect("flappy")

	h.mu.Lock()
	stuck := h.probing["flappy"]
	h.mu.Unlock()
	if stuck {
		t.Error("disconnect must clear an in-flight probe marker")
	}
	_ = port
}

// A charger without SmartCharging is recorded as telemetry-only — the UI
// warns rather than the code blocking, so control is still attempted.
func TestCapabilityProbeRecordsTelemetryOnly(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "meter-only")
	defer srv.Stop()

	connectCapabilityCP(t, port, "meter-only", &capabilityCP{profiles: "Core,FirmwareManagement"})

	v := waitSteerable(t, srv, "meter-only")
	if v.Steerable == nil || *v.Steerable {
		t.Fatalf("expected steerable=false, got %+v", v)
	}
	if v.FeatureProfiles != "Core,FirmwareManagement" {
		t.Errorf("raw profiles not recorded: %+v", v)
	}
}

// A charger that answers without the key leaves the verdict unknown — absent
// from the JSON entirely, which the UI renders as "not reported" rather than
// claiming the charger cannot be steered.
func TestCapabilityProbeUnknownWhenKeyMissing(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "quiet-cp")
	defer srv.Stop()

	cp := &capabilityCP{omitKey: true}
	connectCapabilityCP(t, port, "quiet-cp", cp)
	time.Sleep(600 * time.Millisecond)

	v := srv.Handler().Snapshot()["quiet-cp"]
	if v.Steerable != nil {
		t.Fatalf("expected unknown steerability, got %v", *v.Steerable)
	}
	if v.FeatureProfiles != "" {
		t.Errorf("no profiles should be recorded, got %q", v.FeatureProfiles)
	}
	// An unanswered probe must not wedge the in-flight marker: the charger
	// gets asked again next time, which is how a firmware update that adds
	// the key is ever noticed.
	askedBefore := cp.askedCount()
	srv.Handler().maybeProbeCapability("quiet-cp")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && cp.askedCount() == askedBefore {
		time.Sleep(25 * time.Millisecond)
	}
	if cp.askedCount() <= askedBefore {
		t.Errorf("probe should retry while the verdict is unknown, still %d", cp.askedCount())
	}
}
