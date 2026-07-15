// Package selftune is the per-battery step-response calibration flow.
//
// Pauses normal control, drives each battery through a known step pattern,
// fits an ARX(1) model to the response, writes it to the BatteryModel as a
// trusted baseline (used later for hardware-health drift detection).
//
// Under the site convention (+ = charge, − = discharge), step commands use:
//   Small:  ±1000 W   — tests τ + gain at modest magnitude
//   Large:  ±3000 W   — probes saturation envelope
package selftune

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/battery"
)

// Step is one position in the self-tune state machine.
type Step string

const (
	StepStabilize    Step = "stabilize"
	StepUpSmall      Step = "step_up_small"
	StepSettleUp     Step = "settle_up"
	StepDownSmall    Step = "step_down_small"
	StepSettleDown   Step = "settle_down"
	StepUpLarge      Step = "step_up_large"
	StepSettleHighUp Step = "settle_high_up"
	StepDownLarge    Step = "step_down_large"
	StepSettleHighDown Step = "settle_high_down"
	StepFit          Step = "fit"
	StepDone         Step = "done"
)

// DurationS returns how long to stay in this step.
// Tuned so 5s control interval gives ≥5 samples per active step.
func (s Step) DurationS() int {
	switch s {
	case StepStabilize: return 15
	case StepUpSmall: return 25
	case StepSettleUp: return 15
	case StepDownSmall: return 25
	case StepSettleDown: return 15
	case StepUpLarge: return 25
	case StepSettleHighUp: return 10
	case StepDownLarge: return 25
	case StepSettleHighDown: return 10
	case StepFit: return 1
	default: return 0
	}
}

// CommandW is the command in site convention sent to the battery during this step.
func (s Step) CommandW() float64 {
	switch s {
	case StepUpSmall: return 1000       // + = charge
	case StepDownSmall: return -1000    // − = discharge
	case StepUpLarge: return 3000
	case StepDownLarge: return -3000
	default: return 0
	}
}

// Collecting reports whether we sample the response during this step.
func (s Step) Collecting() bool {
	switch s {
	case StepUpSmall, StepDownSmall, StepUpLarge, StepDownLarge: return true
	}
	return false
}

// Next returns the step after this one.
func (s Step) Next() Step {
	switch s {
	case StepStabilize: return StepUpSmall
	case StepUpSmall: return StepSettleUp
	case StepSettleUp: return StepDownSmall
	case StepDownSmall: return StepSettleDown
	case StepSettleDown: return StepUpLarge
	case StepUpLarge: return StepSettleHighUp
	case StepSettleHighUp: return StepDownLarge
	case StepDownLarge: return StepSettleHighDown
	case StepSettleHighDown: return StepFit
	case StepFit: return StepDone
	}
	return StepDone
}

// Sample is one (elapsed, command, actual, soc) observation during a step.
type Sample struct {
	ElapsedS float64
	Command  float64
	Actual   float64
	SoC      float64
}

// ModelSnapshot is a before/after comparison report used in UI + API.
type ModelSnapshot struct {
	Gain       float64 `json:"gain"`
	TauS       float64 `json:"tau_s"`
	DeadbandW  float64 `json:"deadband_w"`
	NSamples   uint64  `json:"n_samples"`
	Confidence float64 `json:"confidence"`
}

// SnapshotOf captures the current model for a before/after diff.
func SnapshotOf(m *battery.Model, dtS float64) ModelSnapshot {
	return ModelSnapshot{
		Gain:       m.SteadyStateGain(),
		TauS:       m.TimeConstantS(dtS),
		DeadbandW:  m.DeadbandW,
		NSamples:   m.NSamples,
		Confidence: m.Confidence(),
	}
}

// stepFit is the (gain, τ) extracted from one step-response curve.
type stepFit struct {
	gain, tauS float64
	valid      bool
}

