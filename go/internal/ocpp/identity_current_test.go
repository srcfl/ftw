package ocpp

import (
	"testing"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestCurrentIdentityRequiresBootOnThisConnection(t *testing.T) {
	for _, dialect := range []string{"1.6", "2.0.1"} {
		t.Run(dialect, func(t *testing.T) {
			h := NewHandler(telemetry.NewStore(), 60)
			h.SetApprovedIDs([]string{"garage"})
			boot := func(serial string) {
				t.Helper()
				if dialect == "1.6" {
					_, err := h.OnBootNotification("garage", &core.BootNotificationRequest{ChargePointVendor: "Easee", ChargePointModel: "Home", ChargePointSerialNumber: serial})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					req := provisioning.NewBootNotificationRequest(provisioning.BootReasonPowerUp, "Home", "Easee")
					req.ChargingStation.SerialNumber = serial
					_, err := (&handlerV201{h}).OnBootNotification("garage", req)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			h.OnConnect("garage")
			boot("A")
			if got, ok := h.CurrentIdentity("garage"); !ok || got.Serial != "A" {
				t.Fatalf("boot not current: %+v %v", got, ok)
			}
			h.OnDisconnect("garage")
			if _, ok := h.CurrentIdentity("garage"); ok {
				t.Fatal("offline identity current")
			}
			h.OnConnect("garage")
			if _, ok := h.CurrentIdentity("garage"); ok {
				t.Fatal("previous socket's boot trusted on reconnect")
			}
			if got := h.Identities(); len(got) != 1 || got[0].Serial != "A" {
				t.Fatal("historical UI identity lost")
			}
			boot("B")
			if got, ok := h.CurrentIdentity("garage"); !ok || got.Serial != "B" {
				t.Fatalf("new hardware not current: %+v %v", got, ok)
			}
		})
	}
}
