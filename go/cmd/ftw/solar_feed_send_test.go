package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

func armedNibeDriver() config.Driver {
	return config.Driver{
		Name: "nibe",
		Lua:  "drivers/nibe_local.lua",
		Capabilities: config.Capabilities{
			HTTP: &config.HTTPCapability{
				AllowedHosts: []string{"192.168.1.20:8443"},
				AllowWrite:   true,
			},
		},
		Config: map[string]any{
			"write": map[string]any{"solar_pv": true, "max_w": 9000},
		},
	}
}

func TestSolarFeedDriversFrom_AllGatesArmed(t *testing.T) {
	got := solarFeedDriversFrom([]config.Driver{armedNibeDriver()})
	if !got["nibe"] || len(got) != 1 {
		t.Errorf("want {nibe:true}; got %v", got)
	}
}

func TestSolarFeedDriversFrom_HalfArmedConfigsGetNothing(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Driver)
	}{
		{"disabled driver", func(d *config.Driver) { d.Disabled = true }},
		{"no http capability", func(d *config.Driver) { d.Capabilities.HTTP = nil }},
		{"allow_write off", func(d *config.Driver) { d.Capabilities.HTTP.AllowWrite = false }},
		{"no write block", func(d *config.Driver) { delete(d.Config, "write") }},
		{"solar_pv off", func(d *config.Driver) {
			d.Config["write"].(map[string]any)["solar_pv"] = false
		}},
		{"max_w missing", func(d *config.Driver) {
			delete(d.Config["write"].(map[string]any), "max_w")
		}},
		{"max_w zero", func(d *config.Driver) {
			d.Config["write"].(map[string]any)["max_w"] = 0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := armedNibeDriver()
			tc.mutate(&d)
			if got := solarFeedDriversFrom([]config.Driver{d}); len(got) != 0 {
				t.Errorf("want empty set; got %v", got)
			}
		})
	}
}

// max_w arrives as int from yaml.v3 and float64 from the settings UI's
// JSON round-trip; both must arm the feed.
func TestSolarFeedDriversFrom_NumericTypes(t *testing.T) {
	for _, maxW := range []any{int(9000), int64(9000), float64(9000)} {
		d := armedNibeDriver()
		d.Config["write"].(map[string]any)["max_w"] = maxW
		if got := solarFeedDriversFrom([]config.Driver{d}); !got["nibe"] {
			t.Errorf("max_w %T(%v): want armed; got %v", maxW, maxW, got)
		}
	}
}

func TestSolarFeedSender_LogsRefusalOnceUntilItChanges(t *testing.T) {
	buf := captureWarnings(t)
	sender := &stubSender{handler: func(ctx context.Context, name string) error {
		return errors.New("solar_pv: register 2107 disabled")
	}}
	feed := newSolarFeedSender()
	payload := []byte(`{"action":"solar_pv","power_w":-500}`)

	for i := 0; i < 3; i++ {
		feed.send(context.Background(), sender, "nibe", payload, time.Second)
	}
	if got := strings.Count(buf.String(), "solar feed send"); got != 1 {
		t.Errorf("want 1 warning for a standing refusal, got %d:\n%s", got, buf.String())
	}

	sender.handler = func(ctx context.Context, name string) error {
		return errors.New("solar_pv: pump not detected yet")
	}
	feed.send(context.Background(), sender, "nibe", payload, time.Second)
	if got := strings.Count(buf.String(), "solar feed send"); got != 2 {
		t.Errorf("want a second warning when the refusal changes, got %d:\n%s", got, buf.String())
	}
}

func TestSolarFeedSender_RecoveryClearsTheLatch(t *testing.T) {
	buf := captureWarnings(t)
	fail := true
	sender := &stubSender{handler: func(ctx context.Context, name string) error {
		if fail {
			return errors.New("solar_pv: register 2107 disabled")
		}
		return nil
	}}
	feed := newSolarFeedSender()
	payload := []byte(`{"action":"solar_pv","power_w":-500}`)

	feed.send(context.Background(), sender, "nibe", payload, time.Second)
	fail = false
	feed.send(context.Background(), sender, "nibe", payload, time.Second)
	fail = true
	feed.send(context.Background(), sender, "nibe", payload, time.Second)

	// Refusal → recovery → same refusal again must log the refusal twice:
	// the latch resets on success so a returning fault is not silent.
	if got := strings.Count(buf.String(), "solar feed send"); got != 2 {
		t.Errorf("want 2 warnings around a recovery, got %d:\n%s", got, buf.String())
	}
	if calls := sender.recorded(); len(calls) != 3 || calls[0].payload != string(payload) {
		t.Errorf("unexpected recorded calls: %+v", calls)
	}
}
