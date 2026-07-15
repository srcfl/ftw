package main

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestControlStateFromConfigAppliesSiteGain(t *testing.T) {
	cfg := &config.Config{
		Site: config.Site{
			Gain:                 0.8,
			GridToleranceW:       50,
			SlewRateW:            500,
			MinDispatchIntervalS: 5,
		},
	}
	ctrl := newControlStateFromConfig(cfg)
	if ctrl.PI.Kp != 0.8 {
		t.Fatalf("PI.Kp = %f, want configured site.gain", ctrl.PI.Kp)
	}
}
