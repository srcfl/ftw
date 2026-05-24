package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// capturingModbus is a tiny in-memory Modbus capability for driver tests. It
// records every WriteSingle call so the test can assert that the driver
// issued the right register writes, and returns zero registers for any
// Read (the SolarEdge poll path tolerates this because every pcall is
// guarded — it just emits zeros for the missing fields).
type capturingModbus struct {
	mu     sync.Mutex
	writes []writeOp
}

type writeOp struct {
	Addr  uint16
	Value uint16
}

func (m *capturingModbus) Read(addr uint16, count uint16, kind int32) ([]uint16, error) {
	out := make([]uint16, count)
	return out, nil
}

func (m *capturingModbus) WriteSingle(addr uint16, value uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, writeOp{Addr: addr, Value: value})
	return nil
}

func (m *capturingModbus) WriteMulti(addr uint16, values []uint16) error {
	return errors.New("WriteMulti not used by SolarEdge driver")
}

func (m *capturingModbus) Close() error { return nil }

func (m *capturingModbus) snapshot() []writeOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]writeOp, len(m.writes))
	copy(out, m.writes)
	return out
}

func (m *capturingModbus) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = nil
}

func loadSolarEdgeDriver(t *testing.T, path string, nominalW float64) (*LuaDriver, *capturingModbus) {
	t.Helper()
	tel := telemetry.NewStore()
	mb := &capturingModbus{}
	env := NewHostEnv("solaredge", tel).WithModbus(mb)
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cfg := map[string]any{}
	if nominalW > 0 {
		cfg["nominal_w"] = nominalW
	}
	if err := d.Init(ctx, cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Reset captured writes so init-side reads don't pollute the
	// curtail assertions below. (Init currently doesn't write anything,
	// but be defensive.)
	mb.reset()
	return d, mb
}

func runCmd(t *testing.T, d *LuaDriver, action string, powerW float64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd, _ := json.Marshal(map[string]any{"action": action, "power_w": powerW})
	if err := d.Command(ctx, cmd); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

// 50% curtail on a 10 kW inverter → writes pct=50 to 61441 then
// enable=1 to 61440, in that order.
func TestSolarEdgeCurtailHalfPower(t *testing.T) {
	d, mb := loadSolarEdgeDriver(t, "../../../drivers/solaredge.lua", 10000)
	defer d.Cleanup()
	runCmd(t, d, "curtail", 5000)

	w := mb.snapshot()
	if len(w) != 2 {
		t.Fatalf("expected 2 writes, got %d: %+v", len(w), w)
	}
	if w[0] != (writeOp{Addr: 61441, Value: 50}) {
		t.Errorf("first write: got %+v, want {61441, 50}", w[0])
	}
	if w[1] != (writeOp{Addr: 61440, Value: 1}) {
		t.Errorf("second write: got %+v, want {61440, 1}", w[1])
	}
}

func TestSolarEdgeCurtailZeroPower(t *testing.T) {
	d, mb := loadSolarEdgeDriver(t, "../../../drivers/solaredge.lua", 10000)
	defer d.Cleanup()
	runCmd(t, d, "curtail", 0)

	w := mb.snapshot()
	if len(w) != 2 {
		t.Fatalf("expected 2 writes, got %d", len(w))
	}
	if w[0].Value != 0 || w[1].Value != 1 {
		t.Errorf("force-off curtail: got pct=%d, ena=%d; want pct=0, ena=1",
			w[0].Value, w[1].Value)
	}
}

func TestSolarEdgeCurtailClampsOverNominal(t *testing.T) {
	d, mb := loadSolarEdgeDriver(t, "../../../drivers/solaredge.lua", 10000)
	defer d.Cleanup()
	runCmd(t, d, "curtail", 20000) // > nominal

	w := mb.snapshot()
	if len(w) != 2 {
		t.Fatalf("expected 2 writes, got %d", len(w))
	}
	if w[0].Value != 100 {
		t.Errorf("over-nominal pct: got %d, want 100 (clamped)", w[0].Value)
	}
}

func TestSolarEdgeCurtailDisable(t *testing.T) {
	d, mb := loadSolarEdgeDriver(t, "../../../drivers/solaredge.lua", 10000)
	defer d.Cleanup()
	runCmd(t, d, "curtail", 5000)
	mb.reset()

	runCmd(t, d, "curtail_disable", 0)
	w := mb.snapshot()
	// Release writes BOTH the limit value back to 100 % and disables
	// the enable bit — belt-and-suspenders for firmwares that honor
	// only one or the other.
	if len(w) != 2 {
		t.Fatalf("expected 2 writes on disable, got %d: %+v", len(w), w)
	}
	if w[0] != (writeOp{Addr: 61441, Value: 100}) {
		t.Errorf("first disable write: got %+v, want {61441, 100}", w[0])
	}
	if w[1] != (writeOp{Addr: 61440, Value: 0}) {
		t.Errorf("second disable write: got %+v, want {61440, 0}", w[1])
	}
}

// Without nominal_w in config, curtail must refuse to write anything —
// otherwise the driver would compute meaningless percentages.
func TestSolarEdgeCurtailWithoutNominalRejected(t *testing.T) {
	d, mb := loadSolarEdgeDriver(t, "../../../drivers/solaredge.lua", 0)
	defer d.Cleanup()
	runCmd(t, d, "curtail", 5000)
	if len(mb.snapshot()) != 0 {
		t.Errorf("curtail without nominal_w should write nothing, got %+v", mb.snapshot())
	}
}

// Both SolarEdge driver variants advertise the new pv-curtail capability.
func TestSolarEdgeCatalogAdvertisesCurtail(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	wantIDs := map[string]bool{"solaredge": false, "solaredge-pv": false}
	for _, e := range entries {
		if _, ok := wantIDs[e.ID]; !ok {
			continue
		}
		hasCurtail := false
		for _, c := range e.Capabilities {
			if c == "pv-curtail" {
				hasCurtail = true
				break
			}
		}
		if !hasCurtail {
			t.Errorf("driver %q missing pv-curtail capability: %v", e.ID, e.Capabilities)
		}
		wantIDs[e.ID] = true
	}
	for id, found := range wantIDs {
		if !found {
			t.Errorf("driver %q not in catalog", id)
		}
	}
}

// Same writes path applies to the PV-only variant.
func TestSolarEdgePVCurtail(t *testing.T) {
	d, mb := loadSolarEdgeDriver(t, "../../../drivers/solaredge_pv.lua", 8000)
	defer d.Cleanup()
	runCmd(t, d, "curtail", 2000)

	w := mb.snapshot()
	if len(w) != 2 {
		t.Fatalf("expected 2 writes, got %d", len(w))
	}
	if w[0].Addr != 61441 || w[0].Value != 25 {
		t.Errorf("first write: got %+v, want {61441, 25}", w[0])
	}
	if w[1] != (writeOp{Addr: 61440, Value: 1}) {
		t.Errorf("second write: got %+v, want {61440, 1}", w[1])
	}
}
