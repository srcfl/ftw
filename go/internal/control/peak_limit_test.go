package control

import (
	"strings"
	"testing"
)

// SetPeakLimit is the one place the peak-shaving threshold is validated
// against the site's fuse. A 16 A / 230 V / 3-phase site is 11040 W; the
// default 0.5 A safety margin costs 345 W, so the highest limit that can
// still be the first thing to bind is 10695 W.
func fusedState() *State {
	st := NewState(0, 50, "ferroamp")
	st.SiteFuseAmps = 16
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	st.SiteFuseSafetyA = 0.5
	return st
}

func TestSetPeakLimitAcceptsValueUnderTheFuse(t *testing.T) {
	st := fusedState()
	if err := st.SetPeakLimit(7000); err != nil {
		t.Fatalf("7000 W under an 10695 W ceiling must be accepted, got %v", err)
	}
	if st.PeakLimitW != 7000 {
		t.Errorf("PeakLimitW = %.0f, want 7000", st.PeakLimitW)
	}
}

// The whole point of the rule: a limit above the fuse is not merely
// useless, it is a lie the operator reads back from /api/status.
func TestSetPeakLimitRejectsValueAboveTheFuse(t *testing.T) {
	st := fusedState()
	st.PeakLimitW = 5000
	err := st.SetPeakLimit(20000)
	if err == nil {
		t.Fatal("20000 W on an 11040 W fuse must be rejected — it can never bind")
	}
	// The operator has to be able to act on the message: it names the
	// value they sent and the ceiling that beat it.
	for _, want := range []string{"20000", "10695"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %s, got %q", want, err.Error())
		}
	}
	if st.PeakLimitW != 5000 {
		t.Errorf("a rejected value must not land: PeakLimitW = %.0f, want 5000", st.PeakLimitW)
	}
}

// The safety margin is part of the ceiling, because every import clamp
// already binds at fuse − margin. A limit in the 345 W the margin holds
// back is dead for exactly the same reason.
func TestSetPeakLimitRejectsValueInsideTheSafetyMargin(t *testing.T) {
	st := fusedState()
	if err := st.SetPeakLimit(10695); err != nil {
		t.Fatalf("the ceiling itself must be accepted, got %v", err)
	}
	if err := st.SetPeakLimit(10800); err == nil {
		t.Fatal("10800 W is above fuse−margin (10695 W) and must be rejected")
	}
}

// Zero is a real threshold in peak shaving — "correct everything above
// 0 W of import" — not the zero-means-disabled convention that
// PeakImportCeilingW and MaxExportW use. Dispatch has always read it that
// way; validation must not quietly redefine it.
func TestSetPeakLimitAcceptsZeroAsARealThreshold(t *testing.T) {
	st := fusedState()
	st.PeakLimitW = 5000
	if err := st.SetPeakLimit(0); err != nil {
		t.Fatalf("zero must be accepted, got %v", err)
	}
	if st.PeakLimitW != 0 {
		t.Fatalf("PeakLimitW = %.0f, want 0", st.PeakLimitW)
	}

	// And dispatch still shaves against it rather than treating the site
	// as unlimited. 3 kW of import over a 0 W limit must produce a target.
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st.Mode = ModePeakShaving
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Fatal("peak limit 0 must still shave 3 kW of import, got no targets")
	}
}

// Negative would order export from a knob named for an import peak.
func TestSetPeakLimitRejectsNegative(t *testing.T) {
	st := fusedState()
	st.PeakLimitW = 5000
	if err := st.SetPeakLimit(-2000); err == nil {
		t.Fatal("a negative peak limit must be rejected")
	}
	if st.PeakLimitW != 5000 {
		t.Errorf("a rejected value must not land: PeakLimitW = %.0f, want 5000", st.PeakLimitW)
	}
}

// An undescribed fuse yields no ceiling rather than an invented one —
// the same back-compat rule fuseSafetyMarginW and perPhaseOverageW keep
// for harnesses that wire only some of the fuse fields.
func TestSetPeakLimitWithoutFuseChecksOnlyTheSign(t *testing.T) {
	st := NewState(0, 50, "ferroamp") // no SiteFuse* wired
	if err := st.SetPeakLimit(99000); err != nil {
		t.Fatalf("no fuse described → no ceiling to enforce, got %v", err)
	}
	if err := st.SetPeakLimit(-1); err == nil {
		t.Fatal("the sign check does not depend on the fuse")
	}
}

// An unset limit is whatever NewState chose; validation runs on operator
// input only. A site must never fail to boot over a shaving threshold.
func TestSetPeakLimitLeavesTheDefaultAlone(t *testing.T) {
	if got := NewState(0, 50, "ferroamp").PeakLimitW; got != 5000 {
		t.Errorf("default PeakLimitW = %.0f, want the unchanged 5000", got)
	}
}

// A config reload that lowers the fuse can strand a limit that was legal
// when it was set. SetPeakLimit cannot see that coming; PeakLimitIsDead
// is what main.go asks after it re-wires the fuse fields.
func TestPeakLimitIsDeadAfterTheFuseShrinks(t *testing.T) {
	st := fusedState()
	if err := st.SetPeakLimit(9000); err != nil {
		t.Fatalf("9000 W under a 10695 W ceiling: %v", err)
	}
	if _, dead := st.PeakLimitIsDead(); dead {
		t.Fatal("9000 W under the ceiling is not dead")
	}

	st.SiteFuseAmps = 10 // 6900 W fuse, 6555 W ceiling
	ceiling, dead := st.PeakLimitIsDead()
	if !dead {
		t.Fatal("9000 W against a 6900 W fuse is dead and must be reported")
	}
	if ceiling != 6555 {
		t.Errorf("reported ceiling = %.0f, want 6555", ceiling)
	}

	st.SiteFuseAmps = 0 // fuse no longer described
	if _, dead := st.PeakLimitIsDead(); dead {
		t.Error("no fuse described → nothing to be dead against")
	}
}
