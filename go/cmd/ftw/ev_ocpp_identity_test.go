package main

import (
	"testing"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEVSampleUsesCurrentOCPPBootHardware(t *testing.T) {
	h := ocpp.NewHandler(telemetry.NewStore(), 60)
	h.SetApprovedIDs([]string{"garage"})
	h.OnConnect("garage")
	if got := currentOCPPDeviceID(h, "garage"); got != "" {
		t.Fatalf("pre-boot name became identity: %q", got)
	}
	boot := &core.BootNotificationRequest{ChargePointVendor: "Easee", ChargePointModel: "Home", ChargePointSerialNumber: "A"}
	if _, err := h.OnBootNotification("garage", boot); err != nil {
		t.Fatal(err)
	}
	if got := currentOCPPDeviceID(h, "garage"); got != "easee:A" {
		t.Fatalf("hardware not bound: %q", got)
	}
	h.OnDisconnect("garage")
	if got := currentOCPPDeviceID(h, "garage"); got != "" {
		t.Fatalf("offline identity trusted: %q", got)
	}
	h.OnConnect("garage")
	if got := currentOCPPDeviceID(h, "garage"); got != "" {
		t.Fatalf("reconnect reused previous boot identity: %q", got)
	}
	boot.ChargePointSerialNumber = "B"
	h.OnBootNotification("garage", boot)
	if got := currentOCPPDeviceID(h, "garage"); got != "easee:B" {
		t.Fatalf("replacement inherited prior identity: %q", got)
	}
	boot.ChargePointSerialNumber = ""
	h.OnBootNotification("garage", boot)
	if got := currentOCPPDeviceID(h, "garage"); got != "" {
		t.Fatalf("missing serial reused prior identity: %q", got)
	}
	h.OnConnect("pending")
	boot.ChargePointSerialNumber = "C"
	h.OnBootNotification("pending", boot)
	if got := currentOCPPDeviceID(h, "pending"); got != "" {
		t.Fatalf("unadopted hardware trusted: %q", got)
	}
}
