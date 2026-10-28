package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fakeGitHub struct {
	master   string
	pulls    []pull
	files    map[int][]string
	contexts map[string]map[string]bool
}

func (f fakeGitHub) MasterSHA() (string, error)     { return f.master, nil }
func (f fakeGitHub) Pulls() ([]pull, error)         { return f.pulls, nil }
func (f fakeGitHub) Files(pr int) ([]string, error) { return f.files[pr], nil }
func (f fakeGitHub) Contexts(sha string) (map[string]bool, error) {
	return f.contexts[sha], nil
}
func (f fakeGitHub) SetStatus(sha, context, state, description string) error { return nil }

func pr(n int, sha, repo string, draft bool) pull {
	p := pull{Number: n, Draft: draft}
	p.Head.SHA, p.Head.Repo.FullName = sha, repo
	return p
}

func TestPlanRunsWhatIsDue(t *testing.T) {
	gh := fakeGitHub{
		master: "m1",
		pulls: []pull{
			pr(1, "a1", "srcfl/ftw", false),   // control code: short set
			pr(2, "b1", "srcfl/ftw", false),   // docs only: nothing
			pr(3, "c1", "someone/ftw", false), // a fork: never by default
			pr(4, "d1", "srcfl/ftw", false),   // already has its status
		},
		files: map[int][]string{
			1: {"go/internal/control/dispatch.go"}, 2: {"docs/x.md"},
			3: {"go/internal/ocpp/handlers.go"}, 4: {"go/internal/loadpoint/x.go"},
		},
		contexts: map[string]map[string]bool{"d1": {ctxShort: true}},
	}
	cfg := config{Repo: "srcfl/ftw", Drafts: true, RandomAt: 2, RandomSeeds: 10}
	night := time.Date(2026, 10, 23, 3, 0, 0, 0, time.Local)
	jobs, err := plan(gh, cfg, night, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range jobs {
		got = append(got, j.String()+" "+j.Set)
	}
	want := []string{"siterig/full on master m1 full", "siterig/short on #1 a1 short", "siterig/random on master m1 random:"}
	if len(got) != len(want) {
		t.Fatalf("jobs %v, want %v", got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("job %d: %q, want %q", i, got[i], want[i])
		}
	}

	// Master already tested, random already run today, before the hour.
	gh.contexts["m1"] = map[string]bool{ctxFull: true}
	jobs, _ = plan(gh, cfg, night, "2026-10-23")
	if len(jobs) != 1 || jobs[0].PR != 1 {
		t.Fatalf("second look: %v", jobs)
	}
	cfg.AllowForks = true
	jobs, _ = plan(gh, cfg, night.Add(-2*time.Hour), "")
	if len(jobs) != 2 || jobs[1].PR != 3 {
		t.Fatalf("with forks allowed, before the random hour: %v", jobs)
	}
}

func TestTouchesWatched(t *testing.T) {
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"go/cmd/ftw/main.go", true},
		{"go/internal/control/dispatch.go", true},
		{"go/internal/loadpoint/loadpoint.go", true},
		{"go/internal/ocpp/handlers.go", true},
		{"go/internal/telemetry/store.go", true},
		{"go/internal/drivers/lua.go", true},
		{"go/internal/mpc/mpc.go", true},
		{"go/internal/optimizercontract/contract.go", true},
		{"go/internal/config/config.go", true},
		{"go/internal/configreload/apply.go", true},
		{"go/internal/cars/cars.go", true},
		{"go/internal/sitepower/sitepower.go", true},
		{"go/internal/units/charge_steps.go", true},
		{"go/internal/gridcost/gridcost.go", true},
		{"go/internal/state/store.go", true},
		{"go/internal/events/bus.go", true},
		{"drivers/sungrow.lua", true},
		{"drivers/BUNDLED_SOURCE.json", true},
		{"optimizer/native/bundle/manifest.json", true},
		{"go/test/site/rig_test.go", true},
		{"go/test/site/runner/runner.go", true},
		{"docs/architecture.md", false},
		{"web/components/theme.css", false},
		{"go/internal/api/api.go", false},
		{"go/internal/driverrepo/repo.go", false}, // shares a prefix with drivers/, not a path
		{"go/internal/configx/x.go", false},       // shares a prefix with config/
		{"optimizer/native/src/main.rs", false},   // only the bundle ships
		{"go/cmd/ftwctl/main.go", false},          // shares a prefix with cmd/ftw/
		{"web/drivers/list.js", false},            // drivers/ only at the root
		{".changeset/a.md", false},
	} {
		if got := touchesWatched([]string{tc.file}); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.file, got, tc.want)
		}
	}
	if touchesWatched(nil) {
		t.Error("no files: watched")
	}
	if !touchesWatched([]string{"README.md", "go/internal/mpc/plan.go"}) {
		t.Error("one watched file among others: not watched")
	}
}

// TestWatchedPathsCoverImports fails when a control package starts to
// import an internal package that is not watched, so a change there would
// skip the rig.
func TestWatchedPathsCoverImports(t *testing.T) {
	const module = "github.com/srcfl/ftw/go/internal/"
	args := []string{"list", "-deps"}
	for _, p := range controlPackages {
		if !touchesWatched([]string{"go/internal/" + p + "/x.go"}) {
			t.Errorf("control package %s is not watched", p)
		}
		args = append(args, module+p)
	}
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		rest, ok := strings.CutPrefix(dep, module)
		if !ok {
			continue
		}
		if !touchesWatched([]string{"go/internal/" + rest + "/x.go"}) {
			t.Errorf("%s imports %s, which is not watched", strings.Join(controlPackages, ", "), rest)
		}
	}
}

func TestDescribe(t *testing.T) {
	state, desc := describe([]byte(`[{"name":"s1-25","verdict":"pass"},{"name":"s2-16","verdict":"known"},{"name":"s9-25","verdict":"fail"}]`), 24*time.Minute)
	if state != "failure" || desc != "1 pass, 1 known, 1 fail of 3 in 24m0s: s9-25" {
		t.Fatalf("%s %q", state, desc)
	}
	state, _ = describe([]byte(`[{"name":"s1-25","verdict":"pass"},{"name":"s2-16","verdict":"known"}]`), time.Minute)
	if state != "success" {
		t.Fatalf("known only: %s", state)
	}
	if state, _ := describe(nil, time.Minute); state != "error" {
		t.Fatalf("no summary: %s", state)
	}
	long := `[` + strings.Repeat(`{"name":"a-very-long-scenario-name","verdict":"fail"},`, 9) + `{"name":"x","verdict":"fail"}]`
	if _, desc := describe([]byte(long), time.Minute); len(desc) > 140 {
		t.Fatalf("%d characters", len(desc))
	}
}
