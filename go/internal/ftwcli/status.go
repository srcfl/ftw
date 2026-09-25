package ftwcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type versionInfo struct {
	Current            string    `json:"current"`
	Native             bool      `json:"native"`
	Previous           string    `json:"previous"`
	Channel            string    `json:"channel"`
	Latest             string    `json:"latest"`
	UpdateAvailable    bool      `json:"update_available"`
	FullBackupRequired bool      `json:"full_backup_required"`
	CurrentStateSchema int       `json:"current_state_schema"`
	TargetStateSchema  int       `json:"target_state_schema"`
	CheckedAt          time.Time `json:"checked_at"`
	Err                string    `json:"err"`
	InstallRoot        string    `json:"install_root"`
	InstallFreeBytes   int64     `json:"install_free_bytes"`
	InstallNeedBytes   int64     `json:"install_need_bytes"`
	LastFailed         string    `json:"last_failed"`
}

type updateStatus struct {
	State           string    `json:"state"`
	Action          string    `json:"action"`
	Target          string    `json:"target"`
	Message         string    `json:"message"`
	Step            int       `json:"step"`
	TotalSteps      int       `json:"total_steps"`
	ProgressCurrent int64     `json:"progress_current"`
	ProgressTotal   int64     `json:"progress_total"`
	ProgressUnit    string    `json:"progress_unit"`
	UpdatedAt       time.Time `json:"updated_at"`
	// Phases are the run's finished phases with Core's own timing.
	Phases []phaseRecord `json:"phases"`
}

