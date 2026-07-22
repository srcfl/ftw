package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatusReportsControlPlaneContract(t *testing.T) {
	oldVersion := Version
	Version = "v1.10.0-beta.1"
	t.Cleanup(func() { Version = oldVersion })
	s, _ := newTestServer(t)

	rr := httptest.NewRecorder()
	s.handleStatus(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
	var got struct {
		ProtocolVersion int      `json:"protocol_version"`
		UpdaterVersion  string   `json:"updater_version"`
		Capabilities    []string `json:"capabilities"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != updaterProtocolVersion || got.UpdaterVersion != Version ||
		len(got.Capabilities) != 1 || got.Capabilities[0] != controlPlaneCapability {
		t.Fatalf("status contract = %+v", got)
	}
}

func TestTransactingStateRejectsAnotherUpdate(t *testing.T) {
	s, runner := newTestServer(t)
	s.writeState(State{State: "transacting", Action: "update", Component: "core", Target: "v1.2.3"})
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(controlPlaneUpdateJSON("v1.2.4"))))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("second update ran Docker: %v", calls)
	}
}

func TestControlPlaneDoesNotCommitMixedImageRefs(t *testing.T) {
	s, _ := newTestServer(t)
	s.skipPull = true
	s.imageID = func(_ context.Context, service string) (string, error) {
		return "sha256:previous-" + service, nil
	}
	s.launchTransaction = func(release controlPlaneRelease, previous map[string]string, startedAt time.Time, transactionID string) error {
		s.imageRef = func(_ context.Context, service string) (string, error) {
			if service == updaterServiceName {
				return release.updaterRef(), nil
			}
			return canonicalMainImage + ":v1.2.2", nil
		}
		s.runControlPlaneTransaction(release, previous, startedAt, transactionID)
		return nil
	}

	rr := httptest.NewRecorder()
	s.handleUpdate(rr, httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(controlPlaneUpdateJSON("v1.2.3"))))
	state := waitForState(t, s, "failed")
	if !strings.Contains(state.Message, "previous Core/updater pair restored") || !strings.Contains(state.Message, "runs ghcr.io/srcfl/ftw:v1.2.2") {
		t.Fatalf("mixed pair result = %+v", state)
	}
}

func TestCoreUpdateRequiresVerifiedDigests(t *testing.T) {
	s, runner := newTestServer(t)
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.3"}`)))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid control-plane release") {
		t.Fatalf("missing digests = %d %q, want 400", rr.Code, rr.Body.String())
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("invalid request ran Docker: %v", calls)
	}
}

func TestControlPlaneOverridePinsVerifiedDigests(t *testing.T) {
	s, _ := newTestServer(t)
	release := testControlPlaneRelease("v1.2.3")
	cleanup, err := s.prepareControlPlaneDigestPins(release)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := s.validateControlPlaneDigestPins(release); err != nil {
		t.Fatal(err)
	}
	for service, want := range map[string]string{
		s.mainServiceName: release.coreRef(), updaterServiceName: release.updaterRef(),
	} {
		got, ok, err := serviceImageFromComposeFiles(s.composeFiles(), service)
		if err != nil || !ok || got != want {
			t.Fatalf("%s image = %q, %v, %v; want %q", service, got, ok, err, want)
		}
	}
}

func TestControlPlaneHelperCarriesTransactionFenceAndDigests(t *testing.T) {
	s, runner := newTestServer(t)
	release := testControlPlaneRelease("v1.2.3")
	previous := map[string]string{"core": "sha256:core-old", "updater": "sha256:updater-old"}
	startedAt := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	if err := s.launchControlPlaneHelper("updater-container", previous["updater"], false, release, previous, startedAt, testControlPlaneTransactionID); err != nil {
		t.Fatal(err)
	}
	calls := runner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("helper calls = %v", calls)
	}
	call := strings.Join(calls[0], " ")
	for _, want := range []string{
		"--name " + controlPlaneHelperName(false, testControlPlaneTransactionID),
		previous["updater"],
		"-transaction-id " + testControlPlaneTransactionID,
		"-release-revision " + release.Revision,
		"-core-digest " + release.CoreDigest,
		"-updater-digest " + release.UpdaterDigest,
	} {
		if !strings.Contains(call, want) {
			t.Fatalf("helper call %q does not contain %q", call, want)
		}
	}
}

func TestControlPlaneRejectsMovedTagDigestAtExecution(t *testing.T) {
	s, _ := newTestServer(t)
	s.skipPull = true
	s.imageID = func(_ context.Context, service string) (string, error) {
		return "sha256:previous-" + service, nil
	}
	s.launchTransaction = func(release controlPlaneRelease, previous map[string]string, startedAt time.Time, transactionID string) error {
		s.imageRef = func(_ context.Context, service string) (string, error) {
			if service == updaterServiceName {
				return release.updaterRef(), nil
			}
			return canonicalMainImage + "@sha256:" + strings.Repeat("c", 64), nil
		}
		s.runControlPlaneTransaction(release, previous, startedAt, transactionID)
		return nil
	}

	rr := httptest.NewRecorder()
	s.handleUpdate(rr, httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(controlPlaneUpdateJSON("v1.2.3"))))
	state := waitForState(t, s, "failed")
	if !strings.Contains(state.Message, "previous Core/updater pair restored") ||
		!strings.Contains(state.Message, testControlPlaneRelease("v1.2.3").CoreDigest) {
		t.Fatalf("moved-tag result = %+v", state)
	}
}

