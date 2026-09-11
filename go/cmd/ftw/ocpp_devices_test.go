package main

import (
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/state"
)

func openTestStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// A charge point earns its device row on vendor+serial, like every other
// device. The name it dialled with is not identity — an installer typed it and
// the charger's own web page can change it — so it is recorded as the endpoint
// and only becomes the key when the charger reports no serial at all.
func TestOCPPDeviceIsKeyedOnHardwareNotTheDialledName(t *testing.T) {
	st := openTestStore(t)

	registerOCPPDevice(st, ocpp.ChargerIdentity{
		ID:     "garage",
		Vendor: "Charge Amps",
		Model:  "Halo",
		Serial: "CA-0001",
	})

	devs, err := st.AllDevices()
	if err != nil {
		t.Fatalf("AllDevices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
	d := devs[0]
	if d.DeviceID != "charge amps:CA-0001" {
		t.Errorf("device_id: got %q, want it keyed on vendor+serial", d.DeviceID)
	}
	if d.DriverName != "garage" {
		t.Errorf("driver_name: got %q, want the id the charger dialled with", d.DriverName)
	}
	if d.Endpoint != "ocpp://garage" {
		t.Errorf("endpoint: got %q, want ocpp://garage", d.Endpoint)
	}

	// Re-commissioned under a new name: same hardware, same row.
	registerOCPPDevice(st, ocpp.ChargerIdentity{
		ID:     "carport",
		Vendor: "Charge Amps",
		Serial: "CA-0001",
	})
	devs, _ = st.AllDevices()
	if len(devs) != 1 {
		t.Fatalf("renaming a charger created a second device row: %+v", devs)
	}
	if devs[0].DriverName != "carport" {
		t.Errorf("driver_name should follow the rename, got %q", devs[0].DriverName)
	}
}

// Plenty of chargers report no serial. They still deserve a row — the name
// they dialled with is stable enough to be an endpoint, which is exactly how
// the registry treats an address that only holds until someone changes it.
func TestOCPPDeviceFallsBackToTheDialledName(t *testing.T) {
	st := openTestStore(t)

	registerOCPPDevice(st, ocpp.ChargerIdentity{ID: "garage", Vendor: "Easee"})

	devs, err := st.AllDevices()
	if err != nil {
		t.Fatalf("AllDevices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
	if devs[0].DeviceID != "ep:ocpp://garage" {
		t.Errorf("device_id: got %q, want the endpoint fallback", devs[0].DeviceID)
	}
}

// An empty identity has nothing to key on and must not create a row.
func TestOCPPDeviceIgnoresAnEmptyIdentity(t *testing.T) {
	st := openTestStore(t)

	registerOCPPDevice(st, ocpp.ChargerIdentity{})
	registerOCPPDevice(nil, ocpp.ChargerIdentity{ID: "garage"})

	devs, err := st.AllDevices()
	if err != nil {
		t.Fatalf("AllDevices: %v", err)
	}
	if len(devs) != 0 {
		t.Fatalf("got %d devices, want none: %+v", len(devs), devs)
	}
}
