package flexload

import (
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/thermalmodel"
)

// TestPauseIsNetWin checks the reheat-vs-saving economics that gate any
// reduction below the comfort target: pausing only pays when the price now
// exceeds the forecast price at the reheat moment.
func TestPauseIsNetWin(t *testing.T) {
	m := trainedModel()
	const pauseH = 2.0

	// Case 1: reheat later is CHEAPER than now → pausing is a net win.
	svcCheaperLater := &Service{
		PriceAt: func(at time.Time) (float64, bool) {
			// now = 300, reheat (2h later) = 100
			if at.After(time.Now().Add(time.Hour)) {
				return 100, true
			}
			return 300, true
		},
	}
	worth, reason := svcCheaperLater.pauseIsNetWin(m, 21, 0, 1, pauseH, time.Now().UnixMilli())
	if !worth {
		t.Errorf("expected net-win pause when reheat is cheaper: %s", reason)
	}

	// Case 2: reheat later is PRICIER than now → must NOT pause (the exact
	// "dumb lowering" the operator warned about — cool now, reheat expensive).
	svcPricierLater := &Service{
		PriceAt: func(at time.Time) (float64, bool) {
			if at.After(time.Now().Add(time.Hour)) {
				return 500, true
			}
			return 200, true
		},
	}
	worth, reason = svcPricierLater.pauseIsNetWin(m, 21, 0, 1, pauseH, time.Now().UnixMilli())
	if worth {
		t.Errorf("must not pause when reheat is pricier than now: %s", reason)
	}

	// Case 3: no price model → never a speculative reduction.
	svcNoPrice := &Service{}
	if worth, _ := svcNoPrice.pauseIsNetWin(m, 21, 0, 1, pauseH, time.Now().UnixMilli()); worth {
		t.Error("no price model must block any reduction")
	}
}

// TestStoveDecisionHoldsComfortWithoutLearning verifies that with a fresh
// (untrained) model and no learned firing cycles, a detected stove only
// suppresses pre-heat and holds the comfort target — never a deep reduction.
func TestStoveDecisionHoldsComfortWithoutLearning(t *testing.T) {
	svc := &Service{
		thermal: map[string]*thermalmodel.Model{"living": thermalmodel.NewModel()},
		stove:   map[string]*ExternalHeatDetector{"living": {}},
	}
	// Force the detector active.
	now := time.Now().UnixMilli()
	det := svc.stove["living"]
	det.active = true
	det.sinceMs = now
	det.lastDetectMs = now

	d := Device{Type: "thermostat", DriverName: "living", MinC: 18, MaxC: 23, TargetC: 21, Mode: "simple"}
	sp, active, _ := svc.stoveDecision(d, 22.0, d.TargetC, 0, now)
	if !active {
		t.Fatal("stove should be active")
	}
	if sp != d.TargetC {
		t.Errorf("without learning, stove pause must hold comfort target %.1f, got %.1f", d.TargetC, sp)
	}
}
