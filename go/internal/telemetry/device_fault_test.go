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

// The other end of the same wire: a driver that answers polls and rejects
// writes believes the device is fine and says so on every poll. Its verdict
// must not be able to clear core's, or the two would flip the driver in and
// out of the fleet for as long as the refusals lasted.
func TestDriverPollCannotClearACommandFault(t *testing.T) {
	s := NewStore()
	s.DriverHealthMut("sungrow").RecordSuccess()

	s.SetDriverCommandFault("sungrow", true, "modbus write refused")
	if s.DriverHealth("sungrow").IsOnline() {
		t.Fatal("a driver that refuses commands must leave the control set")
	}

	// Every poll of a driver that thinks it is healthy.
	for i := 0; i < 5; i++ {
		s.SetDriverDeviceFault("sungrow", false, "")
		s.RecordDriverSuccess("sungrow")
		if s.DriverHealth("sungrow").IsOnline() {
			t.Fatalf("poll %d cleared the command fault", i)
		}
	}

	s.SetDriverCommandFault("sungrow", false, "")
	if !s.DriverHealth("sungrow").IsOnline() {
		t.Error("clearing the command fault should restore online")
	}
}

// The driver's own fault survives core clearing its command fault: two
// sources, one derived flag, neither able to overwrite the other.
func TestDeviceAndCommandFaultsAreIndependent(t *testing.T) {
	h := &DriverHealth{Name: "sungrow"}
	h.RecordSuccess()

	h.SetDeviceFault(true, "inverter fault 0x12")
	h.SetCommandFault(true, "modbus write refused")
	if h.DeviceFaultReason != "inverter fault 0x12" {
		t.Errorf("reason = %q, want the driver's own — it saw the device", h.DeviceFaultReason)
	}

	h.SetCommandFault(false, "")
	if !h.DeviceFault {
		t.Error("clearing the command fault dropped the driver's own fault")
	}

	h.SetDeviceFault(false, "")
	if h.DeviceFault || !h.IsOnline() {
		t.Error("with both sources clear the driver should be back in control")
	}
}

// A driver that is gone must not be resurrected by a late verdict about it.
func TestSetDriverCommandFaultDoesNotCreateHealth(t *testing.T) {
	s := NewStore()
	s.SetDriverCommandFault("ghost", true, "modbus write refused")
	if h := s.DriverHealth("ghost"); h != nil {
		t.Fatalf("command fault created a health record for an unknown driver: %+v", h)
	}
}
