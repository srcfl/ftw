package drivers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// writeTempDriver drops a .lua file into a temp dir and returns its path.
func writeTempDriver(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.lua")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp driver: %v", err)
	}
	return path
}

func TestFingerprintTriState(t *testing.T) {
	cases := []struct {
		name string
		body string
		want MatchState
	}{
		{"true_is_match", `function driver_fingerprint() return true end`, MatchYes},
		{"false_is_no_match", `function driver_fingerprint() return false end`, MatchNo},
		{"nil_is_unknown", `function driver_fingerprint() return nil end`, MatchUnknown},
		{"absent_is_unknown", `-- no driver_fingerprint here`, MatchUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewHostEnv("probe", telemetry.NewStore())
			fp, err := RunFingerprint(writeTempDriver(t, tc.body), env, FingerprintTarget{})
			if err != nil {
				t.Fatalf("RunFingerprint: %v", err)
			}
			if fp.Match != tc.want {
				t.Fatalf("Match = %q, want %q", fp.Match, tc.want)
			}
		})
	}
}

func TestFingerprintIdentityHint(t *testing.T) {
	body := `function driver_fingerprint()
	    return true, { make = "Acme", model = "X1", serial = "SN-007", confidence = 0.8 }
	end`
	env := NewHostEnv("probe", telemetry.NewStore())
	fp, err := RunFingerprint(writeTempDriver(t, body), env, FingerprintTarget{})
	if err != nil {
		t.Fatalf("RunFingerprint: %v", err)
	}
	if fp.Match != MatchYes {
		t.Fatalf("Match = %q, want match", fp.Match)
	}
	if fp.Make != "Acme" || fp.Model != "X1" || fp.Serial != "SN-007" {
		t.Fatalf("identity = %+v, want Acme/X1/SN-007", fp)
	}
	if fp.Confidence != 0.8 {
		t.Fatalf("Confidence = %v, want 0.8", fp.Confidence)
	}
}

func TestFingerprintErrorIsUnknown(t *testing.T) {
	body := `function driver_fingerprint() error("boom") end`
	env := NewHostEnv("probe", telemetry.NewStore())
	fp, err := RunFingerprint(writeTempDriver(t, body), env, FingerprintTarget{})
	if err == nil {
		t.Fatalf("expected an error from a throwing fingerprint")
	}
	if fp.Match != MatchUnknown {
		t.Fatalf("Match = %q, want unknown on lua error", fp.Match)
	}
	if fp.Err == "" {
		t.Fatalf("Err empty, want the lua error surfaced")
	}
}

// erroringModbus fails every read — models an endpoint that accepts the
// TCP connection but rejects/ times-out the Modbus request (wrong unit id,
// not actually Modbus, etc.). Drivers must treat this as inconclusive.
type erroringModbus struct{}

func (erroringModbus) Read(uint16, uint16, int32) ([]uint16, error) {
	return nil, errors.New("modbus: i/o timeout")
}
func (erroringModbus) WriteSingle(uint16, uint16) error  { return nil }
func (erroringModbus) WriteMulti(uint16, []uint16) error { return nil }
func (erroringModbus) Close() error                      { return nil }

// packASCII writes s (null-padded) into a recordingModbus starting at addr,
// two bytes per register, big-endian (hi byte first) — the SunSpec string
// convention.
func packASCII(m *recordingModbus, addr uint16, s string, regs int) {
	b := make([]byte, regs*2)
	copy(b, s)
	for i := 0; i < regs; i++ {
		m.regs[addr+uint16(i)] = uint16(b[i*2])<<8 | uint16(b[i*2+1])
	}
}

func TestSungrowFingerprint(t *testing.T) {
	const devTypeReg = 4999
	t.Run("sh_family_matches", func(t *testing.T) {
		m := newRecordingModbus()
		m.regs[devTypeReg] = 0x0E0E // SH10RT
		env := NewHostEnv("sungrow", telemetry.NewStore()).WithModbus(m)
		fp, err := RunFingerprint("../../../drivers/sungrow.lua", env, FingerprintTarget{Protocol: "modbus"})
		if err != nil {
			t.Fatalf("RunFingerprint: %v", err)
		}
		if fp.Match != MatchYes {
			t.Fatalf("Match = %q, want match for 0x0E0E", fp.Match)
		}
		if fp.Make != "Sungrow" {
			t.Fatalf("Make = %q, want Sungrow", fp.Make)
		}
	})
	t.Run("foreign_code_is_no_match", func(t *testing.T) {
		m := newRecordingModbus()
		m.regs[devTypeReg] = 0x2A2A // not a Sungrow hybrid code
		env := NewHostEnv("sungrow", telemetry.NewStore()).WithModbus(m)
		fp, _ := RunFingerprint("../../../drivers/sungrow.lua", env, FingerprintTarget{Protocol: "modbus"})
		if fp.Match != MatchNo {
			t.Fatalf("Match = %q, want no_match for foreign device code", fp.Match)
		}
	})
	t.Run("read_error_is_unknown", func(t *testing.T) {
		env := NewHostEnv("sungrow", telemetry.NewStore()).WithModbus(erroringModbus{})
		fp, _ := RunFingerprint("../../../drivers/sungrow.lua", env, FingerprintTarget{Protocol: "modbus"})
		if fp.Match != MatchUnknown {
			t.Fatalf("Match = %q, want unknown when the read fails", fp.Match)
		}
	})
}

