package telemetry

import "testing"

// A driver can be reachable and emitting fresh telemetry while the device it
// talks to is in a fault state where it can't actuate (e.g. a Ferroamp
// EnergyHub in Fault Mode with its relays open). The watchdog only catches
// "stopped emitting"; this covers "emitting but the device is faulted", so the
// control loop excludes it from dispatch + the MPC plan instead of commanding a
// dead battery (which would silently turn into grid import).
func TestDeviceFaultMakesDriverOfflineForControl(t *testing.T) {
	h := &DriverHealth{Name: "ferroamp"}
	h.RecordSuccess()
	if !h.IsOnline() {
		t.Fatal("should be online after RecordSuccess")
	}

	h.SetDeviceFault(true, "ehub fault 0x8030")
	if h.IsOnline() {
		t.Error("a device fault must make IsOnline() false (excluded from control)")
	}
	if h.DeviceFaultReason != "ehub fault 0x8030" {
		t.Errorf("DeviceFaultReason = %q, want the reason", h.DeviceFaultReason)
	}

	// The driver keeps emitting from cache every poll — RecordSuccess must NOT
	// clear the fault (that's the whole point; status flapping would let the
	// dispatcher command the dead battery on the next tick).
	h.RecordSuccess()
	if h.IsOnline() {
		t.Error("RecordSuccess must not clear a standing device fault")
	}

	h.SetDeviceFault(false, "")
	if !h.IsOnline() {
		t.Error("clearing the device fault should restore online")
	}
}

func TestStoreSetDriverDeviceFault(t *testing.T) {
	s := NewStore()
	s.DriverHealthMut("ferroamp").RecordSuccess()
	if !s.DriverHealth("ferroamp").IsOnline() {
		t.Fatal("precondition: should be online")
	}
	s.SetDriverDeviceFault("ferroamp", true, "ehub fault")
	if s.DriverHealth("ferroamp").IsOnline() {
		t.Error("faulted driver must read offline for control via the store")
	}
	s.SetDriverDeviceFault("ferroamp", false, "")
	if !s.DriverHealth("ferroamp").IsOnline() {
		t.Error("clearing via the store should restore online")
	}
}
