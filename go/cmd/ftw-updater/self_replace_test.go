package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const composeWithUpdater = `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    volumes:
      - ./data:/app/data
  ftw-optimizer:
    image: ghcr.io/srcfl/ftw-optimizer:${FTW_OPTIMIZER_IMAGE_TAG:-latest}
  ftw-updater:
    image: ghcr.io/srcfl/ftw-updater:${FTW_UPDATER_IMAGE_TAG:-latest}
`

func TestReplaceUpdaterPullsThenStartsDetachedHelper(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	// A Core update is normally still holding its transient image pin when the
	// sidecar replacement starts. That file lives in this container's /tmp and
	// is deleted on return, so the helper must never be handed it. Real
	// hardware failed here with "no such file or directory".
	s.updateOverrideFile = "/tmp/ftw-compose-update-1611897995.yml"

	if err := s.replaceUpdater(context.Background(), "v1.12.0-beta.8"); err != nil {
		t.Fatalf("replaceUpdater: %v", err)
	}

	calls := runner.snapshot()
	envs := runner.envSnapshot()
	if len(calls) != 3 {
		t.Fatalf("want pull, rm, run; got %d calls: %v", len(calls), calls)
	}

	pull := strings.Join(calls[0], " ")
	if !strings.Contains(pull, "pull ftw-updater") {
		t.Errorf("first call should pull the updater service: %v", calls[0])
	}
	if len(envs[0]) != 1 || envs[0][0] != "FTW_UPDATER_IMAGE_TAG=v1.12.0-beta.8" {
		t.Errorf("pull must pin the sidecar tag, got env %v", envs[0])
	}

	if got := strings.Join(calls[1], " "); got != "rm -f "+selfReplaceContainerName {
		t.Errorf("second call should clear a previous helper, got %q", got)
	}

	run := strings.Join(calls[2], " ")
	projectDir := filepath.Dir(s.composeFile)
	for _, want := range []string{
		"run -d",
		"--name " + selfReplaceContainerName,
		"-v /var/run/docker.sock:/var/run/docker.sock",
		"-v " + projectDir + ":" + projectDir,
		"-w " + projectDir,
		"-e FTW_UPDATER_IMAGE_TAG=v1.12.0-beta.8",
		"--entrypoint sh",
		// The helper runs from the image this updater is running, which is on
		// disk by definition. Pulling a new one first would be a second thing
		// that can fail before anything is fixed.
		"sha256:current",
		"up -d --no-deps ftw-updater",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("helper command missing %q\ngot: %s", want, run)
		}
	}
	if strings.Contains(run, s.updateOverrideFile) {
		t.Errorf("helper was handed the updater-owned transient override; it cannot read it\ngot: %s", run)
	}
}

// Pull and recreate have to agree on the config, or they could resolve
// different images for the same service.
func TestReplaceUpdaterPullAndHelperResolveTheSameConfig(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	s.updateOverrideFile = "/tmp/ftw-compose-update-42.yml"

	if err := s.replaceUpdater(context.Background(), "v1.13.2-beta.1"); err != nil {
		t.Fatalf("replaceUpdater: %v", err)
	}
	calls := runner.snapshot()
	pull, helper := strings.Join(calls[0], " "), strings.Join(calls[2], " ")
	if strings.Contains(pull, s.updateOverrideFile) {
		t.Errorf("pull must not depend on the transient override either\ngot: %s", pull)
	}
	// Same -f list on both sides.
	files := "-f " + s.composeFile
	if !strings.Contains(pull, files) || !strings.Contains(helper, files) {
		t.Errorf("pull and helper disagree on compose files\npull:   %s\nhelper: %s", pull, helper)
	}
}

