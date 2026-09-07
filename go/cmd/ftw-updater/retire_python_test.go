package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRetirePythonPreservesCoreAndCustomServices(t *testing.T) {
	for _, style := range []string{"mapping", "sequence"} {
		t.Run(style, func(t *testing.T) {
			env := "      FTW_OPTIMIZER_SOCKET: /run/ftw-optimizer/optimizer.sock\n      KEEP: value"
			deps := "      ftw-optimizer: {condition: service_healthy}\n      mqtt: {condition: service_started}"
			if style == "sequence" {
				env = "      - FTW_OPTIMIZER_SOCKET=/run/ftw-optimizer/optimizer.sock\n      - KEEP=value"
				deps = "      - ftw-optimizer\n      - mqtt"
			}
			input := []byte(`services:
  ` + legacyMainServiceName + `:
    image: ghcr.io/srcfl/ftw:v2.15.2-beta.1
    environment:
` + env + `
    depends_on:
` + deps + `
    volumes:
      - ./data:/app/data
      - optimizer-ipc:/run/ftw-optimizer
      - type: volume
        source: optimizer-ipc
        target: /run/ftw-optimizer
  ftw-optimizer:
    image: retired:latest
  mqtt:
    image: eclipse-mosquitto:2
volumes:
  optimizer-ipc:
`)
			out, changed, err := retiredPythonCompose(input)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			for _, want := range []string{"./data:/app/data", "ghcr.io/srcfl/ftw:v2.15.2-beta.1", "KEEP", "eclipse-mosquitto:2"} {
				if !strings.Contains(string(out), want) {
					t.Fatalf("lost %s: %s", want, out)
				}
			}
			if strings.Contains(string(out), "ftw-optimizer") || strings.Contains(string(out), "FTW_OPTIMIZER_") {
				t.Fatalf("retired wiring remains: %s", out)
			}
			var doc any
			if err := yaml.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			again, changed, err := retiredPythonCompose(out)
			if err != nil || changed || string(again) != string(out) {
				t.Fatalf("not idempotent: %v %v", changed, err)
			}
		})
	}
}

func TestUpdaterRejectsRetiredOptimizer(t *testing.T) {
	s, _ := newTestServer(t)
	if _, err := s.componentSpec("optimizer"); err == nil {
		t.Fatal("optimizer still updatable")
	}
}

func TestRetirePythonHelperUsesLocalImageAndWritableProject(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, "services:\n  renamed-updater:\n    image: mirror.example/team/ftw-updater:v2.16.0-beta.1\n")
	s.imageID = func(_ context.Context, service string) (string, error) {
		if service != "renamed-updater" {
			t.Fatalf("looked up %q instead of the installed updater", service)
		}
		return "sha256:current", nil
	}
	t.Setenv("COMPOSE_PROJECT_NAME", "existing-site")
	if err := s.retirePythonViaHelper(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := runner.snapshot()[0]
	joined := strings.Join(call, " ")
	for _, want := range []string{
		"--pull never", "--network none", "sha256:current", "FTW_RETIRE_PYTHON_HELPER=1",
		"COMPOSE_PROJECT_NAME=existing-site", filepath.Dir(s.composeFile) + ":" + filepath.Dir(s.composeFile) + ":rw",
		"-compose " + s.composeFile, "-main-service " + s.mainServiceName,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

func TestRetirePythonRemovesOrphanWithCleanCompose(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, "services:\n  ftw:\n    image: ftw:test\n")
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *"/api/components"*) printf '%s\n' '{"optimizer":{"bundled_with_core":true,"healthy":true}}' ;;
  "compose "*" ps -q --all ftw") printf '%s\n' 'abcdef012345' ;;
  "inspect "*" abcdef012345") printf '%s\n' 'existing-site' ;;
  "ps --all --quiet --filter label=com.docker.compose.project=existing-site --filter label=com.docker.compose.service=ftw-optimizer") printf '%s\n' 'orphan-id' ;;
  *) exit 31 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := s.retirePythonOptimizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := runner.snapshot()
	if len(calls) != 2 || strings.Join(calls[1], " ") != "rm --force orphan-id" {
		t.Fatalf("orphan was not removed: %v", calls)
	}
}
