//go:build unix

package site

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// These run in every go test, without a simulator.

func TestShardsCoverEveryScenarioOnce(t *testing.T) {
	for _, set := range []string{"short", "full"} {
		all := selected(t, set)
		for n := 1; n <= 7; n++ {
			seen := map[string]int{}
			for k := 1; k <= n; k++ {
				for _, sc := range shard(t, all, fmtShard(k, n)) {
					seen[sc.name]++
				}
			}
			for _, sc := range all {
				if seen[sc.name] != 1 {
					t.Fatalf("%s in %d shards: %s runs %d times", set, n, sc.name, seen[sc.name])
				}
			}
		}
	}
}

func fmtShard(k, n int) string { return string(rune('0'+k)) + "/" + string(rune('0'+n)) }

func TestFullSetRunsTheLongRestart(t *testing.T) {
	names := map[string]bool{}
	for _, sc := range selected(t, "full") {
		names[sc.name] = true
	}
	if !names["s7-25"] || names["s7s-25"] || len(names) != len(scenarios)-1 {
		t.Fatalf("full set %v", names)
	}
}

// A known line must name a criterion its judge reports.
func TestKnownLinesNameCriteria(t *testing.T) {
	for _, sc := range scenarios {
		_, criteria := sc.judge(&runData{marks: map[string]float64{}, meta: map[string]any{"fuse": 25.0}})
		have := map[string]bool{}
		for _, c := range criteria {
			have[c.Name] = true
		}
		for name := range sc.known {
			if !have[name] {
				t.Errorf("%s: known line names no criterion: %q", sc.name, name)
			}
		}
	}
	_, criteria := judgeInvariants(&runData{marks: map[string]float64{}, meta: map[string]any{"fuse": 25.0}})
	have := map[string]bool{}
	for _, c := range criteria {
		have[c.Name] = true
		if invariantKey[c.Name] == "" {
			t.Errorf("invariant %q has no first_violation_s key", c.Name)
		}
	}
	for name := range randomTolerated {
		if !have[name] {
			t.Errorf("tolerated line names no invariant: %q", name)
		}
	}
}

func TestRunsOver(t *testing.T) {
	d := &runData{}
	for i, a := range []float64{20, 25, 26, 26, 20, 25, 20} {
		d.samples = append(d.samples, &sample{T: float64(i), PhaseA: [3]float64{a, 0, 0}})
	}
	// As the Python rig counted: a run lasts until the first sample under
	// the limit, and each sample over it adds the time since the last one.
	longest, total := d.runsOver(0, 10, 24)
	if longest != 3 || total != 4 {
		t.Fatalf("longest %v total %v, want 3 and 4", longest, total)
	}
}

func TestRandomPlansAreSeeded(t *testing.T) {
	a, b, c := drawPlan(3), drawPlan(3), drawPlan(4)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	jc, _ := json.Marshal(c)
	if string(ja) != string(jb) || string(ja) == string(jc) {
		t.Fatal("seed 3 twice differs, or seed 4 is the same")
	}
	for seed := int64(1); seed <= 30; seed++ {
		p := drawPlan(seed)
		for _, a := range p.Actions {
			if a.AtS < 40 || a.AtS > p.DurationS-60 {
				t.Fatalf("seed %d: %s at %.0f s", seed, a.Do, a.AtS)
			}
		}
		for _, x := range p.HouseA {
			if x > p.FuseA/2 {
				t.Fatalf("seed %d: house %.1f A on %.0f A", seed, x, p.FuseA)
			}
		}
	}
}

// An over-limit stretch right after an outside step is the house's; the
// same stretch with no step before it is FTW's own and fails after 3 s.
func TestInvariantsTellStepsFromOwnRaises(t *testing.T) {
	mk := func(withStep bool) *runData {
		d := &runData{marks: map[string]float64{"start": 0, "end": 100}, meta: map[string]any{"fuse": 25.0}}
		for i := 0; i <= 100; i++ {
			a := 10.0
			if i >= 50 && i < 56 {
				a = 26 // 6 s over the 25 A fuse
			}
			d.samples = append(d.samples, &sample{T: float64(i), PhaseA: [3]float64{a, a, a}, Bat: plantDevice{SoCPct: 50}})
		}
		if withStep {
			d.events = append(d.events, houseEventLine{T: 49.5, Event: houseEvent{Do: "loads"}})
		}
		return d
	}
	failed := func(cs []criterion) []string {
		var out []string
		for _, c := range cs {
			if !c.OK {
				out = append(out, c.Name)
			}
		}
		return out
	}
	if _, cs := judgeInvariants(mk(true)); len(failed(cs)) != 0 {
		t.Fatalf("after a step: %v", failed(cs))
	}
	_, cs := judgeInvariants(mk(false))
	if f := failed(cs); len(f) != 1 || f[0] != "over the limit at most 3 s after FTW's own raise" {
		t.Fatalf("without a step: %v", f)
	}
}