type phaseRecord struct {
	Step       int       `json:"step"`
	TotalSteps int       `json:"total_steps"`
	Message    string    `json:"message"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Bytes      int64     `json:"bytes"`
}

func (r phaseRecord) label() string {
	if r.Step > 0 && r.TotalSteps > 0 {
		return fmt.Sprintf("%d/%d %s", r.Step, r.TotalSteps, r.Message)
	}
	return r.Message
}

func (r phaseRecord) summary() string {
	took := r.FinishedAt.Sub(r.StartedAt)
	if r.Bytes <= 0 {
		return "in " + formatTook(took)
	}
	line := formatBytes(r.Bytes) + " in " + formatTook(took)
	if took >= 100*time.Millisecond {
		line += " (" + formatBytes(int64(float64(r.Bytes)/took.Seconds())) + "/s)"
	}
	return line
}

func (st updateStatus) inFlight() bool {
	switch st.State {
	case "starting", "snapshotting", "pulling", "restarting", "checking", "restoring":
		return true
	}
	return false
}

type health struct {
	Status          string `json:"status"`
	Phase           string `json:"phase"`
	DriversOK       int    `json:"drivers_ok"`
	DriversDegraded int    `json:"drivers_degraded"`
	DriversOffline  int    `json:"drivers_offline"`
	DriversFaulted  int    `json:"drivers_faulted"`
	History         *struct {
		Migration struct {
			State string `json:"state"`
		} `json:"migration"`
		Writer struct {
			CommitFailures uint64 `json:"commit_failures"`
		} `json:"writer"`
	} `json:"history_storage"`
	// Migration is the history migration a starting Core reports.
	Migration *struct {
		State            string `json:"state"`
		RowsDone         int64  `json:"rows_done"`
		RowsTotal        int64  `json:"rows_total"`
		SourceBytesDone  *int64 `json:"source_bytes_done"`
		SourceBytesTotal *int64 `json:"source_bytes_total"`
	} `json:"migration"`
}

func (h health) drivers() string {
	return fmt.Sprintf("drivers %d ok, %d degraded, %d offline, %d faulted",
		h.DriversOK, h.DriversDegraded, h.DriversOffline, h.DriversFaulted)
}

// waitingForSetup reports a Core that has no site configuration yet and
// serves only the setup wizard.
func (c *client) waitingForSetup(ctx context.Context) bool {
	var health struct{}
	var api *apiError
	if err := c.get(ctx, "/api/health", &health); !errors.As(err, &api) || api.status != http.StatusNotFound {
		return false
	}
	resp, err := c.open(ctx, "/setup")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// selfUpdateOff reports Core's answer when it does not manage its own
// updates: a container, the Home Assistant add-on or a plain binary.
func selfUpdateOff(err error) bool {
	var api *apiError
	return errors.As(err, &api) && api.status == http.StatusServiceUnavailable
}

func runStatus(args []string, out io.Writer, e env) error {
	base, err := parse(args, out, "ftw status [--url URL]", nil)
	if err != nil {
		return err
	}
	c := newClient(base, e)
	ctx := context.Background()
	var h health
	if err := c.get(ctx, "/api/health", &h); err != nil {
		if c.waitingForSetup(ctx) {
			fmt.Fprintf(out, "Core:     waiting for setup; open %s/setup\n", base)
			printNextSteps(out, e)
			return errors.New("Core is waiting for setup")
		}
		fmt.Fprintf(out, "Core:     not answering at %s (%s)\n", base, err)
		printNextSteps(out, e)
		if e.systemd {
			fmt.Fprintf(out, "Back:     %s\n", offlineRollback)
		}
		return errors.New("Core is not answering")
	}
	if h.Status == "starting" {
		fmt.Fprintf(out, "Core:     starting: %s\n", h.Phase)
		printNextSteps(out, e)
		return errors.New("Core is still starting")
	}

	var problems []string
	var info versionInfo
	infoErr := c.get(ctx, "/api/version/check", &info)
	switch {
	case infoErr == nil && info.Native:
		fmt.Fprintf(out, "Core:     %s, native, %s channel\n", info.Current, info.Channel)
		fmt.Fprintf(out, "Release:  %s\n", releaseLine(info))
		var st updateStatus
		if err := c.get(ctx, "/api/version/update/status", &st); err == nil {
			if line := lastRunLine(st); line != "" {
				fmt.Fprintf(out, "Update:   %s\n", line)
			}
		}
		if info.Previous != "" {
			fmt.Fprintf(out, "Previous: %s (ftw rollback returns to it)\n", info.Previous)
		}
		if info.LastFailed != "" && info.LastFailed != info.Latest {
			fmt.Fprintf(out, "Failed:   %s failed on this box\n", info.LastFailed)
		}
		if info.InstallRoot != "" {
			fmt.Fprintf(out, "Releases: %s\n", spaceLine(filepath.Join(info.InstallRoot, "releases"), info.InstallFreeBytes, info.InstallNeedBytes))
			if info.InstallNeedBytes > 0 && info.InstallFreeBytes > 0 && info.InstallFreeBytes < info.InstallNeedBytes {
				problems = append(problems, "not enough disk space for the next release")
			}
		}
		c.printUnusedRollbackPoints(ctx, out)
	case infoErr == nil:
		fmt.Fprintf(out, "Core:     %s, not a native install; ftw does not update it\n", info.Current)
	case selfUpdateOff(infoErr):
		var site struct {
			Version string `json:"version"`
		}
		_ = c.get(ctx, "/api/status", &site)
		fmt.Fprintf(out, "Core:     %s, updates are managed outside FTW\n", orUnknown(site.Version))
	default:
		fmt.Fprintf(out, "Core:     version not readable (%s)\n", infoErr)
	}

	c.printBackups(ctx, out)
	fmt.Fprintf(out, "Health:   %s; %s\n", h.Status, h.drivers())
	c.printDrivers(ctx, out)
	if h.History != nil {
		fmt.Fprintf(out, "History:  %s; %d write failures\n", orUnknown(h.History.Migration.State), h.History.Writer.CommitFailures)
	}
	printNextSteps(out, e)
	if h.Status != "ok" {
		problems = append([]string{"health is " + h.Status}, problems...)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// printBackups names where full backups go and the space left there.
// printDrivers names the version each configured driver runs. Drivers ship
// with the release, so only a file from elsewhere gets its own line. A Core
// that does not say which file a driver runs leaves the section out.
func (c *client) printDrivers(ctx context.Context, out io.Writer) {
	var catalog struct {
		Entries []struct {
			Version        string   `json:"version"`
			Source         string   `json:"source"`
			UsedBy         []string `json:"used_by"`
			ReleaseVersion string   `json:"release_version"`
			Chosen         bool     `json:"chosen"`
		} `json:"entries"`
	}
	if err := c.get(ctx, "/api/drivers/catalog", &catalog); err != nil {
		return
	}
	var running, overrides []string
	for _, e := range catalog.Entries {
		for _, name := range e.UsedBy {
			running = append(running, name+" "+orUnknown(e.Version))
			release := "the release has no copy"
			if e.ReleaseVersion != "" {
				release = "the release has " + e.ReleaseVersion
			}
			switch {
			case e.Source == "managed" && e.Chosen:
				overrides = append(overrides, fmt.Sprintf("%s %s, chosen and kept across updates; %s", name, orUnknown(e.Version), release))
			case e.Source == "managed" && e.ReleaseVersion != "":
				overrides = append(overrides, fmt.Sprintf("%s %s from the driver channel until a release has it; %s", name, orUnknown(e.Version), release))
			case e.Source == "managed":
				overrides = append(overrides, fmt.Sprintf("%s %s from the driver channel; %s", name, orUnknown(e.Version), release))
			case e.Source == "local":
				overrides = append(overrides, fmt.Sprintf("%s runs a local file; %s", name, release))
			}
		}
	}
	if len(running) == 0 {
		return
	}
	sort.Strings(running)
	sort.Strings(overrides)
	fmt.Fprintf(out, "Drivers:  %s\n", strings.Join(running, ", "))
	for _, line := range overrides {
		fmt.Fprintf(out, "Override: %s\n", line)
	}
}

func (c *client) printBackups(ctx context.Context, out io.Writer) {
	var list struct {
		Dir       string `json:"dir"`
		FreeBytes int64  `json:"free_bytes"`
		Backups   []struct {
			SizeBytes int64 `json:"size_bytes"`
		} `json:"backups"`
	}
	if c.get(ctx, "/api/backups", &list) != nil || list.Dir == "" {
		return
	}
	line := fmt.Sprintf("%s, %d archives", list.Dir, len(list.Backups))
	if list.FreeBytes > 0 {
		line += ", " + formatBytes(list.FreeBytes) + " free"
	}
	fmt.Fprintf(out, "Backups:  %s\n", line)
}

// printUnusedRollbackPoints reports rollback points an older Core left.
// Native Core neither takes nor restores them; deleting them is the
// owner's choice.
func (c *client) printUnusedRollbackPoints(ctx context.Context, out io.Writer) {
	var list struct {
		Dir       string `json:"dir"`
		Snapshots []struct {
			SizeBytes int64 `json:"size_bytes"`
		} `json:"snapshots"`
	}
	if c.get(ctx, "/api/version/snapshots", &list) != nil || len(list.Snapshots) == 0 || list.Dir == "" {
		return
	}
	var size int64
	for _, snapshot := range list.Snapshots {
		size += snapshot.SizeBytes
	}
	fmt.Fprintf(out, "Unused:   %d rollback points from older updates, %s, in %s; native Core does not use them\n",
		len(list.Snapshots), formatBytes(size), list.Dir)
}

// offlineRollback is the way back when Core does not start, for an install
// made by install.sh.
const offlineRollback = "if a new release does not start, run: sudo -u ftw /opt/ftw/ftw-launcher -root /opt/ftw rollback && sudo systemctl restart ftw"

func printNextSteps(out io.Writer, e env) {
	if !e.systemd {
		return
	}
	fmt.Fprintln(out, "Logs:     journalctl -u ftw -n 100")
	fmt.Fprintln(out, "Restart:  sudo systemctl restart ftw")
}

func releaseLine(info versionInfo) string {
	checked := ""
	if !info.CheckedAt.IsZero() {
		checked = " (checked " + info.CheckedAt.Local().Format("Jan 2 15:04") + ")"
	}
	switch {
	case info.Err != "":
		return "check failed: " + info.Err + checked
	case info.UpdateAvailable && info.Latest == info.LastFailed:
		return info.Latest + " is published but failed on this box; ftw update waits for a newer release (ftw update --retry tries it again)" + checked
	case info.UpdateAvailable:
		return info.Latest + " is published. Install it with: ftw update" + checked
	default:
		return "nothing newer on " + info.Channel + checked
	}
}

func lastRunLine(st updateStatus) string {
	action := st.Action
	if action == "" {
		action = "update"
	}
	when := ""
	if !st.UpdatedAt.IsZero() {
		when = " (" + st.UpdatedAt.Local().Format("Jan 2 15:04") + ")"
	}
	switch {
	case st.inFlight():
		return action + " running: " + progressLine(st)
	case st.State == "done":
		return "last " + action + " done: " + orUnknown(st.Message) + when
	case st.State == "failed":
		return "last " + action + " FAILED: " + orUnknown(st.Message) + when
	}
	return ""
}

// progressLine describes one status without elapsed time, so an unchanged
// phase can be recognised.
func progressLine(st updateStatus) string {
	parts := []string{}
	if st.Step > 0 && st.TotalSteps > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d", st.Step, st.TotalSteps))
	}
	if st.Message != "" {
		parts = append(parts, st.Message)
	} else {
		parts = append(parts, st.State)
	}
	if st.ProgressUnit == "bytes" && st.ProgressCurrent > 0 {
		if st.ProgressTotal > 0 {
			parts = append(parts, formatBytes(st.ProgressCurrent)+" of "+formatBytes(st.ProgressTotal))
		} else {
			parts = append(parts, formatBytes(st.ProgressCurrent)+", total unknown")
		}
	}
	return strings.Join(parts, " ")
}

func formatBytes(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d B", n)
}

// formatTook gives a finished phase's duration, with tenths below ten
// seconds so a fast step does not read as zero.
func formatTook(d time.Duration) string {
	if d >= 0 && d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return formatElapsed(d)
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
