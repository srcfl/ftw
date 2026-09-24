package ftwcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
}

func (h health) drivers() string {
	return fmt.Sprintf("drivers %d ok, %d degraded, %d offline, %d faulted",
		h.DriversOK, h.DriversDegraded, h.DriversOffline, h.DriversFaulted)
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
		fmt.Fprintf(out, "Core:     not answering at %s (%s)\n", base, err)
		printNextSteps(out)
		return errors.New("Core is not answering")
	}
	if h.Status == "starting" {
		fmt.Fprintf(out, "Core:     starting: %s\n", h.Phase)
		printNextSteps(out)
		return errors.New("Core is still starting")
	}

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

	fmt.Fprintf(out, "Health:   %s; %s\n", h.Status, h.drivers())
	if h.History != nil {
		fmt.Fprintf(out, "History:  %s; %d write failures\n", orUnknown(h.History.Migration.State), h.History.Writer.CommitFailures)
	}
	printNextSteps(out)
	if h.Status != "ok" {
		return fmt.Errorf("health is %s", h.Status)
	}
	return nil
}

func printNextSteps(out io.Writer) {
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
