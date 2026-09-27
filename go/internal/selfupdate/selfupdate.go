// Package selfupdate resolves the stable and beta 0.x releases a native
// install can take, and stages a verified release package in its slots
// (ADR 0007).
//
// GitHub Releases identifies the stable and beta targets. We cannot use raw
// semver over every tag because the tag history isn't monotonic: the older
// 1.x, 2.x and 3.x Docker lines outrank the native v0.X.Y line numerically.
// A release is deployable only once it carries this host's package and its
// checksum, because assets upload after the release is published.
//
// The check is probe-only: nothing changes on the host until the owner asks
// for an update, rollback or restart through the ftw command or the API. See
// docs/self-update.md.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/nativeupdate"
)

// Store is the subset of state.Store methods this package needs. Declared as
// an interface so tests don't need a real SQLite DB.
type Store interface {
	SaveConfig(key, value string) error
	LoadConfig(key string) (string, bool)
}

const (
	skippedKey           = "update.skipped_version"
	channelKey           = "update.channel"
	lastRunKey           = "update.last_run_version"
	defaultCheckInterval = 1 * time.Hour
	defaultHTTPTimeout   = 10 * time.Second
	// staleThreshold flags an in-flight update as failed when its status
	// file hasn't been refreshed within this window, so a run that died
	// does not hold the ftw command or the UI forever.
	staleThreshold = 5 * time.Minute
)

// Channel controls which immutable release stream the checker follows.
// Stable remains the default; beta is an explicit operator opt-in.
type Channel string

const (
	ChannelStable Channel = "stable"
	ChannelBeta   Channel = "beta"
)

var availableChannels = []Channel{ChannelStable, ChannelBeta}

func ParseChannel(v string) (Channel, error) {
	channel := Channel(strings.ToLower(strings.TrimSpace(v)))
	switch channel {
	case ChannelStable, ChannelBeta:
		return channel, nil
	default:
		return "", fmt.Errorf("selfupdate: invalid channel %q", v)
	}
}

// Config configures the Checker.
type Config struct {
	// Repo is the GitHub "owner/name" slug. Defaults to srcfl/ftw.
	Repo string

	// CurrentVersion is the running binary's version (from main.Version).
	CurrentVersion string
	// CurrentStateSchema is the running Core's on-disk state format. Core
	// releases publish the target value in their release notes. A missing or
	// different target keeps the full pre-update backup fail-closed.
	CurrentStateSchema int
	// CheckInterval is the probe cadence. 0 = 1 h.
	CheckInterval time.Duration
	// StatusPath is the update status file. Empty disables Status.
	StatusPath string
	// NativeRoot holds the verified binary release slots. NativeRestart must
	// stop Core gracefully after a candidate has been staged.
	NativeRoot       string
	NativeRestart    func() error
	NativeReleaseURL string // test override; empty uses the public GitHub release URL
	// NativeTrialTimeout is how long a new native Core may take to become
	// ready. The next Core writes no status before it is ready, so a native
	// "restarting" status is not stale until this much time has passed.
	NativeTrialTimeout time.Duration
	// Bus receives an events.UpdateAvailable event whenever Check
	// discovers a new, non-skipped release tag. Nil disables emission.
	Bus *events.Bus

	// ReleasesURL lists published releases. GitHub's /releases/latest stays
	// on the old 2.x line, so both channels read the list. Defaults to the
	// public api.github.com endpoint for Repo; overridable for tests.
	ReleasesURL string

	// Overrides for tests.
	HTTPClient *http.Client
	Now        func() time.Time
}

