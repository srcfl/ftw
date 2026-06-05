package drivers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// fpModbus is a fake SunSpec-ish Modbus device for fingerprint tests. It
// answers reads only on answerFC (others "time out", mirroring how a
// SolarEdge K-series ignores FC 0x04), and fails the test if a driver's
// fingerprint ever issues a write — fingerprints must be strictly read-only.
type fpModbus struct {
	t        *testing.T
	answerFC int32 // 2 = holding, 3 = input
	hasSunS  bool
	mfr      string
	model    string // SunSpec C_Model @ 40020 (e.g. "SE8K-RW0TEBNN4")
	serial   string
	meterV   int // raw i16 line voltage at 40196; 0 = no meter
	meterVSF int // i16 scale factor at 40203
	whRtg    int // SunSpec 802 WHRtg @ 40124 (0 = leave as not-implemented)
	whRtgSF  int // SunSpec 802 WHRtg_SF @ 40174
}

func (m *fpModbus) Read(addr, count uint16, kind int32) ([]uint16, error) {
	if kind != m.answerFC {
		return nil, fmt.Errorf("fc %d not supported (timeout)", kind)
	}
	out := make([]uint16, count)
	switch addr {
	case 40000:
		if m.hasSunS {
			out[0] = 0x5375 // "Su"
			if count > 1 {
				out[1] = 0x6e53 // "nS"
			}
		}
	case 40004:
		copy(out, asciiRegs(m.mfr, int(count)))
	case 40020:
		copy(out, asciiRegs(m.model, int(count)))
	case 40052:
		copy(out, asciiRegs(m.serial, int(count)))
	case 40196:
		out[0] = uint16(int16(m.meterV))
	case 40203:
		out[0] = uint16(int16(m.meterVSF))
	case 40124: // SunSpec 802 WHRtg
		if m.whRtg == 0 {
			out[0] = 0xFFFF // not implemented
		} else {
			out[0] = uint16(m.whRtg)
		}
	case 40174: // SunSpec 802 WHRtg_SF
		out[0] = uint16(int16(m.whRtgSF))
	}
	return out, nil
}

func (m *fpModbus) WriteSingle(uint16, uint16) error {
	m.t.Error("driver_fingerprint issued a Modbus WriteSingle — fingerprints must be read-only")
	return nil
}
func (m *fpModbus) WriteMulti(uint16, []uint16) error {
	m.t.Error("driver_fingerprint issued a Modbus WriteMulti — fingerprints must be read-only")
	return nil
}
func (m *fpModbus) Close() error { return nil }

// asciiRegs packs a string into n SunSpec registers, high byte first.
func asciiRegs(s string, n int) []uint16 {
	out := make([]uint16, n)
	b := []byte(s)
	for i := 0; i < n; i++ {
		var hi, lo byte
		if 2*i < len(b) {
			hi = b[2*i]
		}
		if 2*i+1 < len(b) {
			lo = b[2*i+1]
		}
		out[i] = uint16(hi)<<8 | uint16(lo)
	}
	return out
}

func runFingerprint(t *testing.T, path string, env *HostEnv) FingerprintResult {
	t.Helper()
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	defer d.Close() // Close, not Cleanup — mirror the read-only probe path
	res, err := d.Fingerprint(context.Background())
	if err != nil {
		t.Fatalf("fingerprint %s: %v", path, err)
	}
	return res
}

