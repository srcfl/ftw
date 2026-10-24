package main

import (
	"context"
	"log/slog"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/fleetping"
	"github.com/srcfl/ftw/go/internal/selfupdate"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/statsrelay"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The anonymous fleet ping, wired to the box.
//
// Everything here is adapters. What may leave the house is decided in
// go/internal/fleetping, in one function, so that this file cannot widen the
// message by accident.

// firstSeenKey records when this box first ran a version that counts install
// age. It never leaves the box in this form — only as a bucket a few months
// wide.
const firstSeenKey = "install.first_seen_ms"

// firstSeen is when this box first ran FTW, as far as it can tell.
//
// A box upgrading into this version starts its clock now, so an install that
// has been running for two years reports "0-1m" for its first month. The
// alternative — digging the oldest surviving history row out of SQLite — is a
// query that would have to stay correct forever to improve one cohort's number
// once, and cold storage rolls off anyway, so the oldest row is not the install
// date either.
func firstSeen(st *state.Store, now time.Time) time.Time {
	if st == nil {
		return time.Time{}
	}
	if v, ok := st.LoadConfig(firstSeenKey); ok {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			return time.UnixMilli(ms)
		}
	}
	if err := st.SaveConfig(firstSeenKey, strconv.FormatInt(now.UnixMilli(), 10)); err != nil {
		slog.Debug("could not record first-seen time", "err", err)
		return time.Time{}
	}
	return now
}

// startFleetPing builds the pinger and starts its schedule.
//
// It is constructed even when the ping is switched off, because Settings
// renders the payload this box would send and that screen has to work whether
// or not anything is being sent — being able to see the message while it is
// off is most of the point.
func startFleetPing(
	ctx context.Context,
	cfg *config.Config,
	cfgMu *sync.RWMutex,
	st *state.Store,
	answer statsAnswer,
	tel *telemetry.Store,
	updates *selfupdate.Checker,
	version string,
	managedDriverDir string,
) (*fleetping.Pinger, error) {
	cfgMu.RLock()
	endpoint := cfg.FleetPing.Resolved()
	cfgMu.RUnlock()

	provider, err := fleetProvider(endpoint, statsrelay.Pinned())
	if err != nil {
		return nil, err
	}

	installedAt := firstSeen(st, time.Now())

	pinger, err := fleetping.New(fleetping.Options{
		Enabled: fleetReportAllowed(answer, endpoint, provider),
		Facts: func() fleetping.Facts {
			// Drivers can be installed without restarting FTW. Read the signed
			// catalogue at the same time as the config so tomorrow's report and
			// the Settings preview both see the install.
			catalogue := fleetCatalogue(st, managedDriverDir)
			health := fleetHealth(st, tel, updates, time.Now())
			cfgMu.RLock()
			defer cfgMu.RUnlock()
			f := fleetping.FactsFromConfig(cfg, version, catalogue, installedAt)
			f.Health = health
			return f
		},
		Provider: provider,
	})
	if err != nil {
		return nil, err
	}

	go pinger.Run(ctx)
	return pinger, nil
}

// fleetHealth reads how the box ran lately, for the report's health buckets:
// the planner's last 24 hours from the plans it kept, the drivers' last 24
// hours of polls once the count covers six of them, and the newest update of
// the last 7 days. A source that is missing or too short, as the updater is
// on a box that does not update itself, reads as nothing to report.
func fleetHealth(st *state.Store, tel *telemetry.Store, updates *selfupdate.Checker, now time.Time) fleetping.Health {
	var h fleetping.Health
	if st != nil {
		plans, slowestMs, fallbacks, err := st.PlannerDay(now.Add(-24 * time.Hour).UnixMilli())
		if err != nil {
			slog.Debug("fleet report: planner day unavailable", "err", err)
		} else {
			h.Plans, h.SlowestSolve, h.Fallbacks = plans, time.Duration(slowestMs*float64(time.Millisecond)), fallbacks
		}
	}
	// Counts that cover under six hours, as after an update's restart, say
	// too little: a start-up burst of failed polls would read as trouble.
	if tel != nil {
		if polls, errors, covered := tel.PollsLastDay(now); covered >= 6*time.Hour {
			h.Polls, h.PollErrors = polls, errors
		}
	}
	if updates != nil {
		u := updates.Status()
		if u.Action == "update" && !u.UpdatedAt.IsZero() && now.Sub(u.UpdatedAt) <= 7*24*time.Hour {
			switch u.State {
			case "done":
				h.Update = "ok"
			case "failed":
				h.Update = "failed"
			}
		}
	}
	return h
}

