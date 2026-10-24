package main

import (
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