func TestSolarEdgeFingerprint(t *testing.T) {
	// SunSpec common block layout used by the SolarEdge driver:
	//   40000-40001  SID "SunS"
	//   40004-40019  Manufacturer (ASCII)
	//   40052-40067  Serial (ASCII)
	setSunS := func(m *recordingModbus) {
		m.regs[40000] = 0x5375 // 'S','u'
		m.regs[40001] = 0x6E53 // 'n','S'
	}
	t.Run("solaredge_matches_with_serial", func(t *testing.T) {
		m := newRecordingModbus()
		setSunS(m)
		packASCII(m, 40004, "SolarEdge", 16)
		packASCII(m, 40052, "7E0A1B2C", 16)
		env := NewHostEnv("solaredge", telemetry.NewStore()).WithModbus(m)
		fp, err := RunFingerprint("../../../drivers/solaredge.lua", env, FingerprintTarget{Protocol: "modbus"})
		if err != nil {
			t.Fatalf("RunFingerprint: %v", err)
		}
		if fp.Match != MatchYes {
			t.Fatalf("Match = %q, want match", fp.Match)
		}
		if fp.Make != "SolarEdge" {
			t.Fatalf("Make = %q, want SolarEdge", fp.Make)
		}
		if fp.Serial != "7E0A1B2C" {
			t.Fatalf("Serial = %q, want 7E0A1B2C", fp.Serial)
		}
	})
	t.Run("other_sunspec_vendor_is_no_match", func(t *testing.T) {
		m := newRecordingModbus()
		setSunS(m)
		packASCII(m, 40004, "Fronius", 16)
		env := NewHostEnv("solaredge", telemetry.NewStore()).WithModbus(m)
		fp, _ := RunFingerprint("../../../drivers/solaredge.lua", env, FingerprintTarget{Protocol: "modbus"})
		if fp.Match != MatchNo {
			t.Fatalf("Match = %q, want no_match for a non-SolarEdge SunSpec device", fp.Match)
		}
	})
	t.Run("non_sunspec_is_no_match", func(t *testing.T) {
		m := newRecordingModbus() // all registers zero → no SunS marker
		env := NewHostEnv("solaredge", telemetry.NewStore()).WithModbus(m)
		fp, _ := RunFingerprint("../../../drivers/solaredge.lua", env, FingerprintTarget{Protocol: "modbus"})
		if fp.Match != MatchNo {
			t.Fatalf("Match = %q, want no_match when the SunSpec marker is absent", fp.Match)
		}
	})
	t.Run("read_error_is_unknown", func(t *testing.T) {
		env := NewHostEnv("solaredge", telemetry.NewStore()).WithModbus(erroringModbus{})
		fp, _ := RunFingerprint("../../../drivers/solaredge.lua", env, FingerprintTarget{Protocol: "modbus"})
		if fp.Match != MatchUnknown {
			t.Fatalf("Match = %q, want unknown when the read fails", fp.Match)
		}
	})
}

