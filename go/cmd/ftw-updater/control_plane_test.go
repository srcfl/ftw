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
	s.handleUpdate(rr, httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.4"}`)))
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
	s.launchTransaction = func(target string, previous map[string]string, startedAt time.Time) error {
		s.imageRef = func(_ context.Context, service string) (string, error) {
			if service == updaterServiceName {
				return canonicalUpdaterImage + ":" + target, nil
			}
			return canonicalMainImage + ":v1.2.2", nil
		}
		s.runControlPlaneTransaction(target, previous, startedAt)
		return nil
	}

	rr := httptest.NewRecorder()
	s.handleUpdate(rr, httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.3"}`)))
	state := waitForState(t, s, "failed")
	if !strings.Contains(state.Message, "previous Core/updater pair restored") || !strings.Contains(state.Message, "runs ghcr.io/srcfl/ftw:v1.2.2") {
		t.Fatalf("mixed pair result = %+v", state)
	}
}