// s18 at margin 0: Core holds the phases at the 16 A line, so the house's own
// 0.1 A step lands over it until Core cuts. That stretch is the house's, not
// an own command. The same overage at any other time is Core's and fails.
func TestS18OwnCommandsSkipTheHouseStep(t *testing.T) {
	const own = "own commands stay inside the rated fuse"
	mk := func(overFrom float64) *runData {
		d := &runData{marks: map[string]float64{"charge_now": 0, "small_step": 100, "no_room": 200, "room_returns": 300, "end": 450}}
		for i := 0; i <= 450; i++ {
			a := 16.0
			if float64(i) >= overFrom && float64(i) < overFrom+4 {
				a = 16.1
			}
			d.samples = append(d.samples, &sample{T: float64(i), PhaseA: [3]float64{a, a, a}})
		}
		return d
	}
	ok := func(d *runData) bool {
		_, cs := judgeS18(d)
		for _, c := range cs {
			if c.Name == own {
				return c.OK
			}
		}
		t.Fatalf("no %q criterion", own)
		return false
	}
	if !ok(mk(100)) {
		t.Error("4 s at 16.1 A right after the house's step failed")
	}
	for _, at := range []float64{50, 150} {
		if ok(mk(at)) {
			t.Errorf("4 s at 16.1 A from %.0f s with no outside step passed", at)
		}
	}
}

// s8 at margin 0: the car settles at the fuse rating, as the owner's rule
// says. A phase at exactly the fuse for the whole run passes; 0.1 A above
// it for longer than the charger's delay + 7 s fails. Before, the judge
// counted every second above fuse - 1 A, and s8-60-25 failed at 25 A.
func TestS8JudgesAtTheFuse(t *testing.T) {
	const over = "no longer than the charger's delay + 7 s"
	ok := func(a float64) bool {
		d := &runData{marks: map[string]float64{"charge_now": 0, "end": 300}, meta: map[string]any{"fuse": 25.0, "charger_delay_s": 60.0}}
		for i := 0; i <= 300; i++ {
			d.samples = append(d.samples, &sample{T: float64(i), PhaseA: [3]float64{a, 15, 15}})
		}
		_, cs := judgeS8(d)
		for _, c := range cs {
			if strings.HasSuffix(c.Name, over) {
				return c.OK
			}
		}
		t.Fatalf("no criterion ending %q", over)
		return false
	}
	if !ok(25) {
		t.Error("a phase at exactly the 25 A fuse failed")
	}
	if ok(25.1) {
		t.Error("a phase 0.1 A over the 25 A fuse for 300 s passed")
	}
}

// The limit is the fuse rating: a phase may sit at exactly the fuse until
// the end of the run, and 0.1 A above it fails once the time budget is up.
func TestInvariantsJudgeAtTheFuse(t *testing.T) {
	const fuse = 25.0
	const own = "over the limit at most 3 s after FTW's own raise"
	const step = "over the limit no longer than an outside step allows"
	for _, c := range []struct {
		name     string
		a        float64
		to       int // the level holds from 50 s until this second
		withStep bool
		want     []string
	}{
		{"at the fuse", fuse, 101, false, nil},
		{"at the fuse after a step", fuse, 101, true, nil},
		// 20 s is past the 3 s for FTW's own raise, and past the
		// 2 + 2 + 3 s a step allows while the battery can answer.
		{"0.1 A over", fuse + 0.1, 70, false, []string{own}},
		{"0.1 A over after a step", fuse + 0.1, 70, true, []string{step}},
	} {
		d := &runData{marks: map[string]float64{"start": 0, "end": 100}, meta: map[string]any{"fuse": fuse}}
		for i := 0; i <= 100; i++ {
			a := 10.0
			if i >= 50 && i < c.to {
				a = c.a
			}
			d.samples = append(d.samples, &sample{T: float64(i), PhaseA: [3]float64{a, a, a}, Bat: plantDevice{SoCPct: 50}})
		}
		if c.withStep {
			d.events = append(d.events, houseEventLine{T: 49.5, Event: houseEvent{Do: "loads"}})
		}
		var failed []string
		_, cs := judgeInvariants(d)
		for _, x := range cs {
			if !x.OK {
				failed = append(failed, x.Name)
			}
		}
		if !slices.Equal(failed, c.want) {
			t.Errorf("%s: failed %v, want %v", c.name, failed, c.want)
		}
	}
}

// A run that selects planner_self plans in self_consumption; any other run
// selects planner_passive_arbitrage and plans in passive_arbitrage. s15bat
// needs Self, where PV surplus charges the battery (#1875).
func TestRigModeMatchesItsPlannerConfig(t *testing.T) {
	for _, tc := range []struct{ spec, mode, planner string }{
		{"", "planner_passive_arbitrage", "passive_arbitrage"},
		{"planner_self", "planner_self", "self_consumption"},
	} {
		st := newStack(nil, lane(21700), t.TempDir(), stackSpec{fuseA: 16, mode: tc.spec})
		st.prices = newPriceServer(time.Now())
		path, err := st.ftwConfig()
		st.prices.close()
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Planner struct {
				Mode string `json:"mode"`
			} `json:"planner"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			t.Fatal(err)
		}
		if st.mode() != tc.mode || cfg.Planner.Mode != tc.planner {
			t.Errorf("spec mode %q: selects %q and plans in %q, want %q and %q",
				tc.spec, st.mode(), cfg.Planner.Mode, tc.mode, tc.planner)
		}
	}
}
