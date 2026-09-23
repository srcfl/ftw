package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func imageRemovals(calls [][]string) []string {
	var refs []string
	for _, call := range calls {
		if len(call) == 3 && call[0] == "image" && call[1] == "rm" {
			refs = append(refs, call[2])
		}
	}
	return refs
}

// After a verified Core update the updater removes the images it replaced,
// but keeps every image a container uses and the one it would roll back to
// (#1305).
func TestHandleUpdate_RemovesReplacedImages(t *testing.T) {
	s, runner := newTestServer(t)
	s.listImages = func(_ context.Context, repository string) ([]imageTag, error) {
		switch repository {
		case canonicalMainImage:
			return []imageTag{
				{ID: "sha256:new", Ref: "ghcr.io/srcfl/ftw:v1.2.3"},
				{ID: "sha256:current", Ref: "ghcr.io/srcfl/ftw:v1.2.2"},
				{ID: "sha256:old", Ref: "ghcr.io/srcfl/ftw:v1.2.1"},
				{ID: "sha256:untagged", Ref: "sha256:untagged"},
			}, nil
		case canonicalUpdaterImage:
			return []imageTag{
				{ID: "sha256:updater-running", Ref: "ghcr.io/srcfl/ftw-updater:v1.2.2"},
				{ID: "sha256:updater-old", Ref: "ghcr.io/srcfl/ftw-updater:v1.2.1"},
			}, nil
		}
		return nil, nil
	}
	s.usedImageIDs = func(context.Context) (map[string]bool, error) {
		return map[string]bool{"sha256:new": true, "sha256:updater-running": true}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/update", bytes.NewBufferString(`{"action":"update","target":"v1.2.3"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d", rr.Code)
	}
	st := waitForState(t, s, "done")
	if st.PreviousImageID != "sha256:current" {
		t.Fatalf("rollback image = %q", st.PreviousImageID)
	}

	calls := runner.snapshot()
	got := imageRemovals(calls)
	want := []string{"ghcr.io/srcfl/ftw:v1.2.1", "sha256:untagged", "ghcr.io/srcfl/ftw-updater:v1.2.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed images = %v, want %v", got, want)
	}
	// Cleanup runs only after the service is up and the update is done.
	upIndex, firstRemoval := -1, -1
	for i, call := range calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "up -d") && upIndex < 0 {
			upIndex = i
		}
		if call[0] == "image" && firstRemoval < 0 {
			firstRemoval = i
		}
	}
	if upIndex < 0 || firstRemoval < upIndex {
		t.Fatalf("image removal must follow compose up: %v", calls)
	}
}

// A failed or impossible cleanup must never turn a finished update into a
// failure, and the sidecar must still be brought to the same release.
func TestHandleUpdate_ImageCleanupFailureKeepsUpdateDone(t *testing.T) {
	s, runner := newTestServer(t)
	s.listImages = func(context.Context, string) ([]imageTag, error) {
		return []imageTag{{ID: "sha256:old", Ref: "ghcr.io/srcfl/ftw:v1.2.1"}}, nil
	}
	s.usedImageIDs = func(context.Context) (map[string]bool, error) {
		return nil, errors.New("docker ps unavailable")
	}
	replaced := ""
	s.selfReplace = func(target string) error { replaced = target; return nil }

	req := httptest.NewRequest(http.MethodPost, "/update", bytes.NewBufferString(`{"action":"update","target":"v1.2.3"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	waitForState(t, s, "done")
	if got := imageRemovals(runner.snapshot()); len(got) != 0 {
		t.Fatalf("cleanup removed images without a container inventory: %v", got)
	}
	if replaced != "v1.2.3" {
		t.Fatalf("updater self-replace target = %q", replaced)
	}
}

// Restarts and rollbacks replace nothing, so they clean nothing.
func TestHandleUpdate_RestartDoesNotRemoveImages(t *testing.T) {
	s, runner := newTestServer(t)
	s.listImages = func(context.Context, string) ([]imageTag, error) {
		return []imageTag{{ID: "sha256:old", Ref: "ghcr.io/srcfl/ftw:v1.2.1"}}, nil
	}
	s.usedImageIDs = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	req := httptest.NewRequest(http.MethodPost, "/update", bytes.NewBufferString(`{"action":"restart"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	waitForState(t, s, "done")
	if got := imageRemovals(runner.snapshot()); len(got) != 0 {
		t.Fatalf("restart removed images: %v", got)
	}
}

func TestParseImageTagsKeepsIDsAndNamesUntaggedByID(t *testing.T) {
	out := "sha256:aaa\tghcr.io/srcfl/ftw:v1.2.3\n" +
		"sha256:bbb\tghcr.io/srcfl/ftw:<none>\n" +
		"WARN[0000] some compose noise\n" +
		"\n"
	got := parseImageTags(out)
	want := []imageTag{
		{ID: "sha256:aaa", Ref: "ghcr.io/srcfl/ftw:v1.2.3"},
		{ID: "sha256:bbb", Ref: "sha256:bbb"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %+v, want %+v", got, want)
	}
}