// Info is the cached view returned to the UI.
type Info struct {
	Current         string    `json:"current"`
	Native          bool      `json:"native,omitempty"`
	Previous        string    `json:"previous,omitempty"`
	Channel         Channel   `json:"channel"`
	Channels        []Channel `json:"channels"`
	Latest          string    `json:"latest,omitempty"`
	PublishedAt     time.Time `json:"published_at,omitempty"`
	ReleaseNotesURL string    `json:"release_notes_url,omitempty"`
	// ReleaseBody is the markdown body of the GitHub release —
	// typically the auto-generated changelog section (Features, Bug
	// Fixes). The UI renders this inline in the update modal so
	// operators can read what's about to be applied without opening
	// a new tab. Capped at MaxReleaseBodyBytes to keep a pathological
	// release note from ballooning the Info payload.
	ReleaseBody        string    `json:"release_body,omitempty"`
	CurrentStateSchema int       `json:"current_state_schema,omitempty"`
	TargetStateSchema  int       `json:"target_state_schema,omitempty"`
	FullBackupRequired bool      `json:"full_backup_required"`
	CheckedAt          time.Time `json:"checked_at,omitempty"`
	UpdateAvailable    bool      `json:"update_available"`
	Skipped            bool      `json:"skipped"`
	SkippedVersion     string    `json:"skipped_version,omitempty"`
	Err                string    `json:"err,omitempty"`
	// InstallReady is true when the release slots under InstallRoot are
	// readable and hold the running release, so an update can be staged.
	InstallReady bool `json:"install_ready"`
	// InstallRoot holds the native release slots. InstallFreeBytes is the
	// space left there and InstallNeedBytes what the next download needs.
	InstallRoot      string `json:"install_root,omitempty"`
	InstallFreeBytes int64  `json:"install_free_bytes,omitempty"`
	InstallNeedBytes int64  `json:"install_need_bytes,omitempty"`
	// LastFailed is a release that failed on this box: its trial did not
	// become ready, or it kept stopping during probation.
	LastFailed string `json:"last_failed,omitempty"`
}

// MaxReleaseBodyBytes caps the persisted release body. 16 KiB covers a
// few dozen bullets from semantic-release comfortably; anything larger
// is truncated with a trailing marker and the operator keeps the
// ReleaseNotesURL link for the full thing.
const MaxReleaseBodyBytes = 16 * 1024

// UpdateStatus is the saved state of the last update, rollback or restart.
// It lives in a file beside the release slots, so the Core that starts next
// can finish the record and a polling client sees every transition.
type UpdateStatus struct {
	State           string    `json:"state"` // idle, starting, pulling, checking, restarting, done, failed
	Action          string    `json:"action,omitempty"`
	Component       string    `json:"component,omitempty"`
	Target          string    `json:"target,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	PhaseStartedAt  time.Time `json:"phase_started_at,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
	Message         string    `json:"message,omitempty"`
	Step            int       `json:"step,omitempty"`
	TotalSteps      int       `json:"total_steps,omitempty"`
	ProgressCurrent int64     `json:"progress_current,omitempty"`
	ProgressTotal   int64     `json:"progress_total,omitempty"`
	ProgressUnit    string    `json:"progress_unit,omitempty"`
	// Phases lists the finished phases of this run with Core's own timing,
	// so a client that polls slowly still sees every phase.
	Phases []PhaseRecord `json:"phases,omitempty"`
}