func TestFreshDetachedHelperLeaseIsNotRecovered(t *testing.T) {
	s, runner := newTestServer(t)
	now := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	release := testControlPlaneRelease("v1.2.3")
	s.writeState(State{
		State: "transacting", Action: "update", Component: "core", Target: release.Target,
		StartedAt: now.Add(-time.Minute), UpdatedAt: now,
		PreviousImages:  map[string]string{"core": "sha256:core-old", "updater": "sha256:updater-old"},
		ReleaseRevision: release.Revision, CoreDigest: release.CoreDigest, UpdaterDigest: release.UpdaterDigest,
		TransactionID: testControlPlaneTransactionID,
	})
	launches := 0
	s.launchRecovery = func(State) error { launches++; return nil }
	if monitor := s.recoverCrashedState(); !monitor {
		t.Fatal("fresh detached-helper lease should start a monitor")
	}
	if launches != 0 || s.readState().State != "transacting" || len(runner.snapshot()) != 0 {
		t.Fatalf("fresh helper was raced: launches=%d state=%+v calls=%v", launches, s.readState(), runner.snapshot())
	}
}

func TestStoppedHelperHeartbeatRestoresUpdaterThenCore(t *testing.T) {
	s, runner := newTestServer(t)
	now := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	release := testControlPlaneRelease("v1.2.3")
	previous := map[string]string{"core": "sha256:core-old", "updater": "sha256:updater-old"}
	s.writeState(State{
		State: "transacting", Action: "update", Component: "core", Target: release.Target,
		StartedAt: now.Add(-time.Minute), UpdatedAt: now, PreviousImages: previous,
		ReleaseRevision: release.Revision, CoreDigest: release.CoreDigest, UpdaterDigest: release.UpdaterDigest,
		TransactionID: testControlPlaneTransactionID,
	})
	s.launchRecovery = func(st State) error {
		s.runControlPlaneRecovery(st)
		return nil
	}
	if monitor := s.recoverCrashedState(); !monitor {
		t.Fatal("fresh helper should be monitored before its lease expires")
	}
	now = now.Add(controlPlaneStaleAfter + time.Second)
	if recovered := s.recoverStaleControlPlaneTransaction(); !recovered {
		t.Fatal("stale helper was not recovered")
	}
	state := s.readState()
	if state.State != "failed" || strings.Contains(state.State, "done") || !strings.Contains(state.Message, "previous Core/updater pair restored") {
		t.Fatalf("recovery final state = %+v", state)
	}
	if state.PreviousImages["core"] != previous["core"] || state.PreviousImages["updater"] != previous["updater"] {
		t.Fatalf("recovery lost rollback IDs: %v", state.PreviousImages)
	}
	calls := runner.snapshot()
	if len(calls) != 5 || strings.Join(calls[0], " ") != "rm -f "+controlPlaneHelperName(false, testControlPlaneTransactionID) ||
		!strings.Contains(strings.Join(calls[1], " "), previous["updater"]) ||
		calls[2][len(calls[2])-1] != updaterServiceName ||
		!strings.Contains(strings.Join(calls[3], " "), previous["core"]) ||
		calls[4][len(calls[4])-1] != s.mainServiceName {
		t.Fatalf("recovery was not updater-first: %v", calls)
	}
}

func TestHostRestartAfterUpdaterReplacementLaunchesRecoveryHelper(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Date(2026, 7, 22, 8, 10, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	release := testControlPlaneRelease("v1.2.3")
	previous := map[string]string{"core": "sha256:core-old", "updater": "sha256:updater-old"}
	s.writeState(State{
		State: "transacting", Action: "update", Component: "core", Target: release.Target,
		Message: "matching updater ready; replacing Core", StartedAt: now.Add(-10 * time.Minute),
		UpdatedAt: now.Add(-controlPlaneStaleAfter - time.Second), PreviousImages: previous,
		ReleaseRevision: release.Revision, CoreDigest: release.CoreDigest, UpdaterDigest: release.UpdaterDigest,
		TransactionID: testControlPlaneTransactionID,
	})
	var recovered State
	s.launchRecovery = func(st State) error { recovered = st; return nil }
	if monitor := s.recoverCrashedState(); monitor {
		t.Fatal("stale host-restart state should recover before serving")
	}
	if recovered.State != "restoring" || recovered.PreviousImages["core"] != previous["core"] || recovered.PreviousImages["updater"] != previous["updater"] {
		t.Fatalf("recovery helper input = %+v", recovered)
	}
	if state := s.readState(); state.State != "restoring" || strings.Contains(state.State, "done") {
		t.Fatalf("host-restart recovery state = %+v", state)
	}
}

func TestStoppedRecoveryHelperIsFencedBeforeRelaunch(t *testing.T) {
	s, runner := newTestServer(t)
	now := time.Date(2026, 7, 22, 8, 20, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	release := testControlPlaneRelease("v1.2.3")
	s.writeState(State{
		State: "restoring", Action: "update", Component: "core", Target: release.Target,
		StartedAt: now.Add(-20 * time.Minute), UpdatedAt: now.Add(-controlPlaneStaleAfter - time.Second),
		PreviousImages:  map[string]string{"core": "sha256:core-old", "updater": "sha256:updater-old"},
		ReleaseRevision: release.Revision, CoreDigest: release.CoreDigest, UpdaterDigest: release.UpdaterDigest,
		TransactionID: testControlPlaneTransactionID,
	})
	launches := 0
	s.launchRecovery = func(State) error { launches++; return nil }
	if recovered := s.recoverStaleControlPlaneTransaction(); !recovered {
		t.Fatal("stale recovery helper was not reclaimed")
	}
	calls := runner.snapshot()
	if launches != 1 || len(calls) != 1 || strings.Join(calls[0], " ") != "rm -f "+controlPlaneHelperName(true, testControlPlaneTransactionID) {
		t.Fatalf("recovery helper fence: launches=%d calls=%v", launches, calls)
	}
}