// fleetProvider sends the report to FTW Cloud through the relay when this
// build names one, and only then: FTW Cloud then never sees the box's
// address. A build without a relay posts straight to /v1/fleet as before. An
// endpoint the household chose is its own collector and is posted to
// directly.
func fleetProvider(endpoint string, relay *statsrelay.Client) (fleetping.Provider, error) {
	if relay != nil && strings.TrimSpace(endpoint) == config.DefaultFleetPingEndpoint {
		return fleetping.NewRelayProvider(relay), nil
	}
	return fleetping.NewHTTPProvider(endpoint, nil)
}

// statsAnswer is the one answer for both anonymous daily reports, the fleet
// report and the energy report: part of FTW Cloud, and on for a box without
// it until the owner turns it off. One reader, so the two reports cannot come
// to read different answers. It asks the running Cloud link, which counts a
// grant only with the key it was made with; with no link, the box's own
// answer alone. fleet_ping.enabled is no longer a second switch;
// migrateStatistics read it once.
type statsAnswer struct {
	st   *state.Store
	link *cloudlink.Link
}

func (a statsAnswer) shares() bool {
	if a.link != nil {
		return a.link.SharesStatistics()
	}
	return a.st != nil && cloudlink.SharesStatistics(a.st)
}

func (a statsAnswer) saidYes() bool {
	if a.link != nil {
		return a.link.SaidYesToStatistics()
	}
	return a.st != nil && cloudlink.SaidYesToStatistics(a.st)
}

// fleetReportAllowed is the one answer, with one more rule for the direct
// path (Fredrik, 24 October 2026): a box that shares only by default, never
// having said yes, posts nothing straight to FTW Cloud, which would see its
// address; it waits for the relay. Through the relay, or to a collector the
// household chose, the answer alone decides.
func fleetReportAllowed(answer statsAnswer, endpoint string, provider fleetping.Provider) func() bool {
	_, relayed := provider.(*fleetping.RelayProvider)
	direct := !relayed && strings.TrimSpace(endpoint) == config.DefaultFleetPingEndpoint
	return func() bool {
		return answer.shares() && (!direct || answer.saidYes())
	}
}

// migrateStatistics turns what an earlier release stored into the one
// answer, once: a fleet report switched off in the config stays off. Then
// the Cloud grant follows it, so an answer that came out off also stops an
// hourly upload the grant still carried.
func migrateStatistics(cfg *config.Config, cfgMu *sync.RWMutex, st *state.Store, link *cloudlink.Link) {
	if st == nil {
		return
	}
	if err := cloudlink.MigrateStatistics(st, !fleetPingEnabled(cfg, cfgMu)()); err != nil {
		slog.Warn("could not record the statistics answer; nothing is sent until it is", "err", err)
		return
	}
	if link != nil {
		if err := link.ReconcileStatistics("statistics answer"); err != nil {
			slog.Warn("could not make the FTW Cloud grant follow the statistics answer", "err", err)
		}
	}
}

// fleetPingEnabled reads the old switch under the config lock. Since the one
// statistics answer it is read once, by migrateStatistics.
//
// On() and not a second reading of nil. A config posted to /api/config never
// passes through applyDefaults and configreload.Apply swaps it in whole, so a
// client that saves a config with no fleet_ping object hands the box a nil
// section. Read here as "off", that would stop the ping the first time a
// household saved anything at all, and keep it off until the next reboot.
func fleetPingEnabled(cfg *config.Config, cfgMu *sync.RWMutex) func() bool {
	return func() bool {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		return cfg.FleetPing.On()
	}
}