// PhaseRecord is one finished phase of an update or rollback.
type PhaseRecord struct {
	Step       int       `json:"step,omitempty"`
	TotalSteps int       `json:"total_steps,omitempty"`
	Message    string    `json:"message"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Bytes      int64     `json:"bytes,omitempty"`
}

// EndPhase records the current phase as finished at now.
func (st *UpdateStatus) EndPhase(now time.Time) {
	record := PhaseRecord{Step: st.Step, TotalSteps: st.TotalSteps, Message: st.Message,
		StartedAt: st.PhaseStartedAt, FinishedAt: now}
	if st.ProgressUnit == "bytes" {
		record.Bytes = st.ProgressCurrent
	}
	st.Phases = append(st.Phases, record)
}

// Checker is the background version-check service.
type Checker struct {
	cfg   Config
	store Store

	mu               sync.RWMutex
	info             Info
	lastAnnouncedTag string // dedupe: last tag we emitted UpdateAvailable for
}

// New constructs a Checker but does not start the background loop.
// Call Start(ctx) once wiring is complete.
func New(cfg Config, store Store) *Checker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = defaultCheckInterval
	}
	if cfg.Repo == "" {
		cfg.Repo = "srcfl/ftw"
	}
	if cfg.ReleasesURL == "" {
		cfg.ReleasesURL = "https://api.github.com/repos/" + cfg.Repo + "/releases?per_page=100"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	channel := inferChannel(cfg.CurrentVersion)
	if store != nil {
		if persisted, ok := store.LoadConfig(channelKey); ok && persisted != "" {
			if persisted == "edge" {
				channel = ChannelBeta
				_ = store.SaveConfig(channelKey, string(ChannelBeta))
			} else if parsed, err := ParseChannel(persisted); err == nil {
				channel = parsed
			}
		}
	}
	c := &Checker{cfg: cfg, store: store}
	c.info.Current = cfg.CurrentVersion
	c.info.CurrentStateSchema = cfg.CurrentStateSchema
	c.info.FullBackupRequired = true
	c.info.Channel = channel
	c.info.Channels = append([]Channel(nil), availableChannels...)
	c.mu.Lock()
	c.reloadSkipLocked()
	c.mu.Unlock()
	return c
}

// Start launches a goroutine that probes at CheckInterval until ctx is
// cancelled. The first probe runs after a 5–30 s random delay so restart
// bursts don't all hit GitHub at the same instant.
func (c *Checker) Start(ctx context.Context) {
	c.announceInstalled()
	go c.loop(ctx)
}

// announceInstalled settles the one question only a fresh boot can answer:
// whether this process is the first run of a new version. The previous run
// wrote its version under lastRunKey; reading a different one back means the
// update installed and survived, and events.UpdateInstalled says so — once,
// because the key is rewritten before the announcement.
func (c *Checker) announceInstalled() {
	cur := c.cfg.CurrentVersion
	if c.store == nil || cur == "" {
		return
	}
	prev, _ := c.store.LoadConfig(lastRunKey)
	if prev == cur {
		return
	}
	if err := c.store.SaveConfig(lastRunKey, cur); err != nil {
		// Fail closed on the announcement too: a version this could not
		// record would be re-announced on every boot, and a repeated
		// "your box updated itself" teaches the operator to ignore it.
		slog.Warn("selfupdate: could not record the running version", "err", err)
		return
	}
	if c.cfg.Bus == nil || prev == "" || prev == "dev" || cur == "dev" {
		// A first boot has nothing to have updated from, and a dev binary
		// changes identity every build without ever installing anything.
		return
	}
	c.cfg.Bus.Publish(events.UpdateInstalled{
		Version:         cur,
		PreviousVersion: prev,
		At:              c.cfg.Now(),
	})
}

func (c *Checker) loop(ctx context.Context) {
	// Jitter the boot probe so many instances upgrading at once don't
	// synchronize. The jitter is coarse (seconds), not security-sensitive.
	delay := time.Duration(5+time.Now().Unix()%25) * time.Second
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return
	}
	if _, err := c.Check(ctx, false); err != nil {
		slog.Warn("selfupdate: initial check failed", "err", err)
	}
	t := time.NewTicker(c.cfg.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := c.Check(ctx, false); err != nil {
				slog.Warn("selfupdate: periodic check failed", "err", err)
			}
		}
	}
}

// Check asks GitHub Releases for the newest 0.x release on the selected
// channel, confirms it carries this host's package and checksum, and (if
// newer than current) flips UpdateAvailable. A non-force call that finds
// the cache younger than half the check interval returns early and never
// hits the network.
func (c *Checker) Check(ctx context.Context, force bool) (Info, error) {
	cached := c.Info()
	// A recorded GitHub 5xx must not occupy the half-interval cache.
	// That is the "restart seemed to fix it" bug: the UI reads this
	// cache for hours, and only a force check or a process start retries.
	if !force && cached.Err == "" && !cached.CheckedAt.IsZero() && c.cfg.Now().Sub(cached.CheckedAt) < c.cfg.CheckInterval/2 {
		return cached, nil
	}

	channel := cached.Channel
	rel, err := c.resolveChannel(ctx, channel)
	if err != nil {
		return c.recordErr(err)
	}
	targetTag := rel.TagName
	deployable := targetTag != "" && hasNativeAssets(rel, runtime.GOARCH)

	c.mu.Lock()
	// A channel switch may complete while an older network probe is in
	// flight. Never let that stale result overwrite the newly selected stream.
	if c.info.Channel != channel {
		info := c.info
		c.mu.Unlock()
		return info, nil
	}
	if deployable {
		targetStateSchema := releaseStateSchema(rel.Body)
		c.info.Latest = targetTag
		c.info.PublishedAt = rel.PublishedAt
		c.info.ReleaseNotesURL = rel.HtmlURL
		c.info.ReleaseBody = truncateBody(releaseBodyWithoutStateSchema(rel.Body))
		c.info.TargetStateSchema = targetStateSchema
		c.info.FullBackupRequired = c.cfg.CurrentStateSchema <= 0 ||
			targetStateSchema <= 0 ||
			targetStateSchema != c.cfg.CurrentStateSchema
		c.info.UpdateAvailable = channelUpdateAvailable(targetTag, c.info.Current)
	} else {
		// Either GH has no published release yet, or its package has not
		// finished uploading. Keep the prior Latest visible (so the UI
		// doesn't flicker) but don't offer it.
		c.info.UpdateAvailable = false
	}
	c.info.CheckedAt = c.cfg.Now()
	c.info.Err = ""
	c.reloadSkipLocked()
	c.refreshRuntimeInfoLocked()
	// Decide whether to emit under the lock, then publish outside it.
	var announce *events.UpdateAvailable
	if c.cfg.Bus != nil && c.info.UpdateAvailable && !c.info.Skipped &&
		c.info.Latest != "" && c.info.Latest != c.lastAnnouncedTag {
		c.lastAnnouncedTag = c.info.Latest
		announce = &events.UpdateAvailable{
			Version:         c.info.Latest,
			PreviousVersion: c.info.Current,
			ReleaseNotesURL: c.info.ReleaseNotesURL,
			PublishedAt:     c.info.PublishedAt,
			At:              c.cfg.Now(),
		}
	}
	info := c.info
	c.mu.Unlock()
	if announce != nil {
		c.cfg.Bus.Publish(*announce)
	}
	return info, nil
}

// resolveChannel selects the newest 0.x release on channel. Beta includes
// stable releases, so beta testers converge back to a promoted stable build
// instead of remaining pinned to the final prerelease.
func (c *Checker) resolveChannel(ctx context.Context, channel Channel) (ghRelease, error) {
	return c.fetchReleaseList(ctx, func(rel ghRelease) bool {
		if !strings.HasPrefix(rel.TagName, "v0.") {
			return false
		}
		if channel == ChannelStable {
			return !rel.Prerelease && isStableTag(rel.TagName)
		}
		return !rel.Prerelease && isStableTag(rel.TagName) || rel.Prerelease && isBetaTag(rel.TagName)
	})
}

func hasNativeAssets(rel ghRelease, arch string) bool {
	archive := "ftw-linux-" + arch + ".tar.gz"
	var foundArchive, foundChecksum bool
	for _, asset := range rel.Assets {
		if asset.Name == archive {
			foundArchive = true
		}
		if asset.Name == archive+".sha256" {
			foundChecksum = true
		}
	}
	return foundArchive && foundChecksum
}

func (c *Checker) recordErr(err error) (Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.info.Err = err.Error()
	c.info.CheckedAt = c.cfg.Now()
	c.refreshRuntimeInfoLocked()
	return c.info, err
}

const githubRetryLimit = 5

// githubRetryBackoff waits between GitHub 5xx/429 attempts. Tests replace it.
var githubRetryBackoff = time.Sleep

func githubTransientStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func (c *Checker) getGitHub(ctx context.Context, rawURL string) (*http.Response, error) {
	var last error
	for attempt := 1; attempt <= githubRetryLimit; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "FTW-selfupdate")
		resp, err := c.cfg.HTTPClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			last = err
		} else if !githubTransientStatus(resp.StatusCode) {
			return resp, nil
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
			last = fmt.Errorf("github releases %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if attempt < githubRetryLimit {
			githubRetryBackoff(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	if last == nil {
		last = errors.New("github releases: retries exhausted")
	}
	return nil, last
}

// ghRelease mirrors the subset of fields we read from the GitHub
// Releases API.
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	HtmlURL     string    `json:"html_url"`
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Assets      []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func (c *Checker) fetchReleaseList(ctx context.Context, accept func(ghRelease) bool) (ghRelease, error) {
	rawURL := c.cfg.ReleasesURL
	seen := make(map[string]bool)
	for rawURL != "" {
		if seen[rawURL] {
			return ghRelease{}, errors.New("github releases list: repeated page")
		}
		seen[rawURL] = true
		resp, err := c.getGitHub(ctx, rawURL)
		if err != nil {
			return ghRelease{}, err
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
			return ghRelease{}, fmt.Errorf("github releases list %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var releases []ghRelease
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return ghRelease{}, decodeErr
		}
		for _, rel := range releases {
			if !rel.Draft && accept(rel) {
				return rel, nil
			}
		}
		rawURL, err = nextReleasePage(resp.Header.Get("Link"), rawURL)
		if err != nil {
			return ghRelease{}, err
		}
	}
	return ghRelease{}, nil
}

func nextReleasePage(linkHeader, currentURL string) (string, error) {
	current, err := url.Parse(currentURL)
	if err != nil {
		return "", err
	}
	for _, link := range strings.Split(linkHeader, ",") {
		parts := strings.Split(link, ";")
		if len(parts) < 2 || strings.TrimSpace(parts[1]) != `rel="next"` {
			continue
		}
		next, err := url.Parse(strings.Trim(strings.TrimSpace(parts[0]), "<>"))
		if err != nil || next.Scheme != current.Scheme || next.Host != current.Host {
			return "", errors.New("github releases list: invalid next page")
		}
		return next.String(), nil
	}
	return "", nil
}

// Info returns the cached view. Skip state is re-read from the store on each
// call so a Skip/Unskip from another request is reflected immediately without
// broadcasting. The release slots are re-read on every call, so a rollback
// candidate or free space is current without waiting for the next Check.
func (c *Checker) Info() Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloadSkipLocked()
	c.refreshRuntimeInfoLocked()
	return c.info
}

func (c *Checker) refreshRuntimeInfoLocked() {
	c.info.Previous = ""
	c.info.InstallReady = false
	c.info.Native = c.cfg.NativeRoot != ""
	if !c.info.Native {
		return
	}
	manager := nativeupdate.Manager{Root: c.cfg.NativeRoot}
	state, err := manager.Read()
	if err == nil {
		_, err = manager.ReleaseDir(state.Current)
	}
	c.info.InstallReady = err == nil
	c.info.InstallRoot = c.cfg.NativeRoot
	c.info.InstallFreeBytes, _ = manager.FreeBytes()
	c.info.InstallNeedBytes = 0
	c.info.LastFailed = state.LastFailed
	if err == nil {
		if previous, rollbackErr := manager.RollbackCandidate(); rollbackErr == nil {
			c.info.Previous = previous
		}
		c.info.InstallNeedBytes, _ = manager.SpaceNeeded(state.Current)
	}
}

func (c *Checker) TriggerNativeRollback() (string, error) {
	if c.cfg.NativeRoot == "" || c.cfg.NativeRestart == nil {
		return "", errors.New("selfupdate: native rollback is unavailable")
	}
	manager := nativeupdate.Manager{Root: c.cfg.NativeRoot}
	previous, err := manager.PrepareRollback()
	if err != nil {
		return "", err
	}
	now := c.cfg.Now()
	status := UpdateStatus{State: "restarting", Action: "rollback", Component: "core", Target: previous,
		StartedAt: now, PhaseStartedAt: now, UpdatedAt: now,
		Message: "Starting the previous Core once", Step: 1, TotalSteps: 2}
	if err := c.WriteStatus(status); err != nil {
		_ = manager.CancelPrepared(previous)
		return "", err
	}
	if err := c.cfg.NativeRestart(); err != nil {
		_ = manager.CancelPrepared(previous)
		status.State = "failed"
		status.Message = err.Error()
		status.UpdatedAt = c.cfg.Now()
		_ = c.WriteStatus(status)
		return "", err
	}
	return previous, nil
}

// SetChannel persists an operator-selected release stream and clears the
// cached target. It does not pull or restart anything; the caller performs a
// fresh Check and the normal update endpoint remains the only mutation path.
func (c *Checker) SetChannel(channel Channel) error {
	parsed, err := ParseChannel(string(channel))
	if err != nil {
		return err
	}
	if c.store == nil {
		return errors.New("selfupdate: no store configured")
	}
	if err := c.store.SaveConfig(channelKey, string(parsed)); err != nil {
		return err
	}
	c.mu.Lock()
	c.info.Channel = parsed
	c.info.Latest = ""
	c.info.PublishedAt = time.Time{}
	c.info.ReleaseNotesURL = ""
	c.info.ReleaseBody = ""
	c.info.TargetStateSchema = 0
	c.info.FullBackupRequired = true
	c.info.CheckedAt = time.Time{}
	c.info.UpdateAvailable = false
	c.info.Err = ""
	c.info.Skipped = false
	c.lastAnnouncedTag = ""
	c.mu.Unlock()
	return nil
}

func (c *Checker) reloadSkipLocked() {
	if c.store == nil {
		return
	}
	v, ok := c.store.LoadConfig(skippedKey)
	if !ok {
		v = ""
	}
	c.info.SkippedVersion = v
	// Only "skipped" when the persisted version matches the *current* latest.
	// A newer release resurfaces automatically because SkippedVersion !=
	// Latest, so we never silently hide a version the user didn't ask to hide.
	c.info.Skipped = v != "" && v == c.info.Latest
}

// Skip persists the skipped version. An empty string is rejected — use Unskip.
func (c *Checker) Skip(version string) error {
	if c.store == nil {
		return errors.New("selfupdate: no store configured")
	}
	if version == "" {
		return errors.New("selfupdate: empty version")
	}
	if err := c.store.SaveConfig(skippedKey, version); err != nil {
		return err
	}
	c.mu.Lock()
	c.info.SkippedVersion = version
	c.info.Skipped = version == c.info.Latest
	c.mu.Unlock()
	return nil
}

// Unskip clears the persisted skip, so the next check surfaces the
// currently-latest release regardless of what was previously hidden.
func (c *Checker) Unskip() error {
	if c.store == nil {
		return errors.New("selfupdate: no store configured")
	}
	if err := c.store.SaveConfig(skippedKey, ""); err != nil {
		return err
	}
	c.mu.Lock()
	c.info.SkippedVersion = ""
	c.info.Skipped = false
	c.mu.Unlock()
	return nil
}

// TriggerRestart restarts the installed Core. No other release is selected:
// the launcher starts the current slot again.
func (c *Checker) TriggerRestart() error {
	if c.cfg.NativeRoot == "" || c.cfg.NativeRestart == nil {
		return errors.New("selfupdate: native restart is not configured")
	}
	now := c.cfg.Now()
	status := UpdateStatus{State: "restarting", Action: "restart", Component: "core", Target: c.Info().Current,
		StartedAt: now, PhaseStartedAt: now, UpdatedAt: now,
		Message: "Restarting the installed Core", Step: 1, TotalSteps: 2}
	if err := c.WriteStatus(status); err != nil {
		return err
	}
	if err := c.cfg.NativeRestart(); err != nil {
		status.State = "failed"
		status.Message = err.Error()
		status.UpdatedAt = c.cfg.Now()
		_ = c.WriteStatus(status)
		return err
	}
	return nil
}

// NativeUpdateSteps counts a native update's phases: download, check and
// start. A native update takes no rollback point (ADR 0007, decision 3).
const NativeUpdateSteps = 3

// TriggerUpdate downloads and verifies target, stages it as the next slot
// and asks Core to stop so the launcher starts it once. startedAt is when
// the caller accepted the request, so the new Core finishes the same
// history record.
func (c *Checker) TriggerUpdate(ctx context.Context, target string, startedAt time.Time) error {
	if c.cfg.NativeRoot == "" || c.cfg.NativeRestart == nil {
		return errors.New("selfupdate: native restart is not configured")
	}
	if !nativeupdate.ValidTag(target) || !strings.HasPrefix(target, "v0.") {
		return fmt.Errorf("selfupdate: invalid native update target %q", target)
	}
	info := c.Info()
	if target != info.Latest || !info.UpdateAvailable || !info.InstallReady {
		return errors.New("selfupdate: native target is not the available verified release")
	}
	if info.FullBackupRequired {
		// Prepare refuses a state-schema change; refusing here as well
		// keeps a repeated request from downloading the package again.
		return errors.New("selfupdate: " + target + " changes stored data, which a native update cannot take yet")
	}
	if startedAt.IsZero() {
		startedAt = c.cfg.Now()
	}
	manager := nativeupdate.Manager{Root: c.cfg.NativeRoot}
	slots, err := manager.Read()
	if err != nil {
		return err
	}
	if err := manager.CheckSpace(slots.Current); err != nil {
		return err
	}
	phaseStarted := c.cfg.Now()
	status := UpdateStatus{State: "pulling", Action: "update", Component: "core", Target: target,
		StartedAt: startedAt, PhaseStartedAt: phaseStarted, UpdatedAt: phaseStarted,
		Message: "Downloading verified Core release", Step: 1, TotalSteps: NativeUpdateSteps, ProgressUnit: "bytes"}
	if err := c.WriteStatus(status); err != nil {
		return err
	}
	lastWrite := time.Time{}
	// Version probes have a short HTTP deadline; an archive can take much
	// longer on a slow site. Keep the same transport while using the update
	// operation's own deadline for the transfer.
	downloadClient := *c.cfg.HTTPClient
	downloadClient.Timeout = 0
	downloader := nativeupdate.Downloader{Manager: manager, ReleaseBaseURL: c.cfg.NativeReleaseURL,
		HTTPClient: &downloadClient, Progress: func(copied, total int64) {
			if time.Since(lastWrite) < time.Second && copied != total {
				return
			}
			lastWrite = time.Now()
			status.ProgressCurrent = copied
			status.ProgressTotal = total
			status.UpdatedAt = c.cfg.Now()
			if err := c.WriteStatus(status); err != nil {
				slog.Warn("selfupdate: native download progress write failed", "err", err)
			}
		},
		// Unpacking syncs every file; on an SD card that takes longer than
		// the download, so it is its own step.
		Downloaded: func() {
			status.EndPhase(c.cfg.Now())
			status.State = "checking"
			status.Message = "Unpacking and checking the release"
			status.Step = 2
			status.ProgressCurrent = 0
			status.ProgressTotal = 0
			status.ProgressUnit = ""
			status.PhaseStartedAt = c.cfg.Now()
			status.UpdatedAt = status.PhaseStartedAt
			if err := c.WriteStatus(status); err != nil {
				slog.Warn("selfupdate: native unpack status write failed", "err", err)
			}
		}}
	if err := downloader.Install(ctx, target); err != nil {
		return err
	}
	if err := manager.Prepare(target); err != nil {
		return err
	}
	status.EndPhase(c.cfg.Now())
	status.State = "restarting"
	status.Message = "Starting the new Core once"
	status.Step = 3
	status.PhaseStartedAt = c.cfg.Now()
	status.UpdatedAt = status.PhaseStartedAt
	if err := c.WriteStatus(status); err != nil {
		_ = manager.CancelPrepared(target)
		return err
	}
	if err := c.cfg.NativeRestart(); err != nil {
		_ = manager.CancelPrepared(target)
		return err
	}
	return nil
}

// Status reads the saved update status. Missing or unreadable returns
// {state: idle}. An in-flight state whose last write is too old is
// surfaced as failed so the ftw command and the UI stop waiting.
func (c *Checker) Status() UpdateStatus {
	if c.cfg.StatusPath == "" {
		return UpdateStatus{State: "idle"}
	}
	f, err := os.Open(c.cfg.StatusPath)
	if err != nil {
		return UpdateStatus{State: "idle"}
	}
	defer f.Close()
	var st UpdateStatus
	if err := json.NewDecoder(f).Decode(&st); err != nil || st.State == "" {
		return UpdateStatus{State: "idle"}
	}
	if isInFlightState(st.State) && !st.UpdatedAt.IsZero() {
		threshold := staleThreshold
		if st.State == "restarting" && c.cfg.NativeTrialTimeout > threshold {
			threshold = c.cfg.NativeTrialTimeout
		}
		if c.cfg.Now().Sub(st.UpdatedAt) > threshold {
			st.State = "failed"
			if st.Message == "" {
				st.Message = fmt.Sprintf("no updater heartbeat for %s", threshold)
			}
		}
	}
	return st
}

// WriteStatus replaces the saved update status in one rename, so a reader
// never sees a partial file.
func (c *Checker) WriteStatus(st UpdateStatus) error {
	if c.cfg.StatusPath == "" {
		return nil
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = c.cfg.Now()
	}
	tmp := c.cfg.StatusPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, c.cfg.StatusPath)
}

func isInFlightState(state string) bool {
	switch state {
	case "starting", "pulling", "checking", "restarting":
		return true
	default:
		return false
	}
}

func inferChannel(version string) Channel {
	switch {
	case strings.HasPrefix(version, "edge-"):
		// Releases before the two-channel model used immutable edge tags. Treat
		// a running legacy edge build as beta so it can converge automatically.
		return ChannelBeta
	case isBetaTag(version):
		return ChannelBeta
	default:
		return ChannelStable
	}
}

func channelUpdateAvailable(latest, current string) bool {
	if latest == "" || latest == current {
		return false
	}
	// An empty current version is absence of knowledge, not an old version;
	// isNewer would read it as "older than everything" and light the update
	// badge forever. The literal "dev" of an unstamped build still falls
	// through to isNewer.
	if strings.TrimSpace(current) == "" {
		return false
	}
	return isNewer(latest, current)
}

func isBetaTag(tag string) bool {
	v := parseSemanticVersion(tag)
	return v != nil && len(v.pre) == 2 && v.pre[0] == "beta" && isDigits(v.pre[1])
}

func isStableTag(tag string) bool {
	v := parseSemanticVersion(tag)
	return v != nil && len(v.pre) == 0
}

// isNewer implements the SemVer precedence needed by stable and beta,
// including beta.1 -> beta.2 and prerelease -> stable promotion.
func isNewer(latest, current string) bool {
	if latest == "" || latest == current {
		return false
	}
	l := parseSemanticVersion(latest)
	cc := parseSemanticVersion(current)
	if l == nil {
		return false
	}
	if cc == nil {
		return true
	}
	for i := 0; i < 3; i++ {
		if l.numbers[i] > cc.numbers[i] {
			return true
		}
		if l.numbers[i] < cc.numbers[i] {
			return false
		}
	}
	if len(l.pre) == 0 {
		return len(cc.pre) > 0
	}
	if len(cc.pre) == 0 {
		return false
	}
	for i := 0; i < len(l.pre) && i < len(cc.pre); i++ {
		if l.pre[i] == cc.pre[i] {
			continue
		}
		ln, lok := numericIdentifier(l.pre[i])
		cn, cok := numericIdentifier(cc.pre[i])
		switch {
		case lok && cok:
			return ln > cn
		case lok:
			return false
		case cok:
			return true
		default:
			return l.pre[i] > cc.pre[i]
		}
	}
	return len(l.pre) > len(cc.pre)
}

type semanticVersion struct {
	numbers [3]int
	pre     []string
}

func parseSemanticVersion(s string) *semanticVersion {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre []string
	if i := strings.IndexByte(s, '-'); i > 0 {
		pre = strings.Split(s[i+1:], ".")
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil
	}
	out := &semanticVersion{pre: pre}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out.numbers[i] = n
	}
	for _, id := range pre {
		if id == "" {
			return nil
		}
	}
	return out
}

func numericIdentifier(s string) (int, bool) {
	if !isDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// truncateBody caps release-body markdown to MaxReleaseBodyBytes so a
// runaway release note (auto-generated from hundreds of commits on a
// long-lived branch) can't inflate /api/version/check payloads. When we
// cut, we leave a clear marker so the UI can point the operator at
// ReleaseNotesURL for the rest.
func truncateBody(b string) string {
	if len(b) <= MaxReleaseBodyBytes {
		return b
	}
	return b[:MaxReleaseBodyBytes] + "\n\n…(truncated — see release notes for full changelog)"
}

// Release notes carry two hidden markers. Cores newer than v3.6.0-beta.1
// read stateSchemaMarkerV2, the release's real on-disk state schema. Older
// Cores read only stateSchemaMarkerLegacy. Those Cores copied their whole
// history before any update whose marker differed from their own schema,
// and on a Raspberry Pi with a large history that copy could not finish
// (#1302). The release workflows therefore keep the legacy marker at
// state-schema.json's legacy_marker, the last schema those Cores use, so
// they skip the copy and update.
const (
	stateSchemaMarkerV2     = "<!-- ftw-state-schema-v2:"
	stateSchemaMarkerLegacy = "<!-- ftw-state-schema:"
)

func releaseStateSchema(body string) int {
	if schema := parseStateSchemaMarker(body, stateSchemaMarkerV2); schema > 0 {
		return schema
	}
	return parseStateSchemaMarker(body, stateSchemaMarkerLegacy)
}

func parseStateSchemaMarker(body, prefix string) int {
	start := strings.Index(body, prefix)
	if start < 0 {
		return 0
	}
	value := body[start+len(prefix):]
	end := strings.Index(value, "-->")
	if end < 0 {
		return 0
	}
	schema, err := strconv.Atoi(strings.TrimSpace(value[:end]))
	if err != nil || schema <= 0 {
		return 0
	}
	return schema
}

func releaseBodyWithoutStateSchema(body string) string {
	for _, prefix := range []string{stateSchemaMarkerV2, stateSchemaMarkerLegacy} {
		body = stripStateSchemaMarker(body, prefix)
	}
	return body
}

func stripStateSchemaMarker(body, prefix string) string {
	start := strings.Index(body, prefix)
	if start < 0 {
		return body
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, "-->")
	if end < 0 {
		return body
	}
	return strings.TrimSpace(body[:start] + rest[end+3:])
}