// fitStepResponse recovers τ + gain from a first-order step response.
// Takes the last 50% of samples as steady-state estimate; 63.2%-crossing
// time as τ. Rejects fits where the delta was too small to be informative.
func fitStepResponse(samples []Sample) stepFit {
	if len(samples) < 3 {
		return stepFit{gain: 1, tauS: 5, valid: false}
	}
	u := samples[0].Command
	if math.Abs(u) < 100 {
		return stepFit{gain: 1, tauS: 5, valid: false}
	}
	initial := samples[0].Actual

	// Steady-state: mean of second half
	tailStart := len(samples) / 2
	var sum float64
	for _, s := range samples[tailStart:] {
		sum += s.Actual
	}
	ySS := sum / float64(len(samples)-tailStart)
	delta := ySS - initial

	gain := ySS / u
	if gain < 0.3 { gain = 0.3 }
	if gain > 1.5 { gain = 1.5 }

	// Reject tiny responses
	if math.Abs(delta) < 50 {
		return stepFit{gain: gain, tauS: 5, valid: false}
	}

	// Find 63.2% crossing
	target := initial + 0.632*delta
	tau := 5.0
	for _, s := range samples {
		if (delta > 0 && s.Actual >= target) || (delta < 0 && s.Actual <= target) {
			if s.ElapsedS > 0.5 {
				tau = s.ElapsedS
			} else {
				tau = 0.5
			}
			break
		}
	}
	if tau < 0.5 { tau = 0.5 }
	if tau > 30 { tau = 30 }

	return stepFit{gain: gain, tauS: tau, valid: true}
}

// Coordinator manages self-tune across one or more batteries. Thread-safe.
type Coordinator struct {
	mu sync.Mutex

	Active      bool
	Batteries   []string
	CurrentIdx  int
	Before      map[string]ModelSnapshot
	After       map[string]ModelSnapshot
	StartedAt   *time.Time
	LastError   string

	// Per-battery state
	currentName  string
	currentStep  Step
	stepStartedAt time.Time
	samples       []Sample
	fits          []stepFit
}

// NewCoordinator returns a fresh, idle coordinator.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		currentStep: StepDone,
		Before:      map[string]ModelSnapshot{},
		After:       map[string]ModelSnapshot{},
	}
}

// Start begins a tune for the given batteries. Captures `before` snapshots.
func (c *Coordinator) Start(batteries []string, models map[string]*battery.Model, dtS float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Active {
		return fmt.Errorf("self-tune already running")
	}
	if len(batteries) == 0 {
		return fmt.Errorf("at least one battery must be specified")
	}
	c.Before = map[string]ModelSnapshot{}
	c.After = map[string]ModelSnapshot{}
	for _, name := range batteries {
		if m, ok := models[name]; ok {
			c.Before[name] = SnapshotOf(m, dtS)
		}
	}
	c.Batteries = append([]string{}, batteries...)
	c.CurrentIdx = 0
	now := time.Now()
	c.StartedAt = &now
	c.currentName = batteries[0]
	c.currentStep = StepStabilize
	c.stepStartedAt = now
	c.samples = nil
	c.fits = nil
	c.Active = true
	c.LastError = ""
	return nil
}

// Cancel aborts the tune immediately.
func (c *Coordinator) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Active = false
	c.currentStep = StepDone
}

// CurrentCommand returns (name, command_w) if a tune is running, else ("", 0, false).
// The main control loop overrides the normal dispatch for the currently-tuning
// battery using this value.
func (c *Coordinator) CurrentCommand() (string, float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Active { return "", 0, false }
	return c.currentName, c.currentStep.CommandW(), true
}

// IsTuning reports whether this specific battery is the one being probed.
func (c *Coordinator) IsTuning(name string) bool {
	c.mu.Lock(); defer c.mu.Unlock()
	return c.Active && c.currentName == name
}

