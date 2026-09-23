package state

import "testing"

// device_id is the key every piece of persistent per-device state hangs
// off (battery models, the RLS twin, calibration history), so the
// priority order below is a compatibility surface: changing it silently
// orphans that state on the next boot.
func TestResolveDeviceIDPriority(t *testing.T) {
	cases := []struct {
		name                        string
		make, serial, mac, endpoint string
		want                        string
	}{
		{
			name: "hardware serial outranks mac and endpoint",
			make: "Sungrow", serial: "A2340", mac: "aa:bb:cc:dd:ee:ff",
			endpoint: "modbus://192.168.1.5:502",
			want:     "sungrow:A2340",
		},
		{
			name: "mac outranks endpoint",
			mac:  "AA:BB:CC:DD:EE:FF", endpoint: "modbus://192.168.1.5:502",
			want: "mac:aabbccddeeff",
		},
		{
			name:     "endpoint is the last resort",
			endpoint: "modbus://192.168.1.5:502",
			want:     "ep:modbus://192.168.1.5:502",
		},
		{
			name: "nothing known yields no id",
			want: "",
		},
		{
			// Half a hardware identity is not an identity — a make shared
			// by every unit of a model would collide across devices.
			name: "make without serial falls through to mac",
			make: "Sungrow", mac: "aa:bb:cc:dd:ee:ff",
			want: "mac:aabbccddeeff",
		},
		{
			name:   "serial without make falls through to mac",
			serial: "A2340", mac: "aa:bb:cc:dd:ee:ff",
			want: "mac:aabbccddeeff",
		},
		{
			// Make is normalized but serial is not: vendor serials are
			// case-significant and two units may differ only in case.
			name: "make is lowercased and serial case is preserved",
			make: "SunGrow", serial: "A2340b",
			want: "sungrow:A2340b",
		},
		{
			name: "surrounding whitespace is trimmed",
			make: "  sungrow  ", serial: "  A2340  ",
			want: "sungrow:A2340",
		},
		{
			name: "mac separators are stripped and case normalized",
			mac:  "  AA:BB:CC:DD:EE:FF  ",
			want: "mac:aabbccddeeff",
		},
		{
			name: "whitespace-only fields count as absent",
			make: "   ", serial: "  ", mac: " ", endpoint: "ep-fallback",
			want: "ep:ep-fallback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveDeviceID(tc.make, tc.serial, tc.mac, tc.endpoint)
			if got != tc.want {
				t.Errorf("ResolveDeviceID(%q, %q, %q, %q) = %q, want %q",
					tc.make, tc.serial, tc.mac, tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestRegisterDeviceRejectsAnUnidentifiableDevice(t *testing.T) {
	s := freshStore(t)
	if id, err := s.RegisterDevice(Device{DriverName: "inv"}); err == nil {
		t.Fatalf("RegisterDevice with no identity bits = (%q, nil), want an error", id)
	}
	devs, err := s.AllDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 0 {
		t.Fatalf("AllDevices = %d rows, want 0 — a nameless device must not be stored", len(devs))
	}
}

func TestRegisterDeviceIsIdempotentAndPreservesKnownIdentity(t *testing.T) {
	s := freshStore(t)
	const mac = "aa:bb:cc:dd:ee:ff"

	id, err := s.RegisterDevice(Device{
		DriverName: "inv", MAC: mac, Endpoint: "http://192.168.1.5", FirstSeenMs: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "mac:aabbccddeeff" {
		t.Fatalf("device_id = %q, want mac:aabbccddeeff", id)
	}

	// Re-register the same row after a cold ARP cache: the MAC is no
	// longer resolvable and the serial has since arrived. The stored MAC
	// must survive the empty field, otherwise the row loses the very
	// identity it is keyed on.
	if _, err := s.RegisterDevice(Device{
		DeviceID: id, DriverName: "inv", Make: "sungrow", Serial: "A2340", FirstSeenMs: 9999,
	}); err != nil {
		t.Fatal(err)
	}

	devs, err := s.AllDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 {
		t.Fatalf("AllDevices = %d rows, want 1 — re-registering must upsert, not insert", len(devs))
	}
	got := devs[0]
	if got.MAC != mac {
		t.Errorf("mac = %q, want %q — an empty field must not erase a known one", got.MAC, mac)
	}
	if got.Endpoint != "http://192.168.1.5" {
		t.Errorf("endpoint = %q, want it preserved", got.Endpoint)
	}
	if got.Make != "sungrow" || got.Serial != "A2340" {
		t.Errorf("make/serial = %q/%q, want the newly-learned values", got.Make, got.Serial)
	}
	if got.FirstSeenMs != 1000 {
		t.Errorf("first_seen_ms = %d, want 1000 — first sighting is immutable", got.FirstSeenMs)
	}
}

// Documents a real consequence of the priority order rather than
// asserting it is desirable: registerAllDevices resolves the id afresh on
// every pass, so a device first seen by MAC is re-keyed the moment its
// driver reports a serial, leaving the mac: row behind. Any state still
// keyed on the old id is orphaned at that point.
func TestRegisterDeviceRekeysWhenSerialArrivesAfterMAC(t *testing.T) {
	s := freshStore(t)

	first, err := s.RegisterDevice(Device{DriverName: "inv", MAC: "aa:bb:cc:dd:ee:ff"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RegisterDevice(Device{
		DriverName: "inv", Make: "sungrow", Serial: "A2340", MAC: "aa:bb:cc:dd:ee:ff",
	})
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatalf("device_id stayed %q after the serial arrived; this test is stale", first)
	}
	devs, err := s.AllDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("AllDevices = %d rows, want 2 (the orphaned %q plus %q)", len(devs), first, second)
	}
}

func TestLookupDeviceByDriverName(t *testing.T) {
	s := freshStore(t)
	if got := s.LookupDeviceByDriverName("inv"); got != nil {
		t.Fatalf("cold lookup = %+v, want nil", got)
	}

	id, err := s.RegisterDevice(Device{DriverName: "inv", Make: "sungrow", Serial: "A2340"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.LookupDeviceByDriverName("inv")
	if got == nil {
		t.Fatal("lookup after register = nil, want the registered device")
	}
	if got.DeviceID != id {
		t.Errorf("device_id = %q, want %q", got.DeviceID, id)
	}
	if s.LookupDeviceByDriverName("other") != nil {
		t.Error("lookup of an unregistered driver name returned a device")
	}
}

func TestRelateDeviceIDs(t *testing.T) {
	cases := []struct {
		name             string
		expected, actual string
		want             DeviceIDRelation
	}{
		{name: "identical hardware ids match", expected: "pixii:234301002749", actual: "pixii:234301002749", want: DeviceIDMatch},
		{name: "no expected id accepts anything", expected: "", actual: "pixii:234301002749", want: DeviceIDMatch},
		{name: "empty actual waits for identity", expected: "mac:b827b9358d1a", actual: "", want: DeviceIDPending},
		{
			// Live Pixii update: ARP MAC until 2.1.4 actually called set_sn.
			name:     "serial after mac needs shared evidence",
			expected: "mac:b827b9358d1a", actual: "pixii:234301002749",
			want: DeviceIDPending,
		},
		{name: "mac after endpoint needs shared evidence", expected: "ep:modbus://192.168.1.10:502", actual: "mac:b827b9358d1a", want: DeviceIDPending},
		{name: "serial after endpoint needs shared evidence", expected: "ep:modbus://192.168.1.10:502", actual: "pixii:234301002749", want: DeviceIDPending},
		{name: "weaker mac after serial waits", expected: "pixii:234301002749", actual: "mac:b827b9358d1a", want: DeviceIDPending},
		{name: "weaker endpoint after mac waits", expected: "mac:b827b9358d1a", actual: "ep:modbus://192.168.1.10:502", want: DeviceIDPending},
		{name: "different serial is a different device", expected: "pixii:234301002749", actual: "pixii:OTHER", want: DeviceIDConflict},
		{name: "different mac is a different device", expected: "mac:b827b9358d1a", actual: "mac:aabbccddeeff", want: DeviceIDConflict},
		{name: "different endpoint is a different device", expected: "ep:modbus://192.168.1.10:502", actual: "ep:modbus://192.168.1.11:502", want: DeviceIDConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RelateDeviceIDs(tc.expected, tc.actual)
			if got != tc.want {
				t.Errorf("RelateDeviceIDs(%q, %q) = %v, want %v", tc.expected, tc.actual, got, tc.want)
			}
		})
	}
}
