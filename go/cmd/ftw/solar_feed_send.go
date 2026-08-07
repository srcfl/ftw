package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// solarFeedDriversFrom builds control.State.SolarFeedDrivers from config:
// the drivers whose operator armed the opt-in `solar_pv` write path. The
// gates mirror what the driver itself enforces (see the NIBE driver's
// write_cfg validation), so core never sends a payload the driver would
// refuse on configuration grounds alone:
//
//   - capabilities.http.allow_write — the host grant for http_patch;
//   - config.write.solar_pv: true  — the driver-side opt-in;
//   - config.write.max_w > 0       — the clamp ceiling; the driver
//     refuses to arm without one, so a missing/zero ceiling means the
//     operator never finished arming the feed.
//
// The Settings UI sets the first two together and requires the third,
// so a UI-armed driver always passes. Hand-written configs that arm
// only half the gates get no commands — same net effect as the driver
// refusing, minus a per-tick refusal in the log.
func solarFeedDriversFrom(drivers []config.Driver) map[string]bool {
	out := map[string]bool{}
	for _, d := range drivers {
		if d.Disabled {
			continue
		}
		if d.Capabilities.HTTP == nil || !d.Capabilities.HTTP.AllowWrite {
			continue
		}
		w, _ := d.Config["write"].(map[string]any)
		if w == nil {
			continue
		}
		if enabled, _ := w["solar_pv"].(bool); !enabled {
			continue
		}
		if configNumber(w["max_w"]) <= 0 {
			continue
		}
		out[d.Name] = true
	}
	return out
}

// configNumber reads a numeric value out of a driver's opaque config
// map. yaml.v3 decodes numbers as int (or int64 past 32 bits), the
// JSON round-trip through the settings UI as float64.
func configNumber(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

// solarFeedSender sends per-tick `solar_pv` hints with edge-triggered
// logging. Unlike battery dispatch, a refused solar feed can be a
// steady state measured in days — the pump-side enable register still
// off, the pump not detected yet — and the hint repeats every control
// tick precisely to feed the driver's dead-man switch. Logging every
// refusal would print the same line every few seconds for as long as
// the operator leaves the pump half-configured, so only transitions
// are logged: first refusal, a changed refusal, and recovery.
//
// Deliberately not routed through driverActuationTracker: its question
// — does refusing this say core cannot put power where it asked? — is
// answered no for a hint. No power was asked for, the driver's health
// is untouched by declining to relay information the device owner has
// not consented to receive, and booking the refusal would paint a
// correctly-configured read-only pump as a device fault.
type solarFeedSender struct {
	lastErr map[string]string
}

func newSolarFeedSender() *solarFeedSender {
	return &solarFeedSender{lastErr: map[string]string{}}
}

func (s *solarFeedSender) send(ctx context.Context, reg driverCommandSender, name string, payload []byte, timeout time.Duration) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := reg.Send(cmdCtx, name, payload)
	if err == nil {
		if s.lastErr[name] != "" {
			slog.Info("solar feed send recovered", "name", name)
			delete(s.lastErr, name)
		}
		return
	}
	if msg := err.Error(); s.lastErr[name] != msg {
		slog.Warn("solar feed send", "name", name, "err", err)
		s.lastErr[name] = msg
	}
}