// Tick advances the state machine. Call once per control cycle.
//
//   actualLookup(name) → (actual_w, soc), or (0,0,false) if offline
//   models[name] → the battery.Model to update at end
//   dtS → control interval in seconds
//   nowMs → current epoch ms (for SetBaseline timestamps)
func (c *Coordinator) Tick(
	actualLookup func(name string) (float64, float64, bool),
	models map[string]*battery.Model,
	dtS float64,
	nowMs int64,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Active { return }

	// Record sample if we're in an active step
	if c.currentStep.Collecting() {
		if actual, soc, ok := actualLookup(c.currentName); ok {
			c.samples = append(c.samples, Sample{
				ElapsedS: time.Since(c.stepStartedAt).Seconds(),
				Command:  c.currentStep.CommandW(),
				Actual:   actual,
				SoC:      soc,
			})
		}
	}

	// Ready to advance?
	elapsed := int(time.Since(c.stepStartedAt).Seconds())
	if elapsed < c.currentStep.DurationS() { return }

	// Step just finished. If it was a collecting step, fit it.
	if c.currentStep.Collecting() && len(c.samples) > 0 {
		c.fits = append(c.fits, fitStepResponse(c.samples))
		c.samples = nil
	}

	// Advance
	c.currentStep = c.currentStep.Next()
	c.stepStartedAt = time.Now()

	if c.currentStep == StepFit {
		// Aggregate valid fits, write baseline
		validGain, validTau := 0.0, 0.0
		validCount := 0
		for _, f := range c.fits {
			if !f.valid { continue }
			validGain += f.gain
			validTau += f.tauS
			validCount++
		}
		if validCount == 0 {
			c.LastError = fmt.Sprintf(
				"self-tune for %q produced no usable step responses — battery may be saturated, offline, or commands fell within its deadband",
				c.currentName)
		} else {
			avgGain := validGain / float64(validCount)
			avgTau := validTau / float64(validCount)
			if m, ok := models[c.currentName]; ok {
				m.SetFromStepFit(avgGain, avgTau, dtS)
				m.SetBaseline(avgGain, avgTau, nowMs)
				c.After[c.currentName] = SnapshotOf(m, dtS)
			}
		}
	}

	if c.currentStep == StepDone {
		c.CurrentIdx++
		if c.CurrentIdx >= len(c.Batteries) {
			c.Active = false
			c.currentName = ""
			return
		}
		// Next battery
		c.currentName = c.Batteries[c.CurrentIdx]
		c.currentStep = StepStabilize
		c.stepStartedAt = time.Now()
		c.samples = nil
		c.fits = nil
	}
}

// Status returns a snapshot for the API.
type Status struct {
	Active         bool                     `json:"active"`
	BatteryIdx     int                      `json:"battery_index"`
	BatteryTotal   int                      `json:"battery_total"`
	CurrentBattery string                   `json:"current_battery"`
	CurrentStep    Step                     `json:"current_step"`
	StepElapsedS   float64                  `json:"step_elapsed_s"`
	TotalElapsedS  float64                  `json:"total_elapsed_s"`
	Before         map[string]ModelSnapshot `json:"before"`
	After          map[string]ModelSnapshot `json:"after"`
	LastError      string                   `json:"last_error,omitempty"`
}

func (c *Coordinator) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	stepElapsed := time.Since(c.stepStartedAt).Seconds()
	totalElapsed := 0.0
	if c.StartedAt != nil {
		totalElapsed = time.Since(*c.StartedAt).Seconds()
	}
	return Status{
		Active:         c.Active,
		BatteryIdx:     c.CurrentIdx,
		BatteryTotal:   len(c.Batteries),
		CurrentBattery: c.currentName,
		CurrentStep:    c.currentStep,
		StepElapsedS:   stepElapsed,
		TotalElapsedS:  totalElapsed,
		Before:         c.Before,
		After:          c.After,
		LastError:      c.LastError,
	}
}