// A deployment that hard-codes the sidecar image in its own override — which is
// how the Pi is set up — must still resolve, because the helper recreates what
// the host files declare.
func TestUpdaterServiceResolvesThroughHostOverride(t *testing.T) {
	s, _ := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	override := filepath.Join(filepath.Dir(s.composeFile), "docker-compose.override.yml")
	writeCompose(t, override, `services:
  ftw-updater:
    image: ghcr.io/srcfl/ftw-updater:v1.10.0-beta.1
`)
	s.overrideFiles = []string{override}
	s.updateOverrideFile = "/tmp/ftw-compose-update-42.yml"

	got, err := s.updaterServiceName()
	if err != nil || got != "ftw-updater" {
		t.Fatalf("updaterServiceName() = %q, %v; want ftw-updater", got, err)
	}
}

// The safety argument for the whole feature: a site that cannot replace its
// updater keeps the Core update it already earned.
func TestCoreUpdateStaysDoneWhenUpdaterReplacementFails(t *testing.T) {
	s, _ := newTestServer(t)
	replaceCalls := 0
	s.selfReplace = func(string) error {
		replaceCalls++
		return errors.New("no updater service in compose")
	}

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.3"}`))
	s.handleUpdate(httptest.NewRecorder(), req)

	done := waitForState(t, s, "done")
	if done.State != "done" {
		t.Fatalf("failed sidecar replacement must not reopen a finished update: %+v", done)
	}
	if replaceCalls != 1 {
		t.Fatalf("selfReplace calls = %d, want 1", replaceCalls)
	}
}

func TestSelfReplaceSkipsRestartAndOptimizer(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"restart", `{"action":"restart"}`},
		{"optimizer", `{"action":"update","target":"v1.3.2","component":"optimizer"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			called := false
			s.selfReplace = func(string) error { called = true; return nil }

			req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(tc.body))
			s.handleUpdate(httptest.NewRecorder(), req)
			waitForState(t, s, "done")

			if called {
				t.Fatalf("%s must not replace the updater sidecar", tc.name)
			}
		})
	}
}

func TestUpdaterServiceNameNeedsExactlyOneMatch(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		s, _ := newTestServer(t)
		if _, err := s.updaterServiceName(); err == nil {
			t.Fatal("compose without an updater service must not resolve")
		}
	})

	t.Run("renamed service still resolves by image", func(t *testing.T) {
		s, _ := newTestServer(t)
		writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:latest
    volumes:
      - ./data:/app/data
  sidecar:
    image: ghcr.io/srcfl/ftw-updater:v1.11.0
`)
		got, err := s.updaterServiceName()
		if err != nil || got != "sidecar" {
			t.Fatalf("updaterServiceName() = %q, %v; want sidecar", got, err)
		}
	})

	t.Run("two candidates are ambiguous", func(t *testing.T) {
		s, _ := newTestServer(t)
		writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:latest
    volumes:
      - ./data:/app/data
  a:
    image: ghcr.io/srcfl/ftw-updater:v1
  b:
    image: ghcr.io/srcfl/ftw-updater:v2
`)
		if _, err := s.updaterServiceName(); err == nil {
			t.Fatal("two updater services must fail closed, not pick one")
		}
	})
}

func TestIsUpdaterImage(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  bool
	}{
		{"ghcr.io/srcfl/ftw-updater:latest", true},
		{"ghcr.io/srcfl/ftw-updater", true},
		// Compose images reach us unexpanded; the default's own colon must not
		// be mistaken for the tag separator.
		{"ghcr.io/srcfl/ftw-updater:${FTW_UPDATER_IMAGE_TAG:-latest}", true},
		{"ghcr.io/srcfl/ftw-updater@sha256:abc", true},
		{"registry.example.com:5000/team/ftw-updater:v1", true},
		{"ghcr.io/srcfl/ftw:latest", false},
		{"ghcr.io/srcfl/ftw-optimizer:latest", false},
		{"eclipse-mosquitto:2", false},
	} {
		if got := isUpdaterImage(tc.image); got != tc.want {
			t.Errorf("isUpdaterImage(%q) = %v, want %v", tc.image, got, tc.want)
		}
	}
}
