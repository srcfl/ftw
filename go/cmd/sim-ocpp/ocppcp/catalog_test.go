package ocppcp

import (
	"testing"
	"time"
)

func TestInventoryCoversEvifyRange(t *testing.T) {
	want := []string{
		"easee-charge-up", "easee-charge-max",
		"zaptec-go", "zaptec-go-2",
		"nexblue-edge-2", "go-e-gemini-flex",
		"charge-amps-luna", "charge-amps-halo", "charge-amps-aura", "charge-amps-dawn",
		"wallbox-pulsar-max", "defa-power",
		"tesla-wall-connector",
	}
	got := Inventory()
	if len(got) != len(want) {
		t.Fatalf("inventory len=%d, want %d", len(got), len(want))
	}
	seen := map[string]bool{}
	dials := map[string]bool{}
	serials := map[string]bool{}
	for i, m := range got {
		if m.ID != want[i] {
			t.Errorf("inventory[%d]=%s, want %s", i, m.ID, want[i])
		}
		if seen[m.ID] {
			t.Errorf("duplicate id %s", m.ID)
		}
		seen[m.ID] = true
		if m.Vendor == "" || m.Name == "" || m.Serial == "" || m.MaxW <= 0 || m.Phases != 3 {
			t.Errorf("%s incomplete: %+v", m.ID, m)
		}
		if m.SpeaksOCPP() {
			if dials[m.DialID()] {
				t.Errorf("duplicate DialID %s", m.DialID())
			}
			dials[m.DialID()] = true
		}
		if serials[m.Serial] {
			t.Errorf("duplicate serial %s", m.Serial)
		}
		serials[m.Serial] = true
	}

	ocpp := OCPPModels()
	if len(ocpp) != len(got)-1 {
		t.Fatalf("OCPP models=%d, want inventory minus Tesla", len(ocpp))
	}
	for _, m := range ocpp {
		if !m.SpeaksOCPP() {
			t.Errorf("%s listed as OCPP but protocol=%s", m.ID, m.Protocol)
		}
	}

	tesla, ok := Lookup("tesla-wall-connector")
	if !ok || tesla.SpeaksOCPP() || tesla.Protocol != ProtocolHTTP {
		t.Fatalf("Tesla must be catalogued as local HTTP, got %+v ok=%v", tesla, ok)
	}

	zap, ok := Lookup("zaptec-go")
	if !ok || zap.DialID() != zap.Serial || zap.DialID() == zap.ID {
		t.Fatalf("Zaptec must dial as its serial, got DialID=%s id=%s serial=%s", zap.DialID(), zap.ID, zap.Serial)
	}

	defa, ok := Lookup("defa-power")
	if !ok || defa.Protocol != ProtocolOCPP201 {
		t.Fatalf("DEFA Power should speak 2.0.1 as advertised, got %+v", defa)
	}

	for _, m := range OCPPModels() {
		if n := len(idTagFor(m)); n > 20 {
			t.Errorf("%s idTag %q is %d runes, OCPP 1.6 max is 20", m.ID, idTagFor(m), n)
		}
	}

	halo, _ := Lookup("charge-amps-halo")
	if halo.MaxW != 11000 || !halo.Tethered {
		t.Errorf("Halo is the 11 kW tethered unit, got %+v", halo)
	}
	aura, _ := Lookup("charge-amps-aura")
	if aura.Connectors != 2 || !aura.Quirks.RejectConnectorZero {
		t.Errorf("Aura is the dual-socket unit that refuses connector 0, got %+v", aura)
	}
}

func TestPhysicsSettlesInstantlyWhenTauZero(t *testing.T) {
	p := newPhysics(Model{MaxW: 22000, Phases: 3}, 0)
	p.Plugged = true
	p.LimitA = 16
	p.Tick(time.Second)
	want := 16 * SiteVoltage * 3
	if p.PowerW() != want {
		t.Fatalf("power=%v, want %v", p.PowerW(), want)
	}
	p.LimitA = 0
	p.Tick(time.Second)
	if p.PowerW() != 0 {
		t.Fatalf("paused power=%v, want 0", p.PowerW())
	}
}

func TestChargeAmpsRemoteStopKeepsCharging(t *testing.T) {
	m, _ := Lookup("charge-amps-luna")
	s := New(m)
	s.mu.Lock()
	s.physics.Plugged = true
	s.txID = 9
	s.mu.Unlock()
	if _, err := s.OnRemoteStopTransaction(nil); err != nil {
		t.Fatal(err)
	}
	if !s.Plugged() || s.txID != 9 {
		t.Fatal("Charge Amps RemoteStop must ACK and leave the transaction open")
	}
}

func TestAuraRejectsConnectorZero(t *testing.T) {
	m, _ := Lookup("charge-amps-aura")
	d0 := decideProfile(m.Quirks, 0, "Relative", false, 16)
	if d0.applied || d0.status != "Rejected" {
		t.Fatalf("connector 0: %+v", d0)
	}
	d1 := decideProfile(m.Quirks, 1, "Relative", false, 16)
	if !d1.applied || d1.status != "Accepted" {
		t.Fatalf("connector 1: %+v", d1)
	}
}

func TestAbsoluteWithoutStartIsAcceptedAndIgnored(t *testing.T) {
	m, _ := Lookup("charge-amps-dawn")
	d := decideProfile(m.Quirks, 1, "Absolute", false, 16)
	if d.status != "Accepted" || d.applied {
		t.Fatalf("Absolute without start must Accept and not apply, got %+v", d)
	}
	rel := decideProfile(m.Quirks, 1, "Relative", false, 16)
	if !rel.applied {
		t.Fatal("Relative must apply")
	}
}
