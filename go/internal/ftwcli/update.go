package ftwcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"
)

const notNative = "this Core is not a native install; ftw only updates native FTW"

func runUpdate(args []string, out io.Writer, e env) error {
	var channel string
	var retry bool
	base, err := parse(args, out, "ftw update [--channel beta|stable] [--retry] [--url URL]", func(fs *flag.FlagSet) {
		fs.StringVar(&channel, "channel", "", "")
		fs.BoolVar(&retry, "retry", false, "")
	})
	if err != nil {
		return err
	}
	if channel != "" && channel != "beta" && channel != "stable" {
		return usageError("--channel must be beta or stable, not " + channel)
	}
	c := newClient(base, e)
	ctx := context.Background()

	info, err := c.nativeInfo(ctx)
	if err != nil {
		return err
	}
	if st, err := c.status(ctx); err == nil && st.inFlight() {
		fmt.Fprintf(out, "An %s to %s is already running; following it.\n", orUnknown(st.Action), orUnknown(st.Target))
		return c.finish(ctx, out, st.Action, st.Target, info.Current)
	}
	if channel != "" && channel != info.Channel {
		body := map[string]string{"channel": channel}
		if err := c.call(ctx, http.MethodPost, "/api/version/channel", body, nil, e.requestTimeout); err != nil {
			return err
		}
		fmt.Fprintf(out, "Channel: %s -> %s\n", info.Channel, channel)
	}
	return c.update(ctx, out, retry)
}

func (c *client) update(ctx context.Context, out io.Writer, retry bool) error {
	var info versionInfo
	if err := c.get(ctx, "/api/version/check?force=1", &info); err != nil {
		return fmt.Errorf("release check failed: %w", err)
	}
	if info.Err != "" {
		return errors.New("release check failed: " + info.Err)
	}
	if !info.UpdateAvailable {
		fmt.Fprintf(out, "Already current on %s: %s\n", info.Channel, info.Current)
		return nil
	}
	if info.LastFailed != "" && info.LastFailed == info.Latest && !retry {
		// An unattended run would otherwise install a failing release again
		// and again.
		return fmt.Errorf("%s already failed on this box, so ftw update waits for a newer release; run ftw update --retry to try it again", info.Latest)
	}
	if info.FullBackupRequired {
		return fmt.Errorf("%s changes stored data (state schema %d -> %d), which a native update cannot take yet; %s stays installed",
			info.Latest, info.CurrentStateSchema, info.TargetStateSchema, info.Current)
	}
	fmt.Fprintf(out, "Updating %s -> %s on %s\n", info.Current, info.Latest, info.Channel)
	if info.InstallRoot != "" {
		fmt.Fprintf(out, "Releases: %s\n", spaceLine(filepath.Join(info.InstallRoot, "releases"), info.InstallFreeBytes, info.InstallNeedBytes))
		if info.InstallNeedBytes > 0 && info.InstallFreeBytes > 0 && info.InstallFreeBytes < info.InstallNeedBytes {
			return fmt.Errorf("not enough disk space for %s: %s free, %s needed; free some space, then run ftw update again",
				info.Latest, formatBytes(info.InstallFreeBytes), formatBytes(info.InstallNeedBytes))
		}
	}
	var started struct {
		Target string `json:"target"`
	}
	if err := c.post(ctx, "/api/version/update", &started); err != nil {
		return err
	}
	if started.Target != info.Latest {
		return fmt.Errorf("Core started an update to %s, not %s; check it with ftw status", orUnknown(started.Target), info.Latest)
	}
	return c.finish(ctx, out, "update", info.Latest, info.Current)
}

func runRollback(args []string, out io.Writer, e env) error {
	base, err := parse(args, out, "ftw rollback [--url URL]", nil)
	if err != nil {
		return err
	}
	c := newClient(base, e)
	ctx := context.Background()
	info, err := c.nativeInfo(ctx)
	if err != nil {
		return err
	}
	if info.Previous == "" {
		return errors.New("there is no previous release that can read the current data")
	}
	if st, err := c.status(ctx); err == nil && st.inFlight() {
		return fmt.Errorf("an %s to %s is running; wait for it, then run ftw status", orUnknown(st.Action), orUnknown(st.Target))
	}
	fmt.Fprintf(out, "Returning %s -> %s\n", info.Current, info.Previous)
	var started struct {
		Target string `json:"target"`
	}
	if err := c.post(ctx, "/api/version/binary-rollback", &started); err != nil {
		return err
	}
	if started.Target == "" {
		started.Target = info.Previous
	}
	return c.finish(ctx, out, "rollback", started.Target, info.Current)
}

// nativeInfo reads the cached release state and refuses a Core that does not
// update itself natively.
func (c *client) nativeInfo(ctx context.Context) (versionInfo, error) {
	var info versionInfo
	if err := c.get(ctx, "/api/version/check", &info); err != nil {
		if c.waitingForSetup(ctx) {
			return info, fmt.Errorf("Core is waiting for setup; finish it at %s/setup first", c.base)
		}
		if selfUpdateOff(err) {
			return info, errors.New(notNative)
		}
		return info, fmt.Errorf("Core is not answering at %s: %w; %s", c.base, err, offlineRollback)
	}
	if !info.Native {
		return info, errors.New(notNative)
	}
	return info, nil
}

func (c *client) status(ctx context.Context) (updateStatus, error) {
	var st updateStatus
	err := c.get(ctx, "/api/version/update/status", &st)
	return st, err
}