// fleetCatalogue is where the set of "known" driver names comes from, and the
// point of it is to keep a household's own name for a driver out of that set.
//
// Two sources, and neither of them is "whatever is in this directory". A
// config field says where installed drivers are kept, so a directory is
// something the household can decide the contents of.
//
// The shipped catalogue is fleetping.ShippedCatalogue: generated from
// drivers/BUNDLED_SOURCE.json and compiled into the binary, so every box
// running this release has the same one and nothing on a running box can add
// to it. It used to be a scan of the directory the bundled drivers sit in,
// which was wrong: device_repository.root_dir is an ordinary config field, so
// pointing it inside that directory made every artifact the household had ever
// installed part of the shipped catalogue. A directory is something a config
// can point at; a generated constant is not.
//
// The driver-repository active dir is the other, and its listing decides
// nothing on its own. It holds one file per driver somebody installed, under
// whatever name the manifest that installed it gave, so every filename found
// there still has to answer: does this box's install history hold a record for
// that name saying it arrived under a signature made with FTW's own key? That
// is why root_dir may point anywhere at all — the listing only says which
// install records still have a file behind them, and an install record is not
// something a config writes.
//
// Only the name is matched here. That the file is still the artifact the
// record describes is settled before this runs, by driverrepo.reconcileActive:
// it drops any active entry that is not a symlink into the installed tree
// hashing to the recorded digest, and deactivates the record with it. So a
// household file left under an FTW-signed name is gone from both the directory
// and the history by the time the catalogue is read — which is worth knowing,
// because reading this function alone would suggest a name is all that is
// checked.
//
// The question is deliberately not asked of the config. The config is the
// household's to write, and it can be rewritten after the fact: an entry can
// claim a repository id the binary pins, be switched off, or be deleted
// outright while the file it installed stays where it is. Any of those turns
// "which repositories require a signature today" into an answer the household
// chose. What happened at install time is not theirs to rewrite, so that is
// what is read here — the flag driverrepo wrote on the install row.
//
// Anything with no such record reports as "other": a file copied into the
// active dir by hand, one renamed after it was installed, one from a
// repository of the household's own, and one installed before FTW started
// recording. That costs the box a name and nothing else.
func fleetCatalogue(st *state.Store, managedDir string) []string {
	names := fleetping.ShippedCatalogue()
	if managedDir == "" {
		return names
	}
	vouched := ftwSignedRepoFiles(st)
	installed := make([]string, 0, len(vouched))
	for _, name := range catalogueFilenames(managedDir) {
		if vouched[name] {
			installed = append(installed, name)
		}
	}
	return append(names, fleetping.CatalogueNames(installed)...)
}

// catalogueFilenames lists the driver files in one directory. Only ever the
// driver-repository active dir, and only ever to intersect with the install
// history — a name that comes back from here still has to be vouched for.
func catalogueFilenames(dir string) []string {
	entries, err := drivers.LoadCatalogMulti(dir)
	if err != nil {
		// A dir that would not read means every driver installed from a
		// repository reports as "other". That is a worse answer for the owner
		// and a safe one for the household, which is the right way round.
		slog.Debug("fleet ping could not read the driver catalogue", "err", err)
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Filename)
	}
	return names
}

// ftwSignedRepoFiles is the set of filenames the install history says arrived
// under FTW's own signature, keyed the way the catalogue names them —
// "easee_cloud.lua".
//
// The name is taken from the logical path the installer recorded, not from the
// directory listing, so a file renamed on disk after it was installed no
// longer matches anything here.
func ftwSignedRepoFiles(st *state.Store) map[string]bool {
	if st == nil {
		return nil
	}
	installs, err := st.ActiveDriverRepoInstalls()
	if err != nil {
		slog.Debug("fleet ping could not read driver provenance", "err", err)
		return nil
	}
	out := make(map[string]bool, len(installs))
	for _, in := range installs {
		if in.FTWSigned {
			out[path.Base(filepath.ToSlash(in.LogicalPath))] = true
		}
	}
	return out
}