func TestPixiiFingerprint(t *testing.T) {
	// Pixii exposes the SunSpec common block on HOLDING registers; the
	// recording fake ignores the register kind, so the same packed layout
	// works. Marker at 40000, manufacturer at 40004, serial at 40052.
	setSunS := func(m *recordingModbus) {
		m.regs[40000] = 0x5375
		m.regs[40001] = 0x6E53
	}
	t.Run("pixii_matches_with_serial", func(t *testing.T) {
		m := newRecordingModbus()
		setSunS(m)
		packASCII(m, 40004, "Pixii", 16)
		packASCII(m, 40052, "PSX-12345", 16)
		env := NewHostEnv("pixii", telemetry.NewStore()).WithModbus(m)
		fp, err := RunFingerprint("../../../drivers/pixii.lua", env, FingerprintTarget{Protocol: "modbus"})
		if err != nil {
			t.Fatalf("RunFingerprint: %v", err)
		}
		if fp.Match != MatchYes {
			t.Fatalf("Match = %q, want match", fp.Match)
		}
		if fp.Make != "Pixii" || fp.Serial != "PSX-12345" {
			t.Fatalf("identity = %+v, want Pixii / PSX-12345", fp)
		}
	})
	t.Run("other_sunspec_vendor_is_no_match", func(t *testing.T) {
		m := newRecordingModbus()
		setSunS(m)
		packASCII(m, 40004, "SolarEdge", 16)
		env := NewHostEnv("pixii", telemetry.NewStore()).WithModbus(m)
		fp, _ := RunFingerprint("../../../drivers/pixii.lua", env, FingerprintTarget{Protocol: "modbus"})
		if fp.Match != MatchNo {
			t.Fatalf("Match = %q, want no_match for a non-Pixii SunSpec device", fp.Match)
		}
	})
	t.Run("non_sunspec_is_no_match", func(t *testing.T) {
		m := newRecordingModbus()
		env := NewHostEnv("pixii", telemetry.NewStore()).WithModbus(m)
		fp, _ := RunFingerprint("../../../drivers/pixii.lua", env, FingerprintTarget{Protocol: "modbus"})
		if fp.Match != MatchNo {
			t.Fatalf("Match = %q, want no_match when the SunSpec marker is absent", fp.Match)
		}
	})
	t.Run("read_error_is_unknown", func(t *testing.T) {
		env := NewHostEnv("pixii", telemetry.NewStore()).WithModbus(erroringModbus{})
		fp, _ := RunFingerprint("../../../drivers/pixii.lua", env, FingerprintTarget{Protocol: "modbus"})
		if fp.Match != MatchUnknown {
			t.Fatalf("Match = %q, want unknown when the read fails", fp.Match)
		}
	})
}

// httpTarget spins up a test server with the given /api/devices handler and
// returns an HTTP-capable env + target pointed at it (allowlist scoped to
// the server host, mirroring how the API handler wires the probe).
func httpTarget(t *testing.T, handler http.HandlerFunc) (*HostEnv, FingerprintTarget, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/devices", handler)
	srv := httptest.NewServer(mux)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	env := NewHostEnv("zap", telemetry.NewStore()).WithHTTP().WithHTTPAllowedHosts([]string{u.Hostname()})
	return env, FingerprintTarget{Host: u.Hostname(), Port: port, Protocol: "http"}, srv
}

func TestZapFingerprint(t *testing.T) {
	const devicesJSON = `{"devices":[` +
		`{"sn":"p1m-abc","type":"p1_uart"},` +
		`{"sn":"inv-1","device_type":"inverter","ders":[{"type":"pv","enabled":true}]}` +
		`]}`

	t.Run("zap_devices_api_matches", func(t *testing.T) {
		env, target, srv := httpTarget(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(devicesJSON))
		})
		defer srv.Close()
		fp, err := RunFingerprint("../../../drivers/zap.lua", env, target)
		if err != nil {
			t.Fatalf("RunFingerprint: %v", err)
		}
		if fp.Match != MatchYes {
			t.Fatalf("Match = %q, want match", fp.Match)
		}
		if fp.Make != "Sourceful" || fp.Model != "Zap" || fp.Serial != "p1m-abc" {
			t.Fatalf("identity = %+v, want Sourceful / Zap / p1m-abc", fp)
		}
	})
	t.Run("foreign_http_service_is_no_match", func(t *testing.T) {
		env, target, srv := httpTarget(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html>some router admin page</html>"))
		})
		defer srv.Close()
		fp, _ := RunFingerprint("../../../drivers/zap.lua", env, target)
		if fp.Match != MatchNo {
			t.Fatalf("Match = %q, want no_match for a non-Zap HTTP service", fp.Match)
		}
	})
	t.Run("unreachable_is_unknown", func(t *testing.T) {
		env, target, srv := httpTarget(t, func(w http.ResponseWriter, r *http.Request) {})
		srv.Close() // close immediately so the probe's GET is refused
		fp, _ := RunFingerprint("../../../drivers/zap.lua", env, target)
		if fp.Match != MatchUnknown {
			t.Fatalf("Match = %q, want unknown when the host is unreachable", fp.Match)
		}
	})
}
