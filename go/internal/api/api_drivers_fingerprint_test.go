package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/scanner"
)

// fakeModbus is a no-op Modbus capability — the fingerprint orchestration
// only needs the factory to hand back something non-nil; the tri-state
// verdict logic itself is exercised in the drivers package against real
// register layouts. Reads return a single zero register.
type fakeModbus struct{}

func (fakeModbus) Read(uint16, uint16, int32) ([]uint16, error) { return []uint16{0}, nil }
func (fakeModbus) WriteSingle(uint16, uint16) error             { return nil }
func (fakeModbus) WriteMulti(uint16, []uint16) error            { return nil }
func (fakeModbus) Close() error                                 { return nil }

// writeFingerprintDriver drops a catalog-shaped .lua file with the given
// protocols and driver_fingerprint body into dir.
func writeFingerprintDriver(t *testing.T, dir, filename, name, protocols, fpBody string) {
	t.Helper()
	src := "DRIVER = {\n" +
		"  id = \"" + filename + "\",\n" +
		"  name = \"" + name + "\",\n" +
		"  protocols = { " + protocols + " },\n" +
		"}\n" +
		"function driver_fingerprint()\n" + fpBody + "\nend\n"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func postFingerprint(t *testing.T, srv *Server, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/fingerprint", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes()
}

func TestHandleDriverFingerprintRejectsBadInput(t *testing.T) {
	srv := New(&Deps{
		DriverModbusFactory: func(string, *config.ModbusConfig) (drivers.ModbusCap, error) {
			return fakeModbus{}, nil
		},
	})
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"bad json", `nope`, 400, "invalid request"},
		{"missing host", `{"port":502}`, 400, "missing host"},
		{"host with userinfo", `{"host":"user@10.0.0.1","port":502}`, 400, "invalid host"},
		{"host with embedded port", `{"host":"10.0.0.1:80","port":502}`, 400, "invalid host"},
		{"missing port", `{"host":"10.0.0.1"}`, 400, "invalid port"},
		{"port too high", `{"host":"10.0.0.1","port":65536,"protocol":"http"}`, 400, "invalid port"},
		{"negative unit", `{"host":"10.0.0.1","port":502,"unit_id":-1}`, 400, "unit_id"},
		{"unit too high", `{"host":"10.0.0.1","port":502,"unit_id":256}`, 400, "unit_id"},
		{"uninferable protocol", `{"host":"10.0.0.1","port":1234}`, 400, "cannot infer protocol"},
		{"unsupported protocol", `{"host":"10.0.0.1","port":1234,"protocol":"mqtt"}`, 400, "modbus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postFingerprint(t, srv, tc.body)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", code, tc.wantCode, body)
			}
			var resp struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(body, &resp)
			if !strings.Contains(resp.Error, tc.wantErr) {
				t.Errorf("error = %q, want substring %q", resp.Error, tc.wantErr)
			}
		})
	}
}

func TestHandleDriverFingerprintRequiresModbusFactory(t *testing.T) {
	srv := New(&Deps{}) // no DriverModbusFactory
	code, body := postFingerprint(t, srv, `{"host":"10.0.0.1","port":502}`)
	if code != 503 {
		t.Fatalf("status = %d, want 503 (body=%s)", code, body)
	}
}

