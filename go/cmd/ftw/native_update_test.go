package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/nativeupdate"
	"github.com/srcfl/ftw/go/internal/selfupdate"
)

func nativeFixtureRelease(t *testing.T, root, tag string) {
	t.Helper()
	dir := filepath.Join(root, "releases", tag)
	for _, name := range []string{"web", "drivers", "optimizer/native/bundle"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ftw", "web/index.html", "drivers/BUNDLED_SOURCE.json", "optimizer/native/bundle/manifest.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	receipt := `{"tag":"` + tag + `","arch":"` + runtime.GOARCH + `","archive_sha256":"` + strings.Repeat("a", 64) + `","state_schema":7}`
	if err := os.WriteFile(filepath.Join(dir, ".ftw-release.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeTrialCommitsOnlyAfterReadiness(t *testing.T) {
	root := t.TempDir()
	current, next := "v0.131.0-beta.1", "v0.131.0-beta.2"
	nativeFixtureRelease(t, root, current)
	nativeFixtureRelease(t, root, next)
	m := nativeupdate.Manager{Root: root}
	if err := m.Init(current); err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(next); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Select(); err != nil {
		t.Fatal(err)
	}
	if _, err := beginNativeTrial(root, next, current); err == nil {
		t.Fatal("wrong binary identity accepted")
	}
	trial, err := beginNativeTrial(root, next, next)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Read()
	if err != nil || state.Current != current {
		t.Fatalf("trial committed before readiness: %+v %v", state, err)
	}
	checker := selfupdate.New(selfupdate.Config{CurrentVersion: next, NativeRoot: root,
		StatusPath: filepath.Join(root, "update-status.json")}, nil)
	started := time.Now().Add(-time.Minute)
	if err := checker.WriteStatus(selfupdate.UpdateStatus{State: "restarting", Target: next,
		Action: "update", StartedAt: started, PhaseStartedAt: started.Add(50 * time.Second),
		Step: 3, TotalSteps: 3, Message: "Starting the new Core once",
		Phases: []selfupdate.PhaseRecord{{Step: 1, TotalSteps: 3, Message: "Downloading verified Core release", Bytes: 24_000_000}}}); err != nil {
		t.Fatal(err)
	}
	if err := trial.complete(checker); err != nil {
		t.Fatal(err)
	}
	state, err = m.Read()
	if err != nil || state.Current != next || state.Previous != current || checker.Status().State != "done" {
		t.Fatalf("trial did not commit: %+v %v status=%+v", state, err, checker.Status())
	}
	phases := checker.Status().Phases
	if len(phases) != 2 || phases[1].Message != "Starting the new Core once" || phases[1].Step != 3 ||
		phases[1].FinishedAt.Sub(phases[1].StartedAt) < 9*time.Second {
		t.Fatalf("the start of the new Core is not recorded as a phase: %+v", phases)
	}
}

func TestNativeFallbackReportsFailedTrial(t *testing.T) {
	root := t.TempDir()
	current, next := "v0.131.0-beta.1", "v0.131.0-beta.2"
	nativeFixtureRelease(t, root, current)
	nativeFixtureRelease(t, root, next)
	m := nativeupdate.Manager{Root: root}
	if err := m.Init(current); err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(next); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Select(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Select(); err != nil {
		t.Fatal(err)
	}
	checker := selfupdate.New(selfupdate.Config{CurrentVersion: current, NativeRoot: root,
		StatusPath: filepath.Join(root, "update-status.json")}, nil)
	if err := checker.WriteStatus(selfupdate.UpdateStatus{State: "restarting", Target: next,
		Action: "update", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	reconcileNativeFallback(root, current, checker)
	if status := checker.Status(); status.State != "failed" || !strings.Contains(status.Message, "previous Core") {
		t.Fatalf("fallback status: %+v", status)
	}
}

func TestNativeRestartReportsCompletionAfterBoot(t *testing.T) {
	root := t.TempDir()
	current := "v0.131.0-beta.1"
	nativeFixtureRelease(t, root, current)
	if err := (nativeupdate.Manager{Root: root}).Init(current); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	checker := selfupdate.New(selfupdate.Config{CurrentVersion: current, NativeRoot: root,
		StatusPath:    filepath.Join(root, "update-status.json"),
		NativeRestart: func() error { restarts++; return nil }}, nil)
	if err := checker.TriggerRestart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := checker.Status(); status.State != "restarting" || status.Action != "restart" || restarts != 1 {
		t.Fatalf("restart not staged: %+v restarts=%d", status, restarts)
	}
	reconcileNativeFallback(root, current, checker)
	if status := checker.Status(); status.State != "done" || status.Step != status.TotalSteps {
		t.Fatalf("restart did not complete: %+v", status)
	}
}
