//go:build unix

// Package site runs the real FTW binary against srcfl/device-simulator: a
// Sungrow hybrid over Modbus, an SDM630 site meter behind an IEC gG fuse on
// each phase, and OCPP 1.6 chargers. Each scenario builds a fresh house and
// a fresh state.db, drives it, records the simulator and FTW once a second
// and judges the run. See README.md for the scenarios and how to run them.
package site

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// rigEnv is what every scenario shares.
type rigEnv struct {
	root      string // repository root: drivers/ and web/
	ftwBin    string
	devsimBin string
	out       string
}

func repoRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "Makefile")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repository root")
	return ""
}

// devsimCommit is the device-simulator commit the rig runs, from
// devsim.version.
func devsimCommit(t *testing.T, root string) string {
	b, err := os.ReadFile(filepath.Join(root, "go", "test", "site", "devsim.version"))
	if err != nil {
		t.Fatal(err)
	}
	c := strings.TrimSpace(string(b))
	if len(c) != 40 {
		t.Fatalf("devsim.version: want a full commit hash, got %q", c)
	}
	return c
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// buildDevsim builds the pinned device-simulator, from FTW_SITE_DEVSIM_SRC
// when set (it must be at the pinned commit) or from a clone in the cache.
func buildDevsim(t *testing.T, root, bin string) {
	commit := devsimCommit(t, root)
	src := os.Getenv("FTW_SITE_DEVSIM_SRC")
	if src == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		src = filepath.Join(cache, "ftw-site", "device-simulator-"+commit[:12])
		if _, err := os.Stat(filepath.Join(src, "go.mod")); err != nil {
			os.RemoveAll(src)
			mustRun(t, "", "git", "init", "-q", src)
			mustRun(t, src, "git", "fetch", "-q", "--depth=1", "https://github.com/srcfl/device-simulator", commit)
			mustRun(t, src, "git", "checkout", "-q", "FETCH_HEAD")
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = src
	head, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(head)) != commit {
		t.Fatalf("device-simulator in %s is at %s, not the pinned %s", src, strings.TrimSpace(string(head)), commit)
	}
	cmd = exec.Command("go", "build", "-o", bin, "./cmd/simulator/")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build device-simulator: %v\n%s", err, out)
	}
}

func setup(t *testing.T) *rigEnv {
	root := repoRoot(t)
	for _, f := range []string{"sdm630.lua", "sungrow.lua"} {
		if _, err := os.Stat(filepath.Join(root, "drivers", f)); err != nil {
			t.Fatalf("drivers/%s missing: run make drivers first", f)
		}
	}
	out := os.Getenv("FTW_SITE_OUT")
	if out == "" {
		var err error
		if out, err = os.MkdirTemp("", "ftw-site-"); err != nil {
			t.Fatal(err)
		}
	}
	env := &rigEnv{root: root, out: out,
		ftwBin: os.Getenv("FTW_SITE_FTW_BIN"), devsimBin: os.Getenv("FTW_SITE_DEVSIM_BIN")}
	bin := filepath.Join(out, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if env.ftwBin == "" {
		env.ftwBin = filepath.Join(bin, "ftw")
		mustRun(t, filepath.Join(root, "go"), "go", "build", "-o", env.ftwBin, "./cmd/ftw")
	}
	if env.devsimBin == "" {
		env.devsimBin = filepath.Join(bin, "devsim")
		buildDevsim(t, root, env.devsimBin)
	}
	t.Logf("results in %s", out)
	return env
}

// selected resolves FTW_SITE: "short" (the pull-request set), "full" (every
// scenario and every saved random case), "random:<first seed>:<count>", or
// a comma-separated list of names.
func selected(t *testing.T, which string) []scenario {
	if rs, ok := randomSet(which); ok {
		return rs
	}
	var out []scenario
	switch which {
	case "full", "all":
		for _, sc := range scenarios {
			if sc.name != "s7s-25" { // the full set runs the long s7
				out = append(out, sc)
			}
		}
		out = append(out, foundScenarios(repoRoot(t))...)
	case "short":
		for _, sc := range scenarios {
			if sc.short {
				out = append(out, sc)
			}
		}
	default:
		byName := map[string]scenario{}
		for _, sc := range scenarios {
			byName[sc.name] = sc
		}
		for _, n := range strings.Split(which, ",") {
			sc, ok := byName[strings.TrimSpace(n)]
			if !ok {
				t.Fatalf("unknown scenario %q", n)
			}
			out = append(out, sc)
		}
	}
	return out
}

// shard picks shard k of n (FTW_SITE_SHARD "k/n").
func shard(t *testing.T, scs []scenario, spec string) []scenario {
	if spec == "" {
		return scs
	}
	k, n := 0, 0
	if _, err := fmt.Sscanf(spec, "%d/%d", &k, &n); err != nil || n < 1 || k < 1 || k > n {
		t.Fatalf("FTW_SITE_SHARD %q: want k/n", spec)
	}
	return split(scs, n)[k-1]
}

// split deals scenarios into n groups of about equal time: longest first,
// each to the group with the least time so far.
func split(scs []scenario, n int) [][]scenario {
	sorted := append([]scenario(nil), scs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].minutes > sorted[j].minutes })
	load := make([]float64, n)
	out := make([][]scenario, n)
	for _, sc := range sorted {
		best := 0
		for i := range load {
			if load[i] < load[best] {
				best = i
			}
		}
		load[best] += sc.minutes
		out[best] = append(out[best], sc)
	}
	return out
}

