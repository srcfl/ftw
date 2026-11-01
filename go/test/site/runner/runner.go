package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status contexts the runner publishes on a commit.
const (
	ctxShort  = "siterig/short"
	ctxFull   = "siterig/full"
	ctxRandom = "siterig/random"
)

// watchedPaths are the parts of the tree whose changes get the short set on
// a pull request: the control packages, every internal package they import,
// and the rig itself. TestWatchedPathsCoverImports keeps the imports in.
var watchedPaths = []string{
	"go/cmd/ftw/", // control tick wiring
	"go/internal/control/", "go/internal/loadpoint/", "go/internal/ocpp/", "go/internal/telemetry/",
	"go/internal/drivers/", "go/internal/mdnsresolve/", // driver host, command deadlines
	"go/internal/mpc/", "go/internal/optimizercontract/", // plans
	"go/internal/gridcost/", "go/internal/forecasting/", "go/internal/sunpos/", // plan inputs
	"go/internal/config/", "go/internal/configreload/", // limits
	"go/internal/units/",     // charge steps
	"go/internal/localzone/", // household clock: goal deadlines, import hour
	"go/internal/cars/", "go/internal/sitepower/",
	"go/internal/events/", "go/internal/state/",
	"drivers/", "optimizer/native/bundle/", // bundled drivers and solver
	"go/test/site/",
}

// controlPackages are the watched packages whose imports must be watched
// too. go/cmd/ftw is left out: it imports every package, the API and UI
// among them.
var controlPackages = []string{
	"control", "loadpoint", "ocpp", "telemetry", "drivers", "mpc",
	"optimizercontract", "config", "configreload", "cars", "sitepower",
}

// job is one run of the rig on one commit.
type job struct {
	Context string // which status it publishes
	SHA     string
	Set     string // FTW_SITE: short, full or random:<first seed>:<count>
	PR      int    // 0 for master
}

func (j job) String() string {
	if j.PR > 0 {
		return fmt.Sprintf("%s on #%d %s", j.Context, j.PR, short(j.SHA))
	}
	return fmt.Sprintf("%s on master %s", j.Context, short(j.SHA))
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// pull is the part of a pull request the runner needs.
type pull struct {
	Number    int    `json:"number"`
	Draft     bool   `json:"draft"`
	UpdatedAt string `json:"updated_at"` // RFC 3339, so it sorts as text
	Head      struct {
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// github is what the runner asks GitHub; gh api serves it in production.
type github interface {
	MasterSHA() (string, error)
	Pulls() ([]pull, error)
	Files(pr int) ([]string, error)
	Contexts(sha string) (map[string]bool, error) // status contexts already on a commit
	SetStatus(sha, context, state, description string) error
}

// config is the runner's flags.
type config struct {
	Repo        string
	AllowForks  bool // a fork's code runs on this machine; off by default
	Drafts      bool
	RandomAt    int // local hour of the nightly random run; -1 never
	RandomSeeds int
}

// plan lists what to run now, master first. A commit that already carries
// the context, in any state, is not run again. Random seeds run once a day
// (lastRandom is the day they last ran) on whatever master is then.
func plan(gh github, cfg config, now time.Time, lastRandom string) ([]job, error) {
	var jobs []job
	master, err := gh.MasterSHA()
	if err != nil {
		return nil, err
	}
	have, err := gh.Contexts(master)
	if err != nil {
		return nil, err
	}
	if !have[ctxFull] {
		jobs = append(jobs, job{Context: ctxFull, SHA: master, Set: "full"})
	}
	pulls, err := gh.Pulls()
	if err != nil {
		return nil, err
	}
	// Most recently updated first: that is the work in review, and a pull
	// request untouched for weeks can wait.
	sort.SliceStable(pulls, func(i, j int) bool { return pulls[i].UpdatedAt > pulls[j].UpdatedAt })
	for _, p := range pulls {
		if p.Draft && !cfg.Drafts {
			continue
		}
		if p.Head.Repo.FullName != cfg.Repo && !cfg.AllowForks {
			continue
		}
		have, err := gh.Contexts(p.Head.SHA)
		if err != nil {
			return nil, err
		}
		if have[ctxShort] {
			continue
		}
		files, err := gh.Files(p.Number)
		if err != nil {
			return nil, err
		}
		if touchesWatched(files) {
			jobs = append(jobs, job{Context: ctxShort, SHA: p.Head.SHA, Set: "short", PR: p.Number})
		}
	}
	day := now.Format("2006-01-02")
	if cfg.RandomAt >= 0 && cfg.RandomSeeds > 0 && now.Hour() >= cfg.RandomAt && lastRandom != day {
		// One seed range per day, so each night tries new timelines.
		first := int(now.Unix()/86400) * cfg.RandomSeeds
		jobs = append(jobs, job{Context: ctxRandom, SHA: master, Set: fmt.Sprintf("random:%d:%d", first, cfg.RandomSeeds)})
	}
	return jobs, nil
}

// next is the first job not yet run in this tick. A job whose status failed
// to post is planned again; this keeps it from running twice.
func next(jobs []job, ran map[string]bool) (job, bool) {
	for _, j := range jobs {
		if !ran[j.Context+" "+j.SHA] {
			return j, true
		}
	}
	return job{}, false
}

func touchesWatched(files []string) bool {
	for _, f := range files {
		for _, p := range watchedPaths {
			if strings.HasPrefix(f, p) {
				return true
			}
		}
	}
	return false
}

// outcome is one line of the rig's summary.json.
type outcome struct {
	Name      string  `json:"name"`
	Verdict   string  `json:"verdict"` // pass, known, fail
	DurationS float64 `json:"duration_s"`
	Error     string  `json:"error"`
}

// describe turns summary.json into a status: success when nothing failed,
// with counts that fit GitHub's 140 characters.
func describe(summary []byte, took time.Duration) (state, desc string) {
	var outs []outcome
	if err := json.Unmarshal(summary, &outs); err != nil || len(outs) == 0 {
		return "error", "no summary: the rig did not finish (see the runner's log)"
	}
	n := map[string]int{}
	var failed []string
	for _, o := range outs {
		n[o.Verdict]++
		if o.Verdict == "fail" {
			failed = append(failed, o.Name)
		}
	}
	state = "success"
	if len(failed) > 0 {
		state = "failure"
	}
	desc = fmt.Sprintf("%d pass, %d known, %d fail of %d in %s", n["pass"], n["known"], n["fail"], len(outs), took.Round(time.Minute))
	if len(failed) > 0 {
		desc += ": " + strings.Join(failed, " ")
	}
	if len(desc) > 140 {
		desc = desc[:137] + "..."
	}
	return state, desc
}