// finish waits for an update or rollback to end and checks what runs.
func (c *client) finish(ctx context.Context, out io.Writer, action, target, from string) error {
	start := c.env.now()
	m := newMeter(out, c.env)
	st, err := c.follow(ctx, m, action, target, start)
	if err != nil {
		return err
	}
	if st.State == "failed" {
		running := "unknown"
		var info versionInfo
		if c.get(ctx, "/api/version/check", &info) == nil {
			running = info.Current
		}
		return fmt.Errorf("%s to %s failed: %s. Core %s is running; see ftw status", action, target, orUnknown(st.Message), running)
	}
	var info versionInfo
	if err := c.get(ctx, "/api/version/check", &info); err != nil {
		return fmt.Errorf("%s finished, but the running version is not readable: %w", action, err)
	}
	if info.Current != target {
		return fmt.Errorf("%s finished, but Core reports %s instead of %s; see ftw status", action, info.Current, target)
	}
	fmt.Fprintf(out, "Now running %s (was %s) after %s.\n", info.Current, orUnknown(from), formatElapsed(c.env.now().Sub(start)))
	if info.Previous != "" {
		where := info.Previous
		if info.InstallRoot != "" {
			where = filepath.Join(info.InstallRoot, "releases", info.Previous)
		}
		fmt.Fprintf(out, "Previous: %s (ftw rollback returns to it)\n", where)
	}
	c.reportHealth(ctx, out)
	return nil
}

// reportHealth gives a fresh Core a short while to read its devices again;
// readings from before the restart count as stale for a few seconds.
// Health is information here: the run itself has already succeeded.
func (c *client) reportHealth(ctx context.Context, out io.Writer) {
	start := c.env.now()
	for {
		var h health
		err := c.get(ctx, "/api/health", &h)
		if err == nil && h.Status == "ok" {
			fmt.Fprintf(out, "Health: ok; %s.\n", h.drivers())
			return
		}
		if c.env.now().Sub(start) >= c.env.healthSettle {
			if err != nil {
				fmt.Fprintf(out, "Health is not readable (%s). See ftw status.\n", err)
			} else {
				fmt.Fprintf(out, "Health is %s: %s. See ftw status.\n", h.Status, h.drivers())
			}
			return
		}
		c.env.sleep(c.env.pollInterval)
	}
}

// follow polls Core until the run ends. While the next Core starts, Core
// does not answer or answers only /api/health; that time is its own phase,
// unless Core already said it was restarting.
func (c *client) follow(ctx context.Context, m *meter, action, target string, start time.Time) (updateStatus, error) {
	var phase sample
	restarting := false
	printed := 0 // Core's finished phases already shown
	for {
		now := c.env.now()
		if now.Sub(start) > c.env.followLimit {
			m.finish(false)
			return updateStatus{}, fmt.Errorf("no result after %s; Core may still be working. Check it with ftw status", formatElapsed(now.Sub(start)))
		}
		st, err := c.status(ctx)
		if err == nil && st.Target == target && st.Action == action {
			for ; printed < len(st.Phases); printed++ {
				m.recorded(st.Phases[printed].label(), st.Phases[printed].summary())
			}
		}
		switch {
		case err == nil && st.Target == target && st.Action == action && (st.State == "done" || st.State == "failed"):
			switch {
			case st.State == "failed":
				m.finish(false)
			case phase.key == "restart" && finalRecorded(st):
				m.discard() // Core's record already has the restart
			default:
				m.finish(true)
			}
			return st, nil
		case err == nil:
			phase, restarting = statusSample(st), st.State == "restarting"
			m.show(phase)
		default:
			if !restarting {
				phase, restarting = sample{key: "restart", label: "Restarting into " + target}, true
			}
			between := phase
			between.done, between.total, between.unit = 0, 0, ""
			between.detail = "Core is restarting"
			var h health
			if c.get(ctx, "/api/health", &h) == nil && h.Status == "starting" {
				between.detail = "Core starting: " + orUnknown(h.Phase)
				if mig := h.Migration; mig != nil && mig.State != "" && mig.State != "complete" {
					between.detail = "Core migrating history"
					switch {
					case mig.SourceBytesTotal != nil && *mig.SourceBytesTotal > 0 && mig.SourceBytesDone != nil:
						between.unit, between.done, between.total = "bytes", *mig.SourceBytesDone, *mig.SourceBytesTotal
					case mig.RowsTotal > 0:
						between.unit, between.done, between.total = "rows", mig.RowsDone, mig.RowsTotal
					}
				}
			}
			m.show(between)
		}
		c.env.sleep(c.env.pollInterval)
	}
}

// finalRecorded reports whether Core's record of a run includes its last
// step. A Core older than the record, or one that does not note the start
// of the next Core, leaves it out.
func finalRecorded(st updateStatus) bool {
	if len(st.Phases) == 0 {
		return false
	}
	last := st.Phases[len(st.Phases)-1]
	return last.Step > 0 && last.Step == last.TotalSteps
}

// statusSample turns Core's update status into a phase of the meter.
func statusSample(st updateStatus) sample {
	label := st.Message
	if label == "" {
		label = st.State
	}
	if st.Step > 0 && st.TotalSteps > 0 {
		label = fmt.Sprintf("%d/%d %s", st.Step, st.TotalSteps, label)
	}
	s := sample{key: fmt.Sprintf("%s|%d|%s", st.State, st.Step, st.Message), label: label, quiet: st.Step == 0}
	if st.ProgressUnit == "bytes" && (st.ProgressCurrent > 0 || st.ProgressTotal > 0) {
		s.unit, s.done, s.total = "bytes", st.ProgressCurrent, st.ProgressTotal
	}
	return s
}

// spaceLine names a directory with its free space and what a step needs.
func spaceLine(dir string, free, need int64) string {
	line := dir
	if free > 0 {
		line += " (" + formatBytes(free) + " free"
		if need > 0 {
			line += ", the next release needs " + formatBytes(need)
		}
		line += ")"
	}
	return line
}
