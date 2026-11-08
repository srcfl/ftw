package notifications

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// xorath, 5 Nov 2026: a battery charged 2.3 kW while FTW asked it to
// discharge 812 W, and only the box's page said so. The rule tells the owner
// once the box has said it for ten minutes, once until the device follows.
func TestControlNotFollowingTellsTheOwnerOnceAfterTenMinutes(t *testing.T) {
	pub := &fakePub{}
	// Stored rules from before the kind existed: its default holds, and it
	// is on.
	svc, clk := newSvc(pushCfg(config.NotificationRule{Type: PushDriverOffline, Enabled: false}), pub)
	status := "not_following"
	reads := 0
	svc.SetControlReader(func(time.Time) []ControlState {
		reads++
		return []ControlState{
			{Driver: "pixii", Kind: "battery", Status: status},
			{Driver: "easee", Kind: "ev", Status: "following"},
		}
	})
	step := func(d time.Duration) {
		clk.advance(d)
		svc.evaluateControl(clk.now())
	}

	svc.evaluateControl(clk.now())
	for i := 0; i < 19; i++ {
		step(30 * time.Second)
	}
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("before ten minutes: %d messages", n)
	}
	step(30 * time.Second)
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("after ten minutes: %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Kind != PushControlNotFollowing || m.Title != "A device is not following FTW" ||
		m.Body != "pixii does not do what FTW asks. Open FTW to see why." || m.Args["name"] != "pixii" || m.Device != "pixii" {
		t.Fatalf("message = %+v", m)
	}

	// Still not following: nothing more.
	step(30 * time.Minute)
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("still not following: %d messages", n)
	}

	// It follows, then fails again within the hour: the cooldown holds.
	status = "following"
	step(30 * time.Second)
	status = "not_following"
	step(30 * time.Second)
	step(11 * time.Minute)
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("again within the cooldown: %d messages", n)
	}
	// Past the hour since the first, and ten minutes into this one.
	step(30 * time.Minute)
	if n := len(pub.Messages()); n != 2 {
		t.Fatalf("again after the cooldown: %d messages, want 2", n)
	}

	// A HealthTick every few seconds asks at most every 30 s.
	before := reads
	for i := 0; i < 10; i++ {
		step(2 * time.Second)
	}
	if got := reads - before; got > 1 {
		t.Fatalf("asked %d times in 20 s", got)
	}
}

func TestControlNotFollowingStaysQuietWhenTurnedOff(t *testing.T) {
	for name, cfg := range map[string]*config.Notifications{
		"rule off":          pushCfg(config.NotificationRule{Type: PushControlNotFollowing, Enabled: false}),
		"notifications off": {Enabled: false, Events: []config.NotificationRule{{Type: PushControlNotFollowing, Enabled: true}}},
	} {
		pub := &fakePub{}
		svc, clk := newSvc(cfg, pub)
		svc.SetControlReader(func(time.Time) []ControlState {
			return []ControlState{{Driver: "pixii", Kind: "battery", Status: "not_following"}}
		})
		for i := 0; i < 60; i++ {
			clk.advance(30 * time.Second)
			svc.evaluateControl(clk.now())
		}
		if n := len(pub.Messages()); n != 0 {
			t.Fatalf("%s: %d messages", name, n)
		}
	}
}

func TestControlNotFollowingIsARuleThePhonesCanSet(t *testing.T) {
	rule, ok := findRule(DefaultRules(), PushControlNotFollowing)
	if !ok || !rule.Enabled || rule.ThresholdS != 600 || rule.CooldownS != 3600 {
		t.Fatalf("default rule = %+v, %v", rule, ok)
	}
	known := false
	for _, k := range KnownRuleTypes() {
		known = known || k == PushControlNotFollowing
	}
	if !known {
		t.Fatal("control.not_following is not a rule type a phone may set")
	}
}
