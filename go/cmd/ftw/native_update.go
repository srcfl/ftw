package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/nativeupdate"
	"github.com/srcfl/ftw/go/internal/selfupdate"
)

const nativeTrialTimeout = 6 * time.Hour

// Build metadata identifies local patches without changing the installed
// release's SemVer identity. Published trial binaries still require an exact tag.
func nativeSlotVersion(version string) string {
	base, metadata, found := strings.Cut(version, "+")
	if !found || !nativeupdate.ValidTag(base) {
		return version
	}
	for _, id := range strings.Split(metadata, ".") {
		if id == "" {
			return version
		}
		for _, c := range id {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return version
			}
		}
	}
	return base
}

type nativeTrial struct {
	manager nativeupdate.Manager
	tag     string
	timer   *time.Timer
}

func beginNativeTrial(root, tag, version string) (*nativeTrial, error) {
	if root == "" {
		if tag != "" {
			return nil, errors.New("native trial tag has no slot root")
		}
		return nil, nil
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("native slot root must be absolute")
	}
	manager := nativeupdate.Manager{Root: root}
	state, err := manager.Read()
	if err != nil {
		return nil, err
	}
	if tag == "" {
		if state.Current != nativeSlotVersion(version) {
			return nil, fmt.Errorf("native current slot %s does not match binary %s", state.Current, version)
		}
		return nil, nil
	}
	if !nativeupdate.ValidTag(tag) || version != tag {
		return nil, fmt.Errorf("native trial %q does not match binary %q", tag, version)
	}
	if state.Trial != tag || state.Current == tag {
		return nil, fmt.Errorf("native trial %s is not pending in slots", tag)
	}
	trial := &nativeTrial{manager: manager, tag: tag}
	trial.timer = time.AfterFunc(nativeTrialTimeout, func() {
		slog.Error("native trial did not reach readiness before deadline", "tag", tag)
		os.Exit(1)
	})
	return trial, nil
}

// Complete only after the first control tick has finished. By then state,
// drivers and the full API are up, and the control loop runs.
func (trial *nativeTrial) complete(checker *selfupdate.Checker) error {
	if trial == nil {
		return nil
	}
	if err := trial.manager.Commit(trial.tag); err != nil {
		return err
	}
	trial.timer.Stop()
	if checker != nil {
		status := checker.Status()
		status.EndPhase(time.Now())
		status.State = "done"
		status.Message = "Core is ready on " + trial.tag
		status.UpdatedAt = time.Now()
		status.PhaseStartedAt = status.UpdatedAt
		status.Step = status.TotalSteps
		if err := checker.WriteStatus(status); err != nil {
			slog.Warn("native update status could not be saved", "err", err)
		}
	}
	if _, err := trial.manager.Prune(); err != nil {
		slog.Warn("native old releases could not be pruned", "err", err)
	}
	return nil
}

func reconcileNativeFallback(root, version string, checker *selfupdate.Checker) {
	if root == "" || checker == nil {
		return
	}
	status := checker.Status()
	if status.Action == "restart" && status.State == "restarting" && status.Target == version {
		status.EndPhase(time.Now())
		status.State = "done"
		status.Message = "Core restarted on " + version
		status.UpdatedAt = time.Now()
		status.PhaseStartedAt = status.UpdatedAt
		status.Step = status.TotalSteps
		if err := checker.WriteStatus(status); err != nil {
			slog.Warn("native restart status could not be saved", "err", err)
		}
		return
	}
	state, err := (nativeupdate.Manager{Root: root}).Read()
	if err != nil || state.Current != nativeSlotVersion(version) || state.LastFailed == "" || status.Target != state.LastFailed {
		return
	}
	switch status.State {
	case "restarting", "failed":
		status.Message = "New Core did not reach readiness; previous Core is running"
	case "done":
		// It committed, then kept stopping without a clean shutdown, and the
		// launcher went back during its probation.
		status.Message = status.Target + " kept stopping after it started; " + version + " runs again"
	default:
		return
	}
	status.State = "failed"
	status.UpdatedAt = time.Now()
	status.PhaseStartedAt = status.UpdatedAt
	if err := checker.WriteStatus(status); err != nil {
		slog.Warn("native fallback status could not be saved", "err", err)
	}
}

// markNativeCleanExit tells the launcher that this stop is on purpose, so
// the next start does not count as a crash during an update's probation.
func markNativeCleanExit(root string) {
	if root == "" {
		return
	}
	if err := (nativeupdate.Manager{Root: root}).MarkCleanExit(nativeSlotVersion(Version)); err != nil {
		slog.Warn("native clean-exit mark could not be saved", "err", err)
	}
}
