package selftune

import (
	"math"
	"testing"

	"github.com/srcfl/ftw/go/internal/battery"
)

// ---- Step progression ----

func TestStepProgressionVisitsAllStates(t *testing.T) {
	expected := []Step{
		StepStabilize, StepUpSmall, StepSettleUp,
		StepDownSmall, StepSettleDown,
		StepUpLarge, StepSettleHighUp,
		StepDownLarge, StepSettleHighDown,
		StepFit, StepDone,
	}
	s := StepStabilize
	for _, want := range expected {
		if s != want {
			t.Errorf("expected %s, got %s", want, s)
		}
		s = s.Next()
	}
	// Done is terminal
	if StepDone.Next() != StepDone {
		t.Error("done should be terminal")
	}
}

func TestStepCommands(t *testing.T) {
	// Site convention: + = charge, − = discharge
	if StepUpSmall.CommandW() != 1000 {
		t.Error("StepUpSmall should be +1000 (charge)")
	}
	if StepDownSmall.CommandW() != -1000 {
		t.Error("StepDownSmall should be -1000 (discharge)")
	}
	if StepUpLarge.CommandW() != 3000 {
		t.Error("StepUpLarge should be +3000")
	}
	if StepDownLarge.CommandW() != -3000 {
		t.Error("StepDownLarge should be -3000")
	}
	if StepStabilize.CommandW() != 0 {
		t.Error("Stabilize should be 0")
	}
}

func TestCollectingOnlyDuringActiveSteps(t *testing.T) {
	if StepStabilize.Collecting() { t.Error("stabilize should not collect") }
	if !StepUpSmall.Collecting() { t.Error("step_up_small should collect") }
	if StepSettleUp.Collecting() { t.Error("settle_up should not collect") }
	if !StepDownLarge.Collecting() { t.Error("step_down_large should collect") }
}

// ---- fitStepResponse ----

func TestFitRecoversKnownGainAndTau(t *testing.T) {
	// Synthetic step: u=+1000, actual converges to 900 with τ=3s
	samples := make([]Sample, 0, 30)
	g := 0.9
	tau := 3.0
	for i := 0; i < 30; i++ {
		tSec := float64(i) * 0.5
		y := g * 1000 * (1 - math.Exp(-tSec/tau))
		samples = append(samples, Sample{ElapsedS: tSec, Command: 1000, Actual: y})
	}
	fit := fitStepResponse(samples)
	if !fit.valid {
		t.Fatal("expected valid fit")
	}
	if math.Abs(fit.gain-0.9) > 0.05 {
		t.Errorf("gain: got %.3f", fit.gain)
	}
	if math.Abs(fit.tauS-3.0) > 1.0 {
		t.Errorf("τ: got %.2f", fit.tauS)
	}
}

func TestFitHandlesNegativeStep(t *testing.T) {
	// Discharge step: u=-1000, converges to -950 with τ=2s
	samples := make([]Sample, 0, 30)
	for i := 0; i < 30; i++ {
		tSec := float64(i) * 0.5
		y := 0.95 * -1000 * (1 - math.Exp(-tSec/2.0))
		samples = append(samples, Sample{ElapsedS: tSec, Command: -1000, Actual: y})
	}
	fit := fitStepResponse(samples)
	if !fit.valid { t.Fatal("expected valid fit") }
	if math.Abs(fit.gain-0.95) > 0.05 { t.Errorf("gain: %.3f", fit.gain) }
}

func TestFitRejectsTinyResponse(t *testing.T) {
	// Battery didn't respond to command
	samples := make([]Sample, 0, 20)
	for i := 0; i < 20; i++ {
		samples = append(samples, Sample{
			ElapsedS: float64(i) * 0.5,
			Command:  1000,
			Actual:   5, // essentially zero
		})
	}
	fit := fitStepResponse(samples)
	if fit.valid {
		t.Error("expected invalid fit for tiny response")
	}
}

func TestFitRequires3OrMoreSamples(t *testing.T) {
	fit := fitStepResponse([]Sample{{Command: 1000}, {Command: 1000}})
	if fit.valid { t.Error("expected invalid for <3 samples") }
}

// ---- Coordinator ----

func TestStartCapturesBeforeSnapshots(t *testing.T) {
	models := map[string]*battery.Model{
		"a": battery.New("a"),
	}
	c := NewCoordinator()
	if err := c.Start([]string{"a"}, models, 5); err != nil {
		t.Fatal(err)
	}
	if !c.Active { t.Error("should be active") }
	if len(c.Before) != 1 { t.Errorf("before: %+v", c.Before) }
}

func TestStartRejectsDoubleStart(t *testing.T) {
	c := NewCoordinator()
	c.Start([]string{"a"}, map[string]*battery.Model{}, 5)
	if err := c.Start([]string{"b"}, map[string]*battery.Model{}, 5); err == nil {
		t.Error("expected double-start to fail")
	}
}

func TestStartRejectsEmpty(t *testing.T) {
	c := NewCoordinator()
	if err := c.Start(nil, nil, 5); err == nil {
		t.Error("expected empty list to fail")
	}
}

func TestCancelStopsSelfTune(t *testing.T) {
	c := NewCoordinator()
	c.Start([]string{"a"}, map[string]*battery.Model{}, 5)
	c.Cancel()
	if c.Active { t.Error("should be inactive after cancel") }
}

func TestCurrentCommandReturnsStepCommand(t *testing.T) {
	c := NewCoordinator()
	c.Start([]string{"a"}, map[string]*battery.Model{}, 5)
	name, cmd, ok := c.CurrentCommand()
	if !ok { t.Fatal("should return command while active") }
	if name != "a" { t.Errorf("name: %s", name) }
	if cmd != 0 { t.Errorf("initial step is stabilize → 0, got %f", cmd) }
}

func TestStatusReturnsCorrectShape(t *testing.T) {
	c := NewCoordinator()
	c.Start([]string{"a"}, map[string]*battery.Model{}, 5)
	s := c.Status()
	if !s.Active { t.Error("status should reflect active") }
	if s.CurrentBattery != "a" { t.Errorf("current: %q", s.CurrentBattery) }
	if s.BatteryTotal != 1 { t.Errorf("total: %d", s.BatteryTotal) }
}