// outcome is one scenario's line in summary.json.
type outcome struct {
	Name      string      `json:"name"`
	Dir       string      `json:"dir"`
	Verdict   string      `json:"verdict"` // pass, known, fail
	DurationS float64     `json:"duration_s"`
	Criteria  []criterion `json:"criteria"`
	Known     []string    `json:"known,omitempty"`
	Failures  []string    `json:"failures,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// TestSite runs the scenarios FTW_SITE names. FTW_SITE_LANES runs that many
// at once on ports 100 apart from FTW_SITE_PORT_BASE (default 18700);
// FTW_SITE_SHARD splits a set over machines; FTW_SITE_OUT keeps the run
// directories.
func TestSite(t *testing.T) {
	which := os.Getenv("FTW_SITE")
	if which == "" {
		t.Skip("set FTW_SITE=short, full or a list of scenarios; see go/test/site/README.md")
	}
	scs := shard(t, selected(t, which), os.Getenv("FTW_SITE_SHARD"))
	base := 18700
	if s := os.Getenv("FTW_SITE_PORT_BASE"); s != "" {
		var err error
		if base, err = strconv.Atoi(s); err != nil {
			t.Fatalf("FTW_SITE_PORT_BASE: %v", err)
		}
	}
	lanes := 1
	if s := os.Getenv("FTW_SITE_LANES"); s != "" {
		var err error
		if lanes, err = strconv.Atoi(s); err != nil || lanes < 1 {
			t.Fatalf("FTW_SITE_LANES %q", s)
		}
	}
	env := setup(t)
	var mu sync.Mutex
	var outcomes []outcome
	t.Run("lanes", func(t *testing.T) {
		for k, group := range split(scs, lanes) {
			k, group := k, group
			t.Run(fmt.Sprintf("lane%d", k+1), func(t *testing.T) {
				if lanes > 1 {
					t.Parallel()
				}
				for _, sc := range group {
					sc := sc
					t.Run(sc.name, func(t *testing.T) {
						o := runScenario(env, lane(base+100*k), sc)
						mu.Lock()
						outcomes = append(outcomes, o)
						mu.Unlock()
						if strings.HasPrefix(sc.dir, "random-") && o.Verdict == "fail" && o.Error == "" {
							if kept := keepFailure(filepath.Join(env.out, sc.dir), env.out, o); kept != "" {
								t.Logf("kept as %s; copy it to go/test/site/testdata/found/ to run it in the full set", kept)
							}
						}
						t.Logf("%s in %.0f s: %v", o.Verdict, o.DurationS, o.Criteria)
						for _, k := range o.Known {
							t.Logf("fails as known: %s", k)
						}
						if o.Error != "" {
							t.Fatal(o.Error)
						}
						for _, f := range o.Failures {
							t.Error(f)
						}
					})
				}
			})
		}
	})
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Name < outcomes[j].Name })
	writeSummary(t, env.out, outcomes)
}

func runScenario(env *rigEnv, l lane, sc scenario) outcome {
	o := outcome{Name: sc.name, Dir: sc.dir, Verdict: "fail"}
	rundir := filepath.Join(env.out, sc.dir)
	t0 := time.Now()
	meta, err := sc.run(&runCtx{env: env, lane: l, rundir: rundir})
	o.DurationS = time.Since(t0).Round(time.Second).Seconds()
	if meta != nil {
		b, _ := json.MarshalIndent(meta, "", "  ")
		os.WriteFile(filepath.Join(rundir, "meta.json"), b, 0o644)
	}
	if err != nil {
		o.Error = err.Error()
		return o
	}
	return judgeRun(sc, rundir, meta, o)
}

// judgeRun judges a recorded run against the scenario's criteria and its
// known and tolerated lines, and writes result.json.
func judgeRun(sc scenario, rundir string, meta map[string]any, o outcome) outcome {
	// Numbers go through JSON, as a rerun from the run directory sees them.
	var m map[string]any
	b, _ := json.Marshal(meta)
	json.Unmarshal(b, &m)
	d, err := loadRun(rundir, m)
	if err != nil {
		o.Error = err.Error()
		return o
	}
	// The criteria count seconds. A host that sleeps or stalls (a laptop
	// lid) stretches the run and leaves holes in the record. A Core restart
	// costs a sample or two at most: FTW refuses at once or times out in 5 s.
	if gap, at := d.longestGap(); gap > 15 {
		o.Error = fmt.Sprintf("the recorder missed %.0f s at %.0f s into the run: the host paused, so the timing proves nothing", gap, at)
		return o
	}
	numbers, criteria := sc.judge(d)
	o.Criteria = criteria
	for _, c := range criteria {
		if finding, ok := sc.tolerate[c.Name]; ok {
			if !c.OK {
				o.Known = append(o.Known, fmt.Sprintf("%s: %s (tolerated)", c.Name, finding))
			}
			continue
		}
		finding, known := sc.known[c.Name]
		switch {
		case known && c.OK:
			o.Failures = append(o.Failures, fmt.Sprintf("%q passes now: delete its known line (%s)", c.Name, finding))
		case known:
			o.Known = append(o.Known, fmt.Sprintf("%s: %s", c.Name, finding))
		case !c.OK:
			o.Failures = append(o.Failures, fmt.Sprintf("%q fails", c.Name))
		}
	}
	for name := range sc.known {
		found := false
		for _, c := range criteria {
			found = found || c.Name == name
		}
		if !found {
			o.Failures = append(o.Failures, fmt.Sprintf("known line names no criterion: %q", name))
		}
	}
	switch {
	case len(o.Failures) > 0:
		o.Verdict = "fail"
	case len(o.Known) > 0:
		o.Verdict = "known"
	default:
		o.Verdict = "pass"
	}
	res, _ := json.MarshalIndent(map[string]any{"verdict": o.Verdict, "criteria": criteria, "known": o.Known,
		"failures": o.Failures, "duration_s": o.DurationS, "metrics": numbers, "meta": meta, "samples": len(d.samples)}, "", "  ")
	os.WriteFile(filepath.Join(rundir, "result.json"), res, 0o644)
	return o
}

// TestJudge judges the recorded runs in FTW_SITE_JUDGE again with the
// criteria as they are now, without running anything: a changed criterion
// or known line is checked against a long run in seconds.
func TestJudge(t *testing.T) {
	dir := os.Getenv("FTW_SITE_JUDGE")
	if dir == "" {
		t.Skip("set FTW_SITE_JUDGE to a results directory of an earlier run")
	}
	var outs []outcome
	for _, sc := range selected(t, "full") {
		rundir := filepath.Join(dir, sc.dir)
		b, err := os.ReadFile(filepath.Join(rundir, "meta.json"))
		if err != nil {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(b, &meta); err != nil {
			t.Fatalf("%s: %v", rundir, err)
		}
		o := outcome{Name: sc.name, Dir: sc.dir, Verdict: "fail"}
		if b, err := os.ReadFile(filepath.Join(rundir, "result.json")); err == nil {
			var prev struct {
				DurationS float64 `json:"duration_s"`
			}
			json.Unmarshal(b, &prev)
			o.DurationS = prev.DurationS
		}
		o = judgeRun(sc, rundir, meta, o)
		t.Logf("%s: %s %v", sc.name, o.Verdict, append(o.Failures, o.Known...))
		outs = append(outs, o)
	}
	if len(outs) == 0 {
		t.Fatalf("no recorded run of a scenario in %s", dir)
	}
	writeSummary(t, dir, outs)
}

func writeSummary(t *testing.T, out string, outs []outcome) {
	if len(outs) == 0 {
		return
	}
	b, _ := json.MarshalIndent(outs, "", "  ")
	os.WriteFile(filepath.Join(out, "summary.json"), b, 0o644)
	var md strings.Builder
	md.WriteString("| Scenario | Verdict | Time | Failed or known |\n|---|---|---|---|\n")
	for _, o := range outs {
		notes := append(append([]string{}, o.Failures...), o.Known...)
		if o.Error != "" {
			notes = append([]string{o.Error}, notes...)
		}
		fmt.Fprintf(&md, "| %s | %s | %s | %s |\n", o.Name, o.Verdict,
			(time.Duration(o.DurationS) * time.Second).String(), strings.ReplaceAll(strings.Join(notes, "; "), "|", "/"))
	}
	os.WriteFile(filepath.Join(out, "summary.md"), []byte(md.String()), 0o644)
	t.Logf("summary:\n%s", md.String())
}