func TestModbusDriverFingerprints(t *testing.T) {
	const (
		pixii   = "../../../drivers/pixii.lua"
		seModP  = "../../../drivers/solaredge.lua"
		sePv    = "../../../drivers/solaredge_pv.lua"
		seLegcy = "../../../drivers/solaredge_legacy.lua"
		holding = int32(2)
		input   = int32(3)
	)

	cases := []struct {
		name     string
		driver   string
		dev      *fpModbus
		match     bool
		make_     string
		wantCaps  string  // comma-joined, "" when not asserting
		wantCapWh float64 // expected BatteryCapacityWh, 0 = don't assert
	}{
		{
			name:      "pixii matches + reads nameplate WHRtg",
			driver:    pixii,
			dev:       &fpModbus{answerFC: holding, hasSunS: true, mfr: "Pixii AS", serial: "PX-1", whRtg: 20480, whRtgSF: 0},
			match:     true,
			make_:     "Pixii",
			wantCaps:  "battery,meter",
			wantCapWh: 20480,
		},
		{
			name:   "pixii rejects a SolarEdge (holding, mfr SolarEdge)",
			driver: pixii,
			dev:    &fpModbus{answerFC: holding, hasSunS: true, mfr: "SolarEdge"},
			match:  false,
		},
		{
			name:   "solaredge.lua (input variant) has no fingerprint — never matches",
			driver: seModP,
			dev:    &fpModbus{answerFC: input, hasSunS: true, mfr: "SolarEdge"},
			match:  false,
		},
		{
			name:     "solaredge_pv matches an HD-Wave (SE8K) on holding",
			driver:   sePv,
			dev:      &fpModbus{answerFC: holding, hasSunS: true, mfr: "SolarEdge ", model: "SE8K-RW0TEBNN4", serial: "7E16E274"},
			match:    true,
			make_:    "SolarEdge",
			wantCaps: "pv,pv-curtail",
		},
		{
			name:   "solaredge_pv defers on a K-series model (→ legacy)",
			driver: sePv,
			dev:    &fpModbus{answerFC: holding, hasSunS: true, mfr: "SolarEdge", model: "SE17K-XYZ"},
			match:  false,
		},
		{
			name:     "solaredge legacy matches a K-series display unit",
			driver:   seLegcy,
			dev:      &fpModbus{answerFC: holding, hasSunS: true, mfr: "SolarEdge", model: "SE17K-XYZ", serial: "SE-K-1"},
			match:    true,
			make_:    "SolarEdge",
			wantCaps: "pv,pv-curtail",
		},
		{
			name:   "solaredge legacy defers on an HD-Wave (SE8K → pv)",
			driver: seLegcy,
			dev:    &fpModbus{answerFC: holding, hasSunS: true, mfr: "SolarEdge", model: "SE8K-RW0TEBNN4"},
			match:  false,
		},
		{
			name:   "solaredge legacy rejects a Pixii (holding, mfr Pixii)",
			driver: seLegcy,
			dev:    &fpModbus{answerFC: holding, hasSunS: true, mfr: "Pixii"},
			match:  false,
		},
		{
			name:   "pixii rejects a non-SunSpec device",
			driver: pixii,
			dev:    &fpModbus{answerFC: holding, hasSunS: false, mfr: "Pixii"},
			match:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.dev.t = t
			env := NewHostEnv("fp", telemetry.NewStore()).WithModbus(tc.dev)
			res := runFingerprint(t, tc.driver, env)
			if res.Matched != tc.match {
				t.Fatalf("matched = %v, want %v (%+v)", res.Matched, tc.match, res)
			}
			if !tc.match {
				return
			}
			if res.Make != tc.make_ {
				t.Errorf("make = %q, want %q", res.Make, tc.make_)
			}
			if tc.wantCaps != "" && strings.Join(res.Capabilities, ",") != tc.wantCaps {
				t.Errorf("caps = %q, want %q", strings.Join(res.Capabilities, ","), tc.wantCaps)
			}
			if tc.wantCapWh != 0 && res.BatteryCapacityWh != tc.wantCapWh {
				t.Errorf("battery capacity = %v Wh, want %v", res.BatteryCapacityWh, tc.wantCapWh)
			}
		})
	}
}

// fpMQTT is a fake broker for the Ferroamp fingerprint: it records subscribes,
// fails the test on any publish (the fingerprint must be passive), and hands
// the queued messages over once via PopMessages.
type fpMQTT struct {
	t      *testing.T
	msgs   []MQTTMessage
	popped bool
}

func (m *fpMQTT) Subscribe(string) error { return nil }
func (m *fpMQTT) Publish(topic string, _ []byte) error {
	m.t.Errorf("driver_fingerprint published to %q — Ferroamp fingerprint must be passive", topic)
	return nil
}
func (m *fpMQTT) PopMessages() []MQTTMessage {
	if m.popped {
		return nil
	}
	m.popped = true
	return m.msgs
}
func (m *fpMQTT) Close() error { return nil }

func TestFerroampFingerprintMatchesLiveBroker(t *testing.T) {
	mq := &fpMQTT{
		t:    t,
		msgs: []MQTTMessage{{Topic: "extapi/data/ehub", Payload: `{"gridfreq":{"val":"50.0"}}`}},
	}
	env := NewHostEnv("fp", telemetry.NewStore()).WithMQTT(mq)
	res := runFingerprint(t, "../../../drivers/ferroamp.lua", env)
	if !res.Matched || res.Make != "Ferroamp" {
		t.Fatalf("ferroamp fingerprint = %+v, want matched Ferroamp", res)
	}
	if strings.Join(res.Capabilities, ",") != "meter,pv,battery" {
		t.Errorf("caps = %v", res.Capabilities)
	}
}