func TestHandleDriverFingerprintRanksMatches(t *testing.T) {
	dir := t.TempDir()
	// Two Modbus drivers — one claims the device, one declines — plus a
	// non-Modbus driver that must never be probed for a port-502 endpoint.
	writeFingerprintDriver(t, dir, "match.lua", "Matcher", `"modbus"`,
		`return true, { make = "FakeCo", model = "FC-1", confidence = 0.9 }`)
	writeFingerprintDriver(t, dir, "decline.lua", "Decliner", `"modbus"`,
		`return false`)
	writeFingerprintDriver(t, dir, "mqttonly.lua", "MqttOnly", `"mqtt"`,
		`return true`)

	srv := New(&Deps{
		DriverDir: dir,
		DriverModbusFactory: func(string, *config.ModbusConfig) (drivers.ModbusCap, error) {
			return fakeModbus{}, nil
		},
	})

	code, body := postFingerprint(t, srv, `{"host":"10.0.0.7","port":502}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", code, body)
	}
	var resp struct {
		Protocol string `json:"protocol"`
		UnitID   int    `json:"unit_id"`
		Matches  []struct {
			Driver string  `json:"driver"`
			Name   string  `json:"name"`
			Match  string  `json:"match"`
			Make   string  `json:"make"`
			Conf   float64 `json:"confidence"`
		} `json:"matches"`
		Tried []struct {
			Driver string `json:"driver"`
			Match  string `json:"match"`
		} `json:"tried"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if resp.Protocol != "modbus" {
		t.Errorf("protocol = %q, want modbus", resp.Protocol)
	}
	if resp.UnitID != 1 {
		t.Errorf("unit_id = %d, want default 1", resp.UnitID)
	}
	// The mqtt-only driver must have been skipped: exactly two Modbus
	// candidates were tried.
	if len(resp.Tried) != 2 {
		t.Fatalf("tried = %d candidates, want 2 (mqtt-only must be skipped): %s", len(resp.Tried), body)
	}
	if len(resp.Matches) != 1 {
		t.Fatalf("matches = %d, want 1: %s", len(resp.Matches), body)
	}
	m := resp.Matches[0]
	if m.Driver != "match.lua" || m.Name != "Matcher" || m.Make != "FakeCo" || m.Match != "match" {
		t.Errorf("match = %+v, want driver=match.lua name=Matcher make=FakeCo match=match", m)
	}
}

// A factory that can't connect (host unreachable) must not 500 — each
// candidate degrades to an `unknown` verdict carrying the error, and the
// match list comes back empty.
func TestHandleDriverFingerprintFactoryErrorIsUnknown(t *testing.T) {
	dir := t.TempDir()
	writeFingerprintDriver(t, dir, "match.lua", "Matcher", `"modbus"`, `return true`)

	srv := New(&Deps{
		DriverDir: dir,
		DriverModbusFactory: func(string, *config.ModbusConfig) (drivers.ModbusCap, error) {
			return nil, errors.New("dial tcp: connection refused")
		},
	})
	code, body := postFingerprint(t, srv, `{"host":"10.0.0.7","port":502}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", code, body)
	}
	var resp struct {
		Matches []json.RawMessage `json:"matches"`
		Tried   []struct {
			Match string `json:"match"`
			Err   string `json:"error"`
		} `json:"tried"`
	}
	_ = json.Unmarshal(body, &resp)
	if len(resp.Matches) != 0 {
		t.Errorf("matches = %d, want 0 when the connection fails", len(resp.Matches))
	}
	if len(resp.Tried) != 1 || resp.Tried[0].Match != "unknown" {
		t.Fatalf("tried = %+v, want one unknown verdict", resp.Tried)
	}
	if !strings.Contains(resp.Tried[0].Err, "connection refused") {
		t.Errorf("error = %q, want the dial failure surfaced", resp.Tried[0].Err)
	}
}

// HTTP fingerprinting needs no factory (HTTP is a built-in host
// capability). Port 80 infers the http protocol, and only http-speaking
// drivers are probed — the Modbus driver in the same catalog is skipped.
func TestHandleDriverFingerprintHTTPNeedsNoFactory(t *testing.T) {
	dir := t.TempDir()
	writeFingerprintDriver(t, dir, "httpdrv.lua", "HttpDrv", `"http"`,
		`return true, { make = "Webby" }`)
	writeFingerprintDriver(t, dir, "modbusdrv.lua", "ModbusDrv", `"modbus"`,
		`return true`)

	srv := New(&Deps{DriverDir: dir}) // deliberately no DriverModbusFactory

	code, body := postFingerprint(t, srv, `{"host":"10.0.0.9","port":80}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", code, body)
	}
	var resp struct {
		Protocol string `json:"protocol"`
		Matches  []struct {
			Driver string `json:"driver"`
			Make   string `json:"make"`
		} `json:"matches"`
		Tried []json.RawMessage `json:"tried"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if resp.Protocol != "http" {
		t.Errorf("protocol = %q, want http", resp.Protocol)
	}
	if len(resp.Tried) != 1 {
		t.Fatalf("tried = %d, want 1 (modbus driver must be skipped): %s", len(resp.Tried), body)
	}
	if len(resp.Matches) != 1 || resp.Matches[0].Driver != "httpdrv.lua" || resp.Matches[0].Make != "Webby" {
		t.Fatalf("matches = %+v, want one httpdrv.lua/Webby match", resp.Matches)
	}
}

// The /api/scan?fingerprint=1 enrichment attaches matches to Modbus hosts
// and leaves non-Modbus hosts (and the bare FoundDevice shape) untouched.
func TestEnrichScanWithFingerprints(t *testing.T) {
	dir := t.TempDir()
	writeFingerprintDriver(t, dir, "match.lua", "Matcher", `"modbus"`,
		`return true, { make = "FakeCo", confidence = 0.9 }`)

	srv := New(&Deps{
		DriverDir: dir,
		DriverModbusFactory: func(string, *config.ModbusConfig) (drivers.ModbusCap, error) {
			return fakeModbus{}, nil
		},
	})

	out := srv.enrichScanWithFingerprints([]scanner.FoundDevice{
		{IP: "10.0.0.7", Port: 502, Protocol: "modbus"},
		{IP: "10.0.0.8", Port: 1883, Protocol: "mqtt"},
	})
	if len(out) != 2 {
		t.Fatalf("got %d devices, want 2", len(out))
	}
	// Modbus host carries the match, with FoundDevice fields preserved.
	mb := out[0]
	if mb.IP != "10.0.0.7" || mb.Port != 502 {
		t.Errorf("modbus device fields lost: %+v", mb.FoundDevice)
	}
	if len(mb.Matches) != 1 || mb.Matches[0].Make != "FakeCo" {
		t.Fatalf("modbus matches = %+v, want one FakeCo match", mb.Matches)
	}
	// Non-Modbus host is passed through with no matches attached.
	if len(out[1].Matches) != 0 {
		t.Errorf("mqtt device got matches = %+v, want none", out[1].Matches)
	}
}
