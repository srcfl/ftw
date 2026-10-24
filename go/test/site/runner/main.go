// siterig-runner runs FTW's site test (go/test/site) on a machine of its own
// and publishes the result as a GitHub commit status on the exact commit:
// siterig/full on each new master commit, siterig/short on the head of each
// open pull request that touches control, loadpoint, OCPP or telemetry code,
// and siterig/random for a night's random seeds. native-release.yml reads
// siterig/full before it publishes a beta. See README.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	var cfg config
	flag.StringVar(&cfg.Repo, "repo", "srcfl/ftw", "GitHub repository")
	work := flag.String("work", filepath.Join(os.Getenv("HOME"), "siterig"), "clone, worktrees and results")
	every := flag.Duration("every", 5*time.Minute, "how often to look for new commits")
	lanes := flag.Int("lanes", 3, "scenarios run at once")
	portBase := flag.Int("port-base", 18700, "first port; lanes use 100 each")
	keep := flag.Duration("keep", 14*24*time.Hour, "how long to keep results")
	flag.BoolVar(&cfg.AllowForks, "allow-forks", false, "also run pull requests from forks (their code runs here)")
	flag.BoolVar(&cfg.Drafts, "drafts", true, "also run draft pull requests")
	flag.IntVar(&cfg.RandomAt, "random-at", 2, "local hour of the nightly random seeds; -1 never")
	flag.IntVar(&cfg.RandomSeeds, "random-seeds", 10, "random seeds a night")
	once := flag.Bool("once", false, "run what is due and exit")
	flag.Parse()

	r := &runner{cfg: cfg, gh: ghCLI{repo: cfg.Repo}, work: *work, lanes: *lanes, portBase: *portBase, keep: *keep}
	if err := r.prepare(); err != nil {
		log.Fatal(err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	for {
		r.tick()
		if *once {
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(*every):
		}
	}
}

type runner struct {
	cfg      config
	gh       github
	work     string
	lanes    int
	portBase int
	keep     time.Duration
}

func (r *runner) clone() string   { return filepath.Join(r.work, "ftw") }
func (r *runner) results() string { return filepath.Join(r.work, "results") }

// prepare clones the repository once.
func (r *runner) prepare() error {
	if err := os.MkdirAll(r.results(), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.clone(), ".git")); err == nil {
		return nil
	}
	return run(r.work, "git", "clone", "--quiet", "https://github.com/"+r.cfg.Repo, r.clone())
}

// tick runs everything that is due, one job at a time.
func (r *runner) tick() {
	stateFile := filepath.Join(r.work, "last-random")
	last, _ := os.ReadFile(stateFile)
	jobs, err := plan(r.gh, r.cfg, time.Now(), strings.TrimSpace(string(last)))
	if err != nil {
		log.Printf("plan: %v", err)
		return
	}
	for _, j := range jobs {
		log.Printf("start %s", j)
		state, desc := r.runJob(j)
		log.Printf("done %s: %s, %s", j, state, desc)
		if err := r.gh.SetStatus(j.SHA, j.Context, state, desc); err != nil {
			log.Printf("status: %v", err)
		}
		if j.Context == ctxRandom {
			os.WriteFile(stateFile, []byte(time.Now().Format("2006-01-02")), 0o644)
		}
	}
	r.prune()
}

// runJob checks out the commit in a worktree of its own and runs the rig.
func (r *runner) runJob(j job) (state, desc string) {
	if err := r.gh.SetStatus(j.SHA, j.Context, "pending", "running on "+hostname()); err != nil {
		log.Printf("status: %v", err)
	}
	stamp := time.Now().Format("20060102-150405")
	out := filepath.Join(r.results(), fmt.Sprintf("%s-%s-%s", stamp, short(j.SHA), strings.TrimPrefix(j.Context, "siterig/")))
	wt := filepath.Join(r.work, "wt-"+short(j.SHA)+"-"+stamp)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "error", err.Error()
	}
	defer func() {
		run(r.clone(), "git", "worktree", "remove", "--force", wt)
		run(r.clone(), "git", "worktree", "prune")
	}()
	fetch := []string{"fetch", "--quiet", "origin", "master"}
	if j.PR > 0 {
		fetch = append(fetch, fmt.Sprintf("refs/pull/%d/head", j.PR))
	}
	if err := run(r.clone(), "git", fetch...); err != nil {
		return "error", "git fetch failed"
	}
	if err := run(r.clone(), "git", "worktree", "add", "--detach", wt, j.SHA); err != nil {
		return "error", "commit not found"
	}
	log1, _ := os.Create(filepath.Join(out, "rig.log"))
	defer log1.Close()
	mk := exec.Command("make", "drivers")
	mk.Dir, mk.Stdout, mk.Stderr = wt, log1, log1
	if err := mk.Run(); err != nil {
		return "error", "make drivers failed"
	}
	t0 := time.Now()
	test := exec.Command("go", "test", "-count=1", "-timeout", "300m", "-run", "TestSite$", "-v", "./test/site/")
	test.Dir, test.Stdout, test.Stderr = filepath.Join(wt, "go"), log1, log1
	test.Env = append(os.Environ(), "FTW_SITE="+j.Set, fmt.Sprintf("FTW_SITE_LANES=%d", r.lanes),
		fmt.Sprintf("FTW_SITE_PORT_BASE=%d", r.portBase), "FTW_SITE_OUT="+out)
	test.Run() // the summary, not the exit code, says what passed
	summary, _ := os.ReadFile(filepath.Join(out, "summary.json"))
	state, desc = describe(summary, time.Since(t0))
	if state == "success" && test.ProcessState != nil && !test.ProcessState.Success() {
		state, desc = "failure", "the rig failed outside the scenarios: "+desc
	}
	return state, desc
}

// prune drops results older than keep.
func (r *runner) prune() {
	entries, _ := os.ReadDir(r.results())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > r.keep {
			os.RemoveAll(filepath.Join(r.results(), e.Name()))
		}
	}
}

func run(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// ghCLI asks GitHub through the gh CLI, as the machine's logged-in user.
type ghCLI struct{ repo string }

func (g ghCLI) api(out any, args ...string) error {
	b, err := exec.Command("gh", append([]string{"api"}, args...)...).Output()
	if err != nil {
		return fmt.Errorf("gh api %s: %w", strings.Join(args, " "), err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (g ghCLI) MasterSHA() (string, error) {
	var c struct {
		SHA string `json:"sha"`
	}
	err := g.api(&c, "repos/"+g.repo+"/commits/master")
	return c.SHA, err
}

func (g ghCLI) Pulls() ([]pull, error) {
	var ps []pull
	err := g.api(&ps, "repos/"+g.repo+"/pulls?state=open&per_page=100")
	return ps, err
}

func (g ghCLI) Files(pr int) ([]string, error) {
	var fs []struct {
		Filename string `json:"filename"`
	}
	if err := g.api(&fs, fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", g.repo, pr)); err != nil {
		return nil, err
	}
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Filename
	}
	return out, nil
}

func (g ghCLI) Contexts(sha string) (map[string]bool, error) {
	var ss []struct {
		Context string `json:"context"`
	}
	if err := g.api(&ss, "repos/"+g.repo+"/commits/"+sha+"/statuses?per_page=100"); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, s := range ss {
		have[s.Context] = true
	}
	return have, nil
}

func (g ghCLI) SetStatus(sha, context, state, desc string) error {
	return g.api(nil, "-X", "POST", "repos/"+g.repo+"/statuses/"+sha,
		"-f", "state="+state, "-f", "context="+context, "-f", "description="+desc)
}
