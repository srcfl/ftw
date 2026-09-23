package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const composeWithUpdater = `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    environment:
      FTW_IMAGE_TAG: ${FTW_IMAGE_TAG:-}
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
		"'up' '-d' '--no-deps' 'ftw-updater'",
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
	pullFiles := "-f " + s.composeFile
	helperFiles := "'-f' " + shellQuote(s.composeFile)
	if !strings.Contains(pull, pullFiles) || !strings.Contains(helper, helperFiles) {
		t.Errorf("pull and helper disagree on compose files\npull:   %s\nhelper: %s", pull, helper)
	}
}

func TestReplaceUpdaterShellQuotesApostropheInProjectPath(t *testing.T) {
	s, runner := newTestServer(t)
	projectDir := filepath.Join(t.TempDir(), "Fred's FTW")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.composeFile = filepath.Join(projectDir, "docker-compose.yml")
	writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
  ftw-updater:
    image: ghcr.io/srcfl/ftw-updater:${FTW_UPDATER_IMAGE_TAG:-latest}
`)

	if err := s.replaceUpdater(context.Background(), "v2.0.0-beta.1"); err != nil {
		t.Fatalf("replaceUpdater: %v", err)
	}
	runArgs := runner.snapshot()[2]
	script := runArgs[len(runArgs)-1]
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("helper script is invalid for apostrophe path: %v\n%s\n%s", err, out, script)
	}
	if !strings.Contains(script, shellQuote(s.composeFile)) {
		t.Fatalf("compose path is not one quoted shell word:\n%s", script)
	}
	wantMount := projectDir + ":" + projectDir
	foundMount := false
	for _, arg := range runArgs {
		if arg == wantMount {
			foundMount = true
			break
		}
	}
	if !foundMount {
		t.Fatalf("detached helper lost exact bind argument %q: %v", wantMount, runArgs)
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

	// Terminal state precedes sidecar replacement. Wait for the whole job,
	// not merely "done", before checking the callback and its failure result.
	s.runJob("update", "v1.2.3")
	done := s.readState()
	if done.State != "done" {
		t.Fatalf("failed sidecar replacement must not reopen a finished update: %+v", done)
	}
	if replaceCalls != 1 {
		t.Fatalf("selfReplace calls = %d, want 1", replaceCalls)
	}
}

func TestSelfReplaceSkipsRestart(t *testing.T) {
	s, _ := newTestServer(t)
	called := false
	s.selfReplace = func(string) error { called = true; return nil }
	s.runJob("restart", "")
	if done := s.readState(); done.State != "done" {
		t.Fatalf("restart did not finish: %+v", done)
	}
	if called {
		t.Fatal("restart must not replace the updater sidecar")
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
		{legacyUpdaterImage + ":v2.16.0-beta.1", true},
		{legacyUpdaterImage + ":${FTW_UPDATER_IMAGE_TAG:-latest}", true},
		{legacyUpdaterImage + "@sha256:abc", true},
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

func TestBetaUpdateReplacesUpdaterWithTheSameCandidate(t *testing.T) {
	s, _ := newTestServer(t)
	healthy := false
	s.healthCheck = func(_ context.Context, service string) error {
		if service == canonicalMainServiceName {
			healthy = true
		}
		return nil
	}
	var replacement string
	s.selfReplace = func(target string) error {
		if !healthy || s.readState().State != "done" {
			t.Error("updater replacement preceded healthy Core")
		}
		replacement = target
		return nil
	}
	s.runJob("update", "v2.15.0-beta.1")
	if replacement != "v2.15.0-beta.1" {
		t.Fatalf("updater replacement = %q", replacement)
	}
}
