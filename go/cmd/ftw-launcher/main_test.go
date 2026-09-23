package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/nativeupdate"
)

func testRelease(t *testing.T, root, tag string) {
	t.Helper()
	base := filepath.Join(root, "releases", tag)
	for _, name := range []string{"web", "drivers", "optimizer/native/bundle"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ftw", "web/index.html", "drivers/BUNDLED_SOURCE.json", "optimizer/native/bundle/manifest.json"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte("test"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	receipt := `{"tag":"` + tag + `","arch":"` + runtime.GOARCH + `","archive_sha256":"` + strings.Repeat("a", 64) + `","state_schema":7}`
	if err := os.WriteFile(filepath.Join(base, ".ftw-release.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTrialExecFailureRunsCurrentRelease(t *testing.T) {
	root := t.TempDir()
	manager := nativeupdate.Manager{Root: root}
	testRelease(t, root, "v3.8.0-beta.1")
	testRelease(t, root, "v0.131.0-beta.1")
	if err := manager.Init("v3.8.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare("v0.131.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	var launched []string
	err := launch(root, "/data/config.yaml", "/data/drivers", nil, func(binary string, args, env []string) error {
		launched = append(launched, binary)
		if len(launched) == 1 {
			if !containsEnv(env, "FTW_NATIVE_TRIAL_TAG=v0.131.0-beta.1") {
				t.Fatal("trial tag not passed to Core")
			}
			if !strings.Contains(strings.Join(args, " "), filepath.Join(root, "releases", "v0.131.0-beta.1", "web")) {
				t.Fatal("trial did not use its own web assets")
			}
			return errors.New("cannot exec")
		}
		if containsEnv(env, "FTW_NATIVE_TRIAL_TAG=v0.131.0-beta.1") {
			t.Fatal("fallback inherited the trial tag")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(launched) != 2 || !strings.HasSuffix(launched[0], "v0.131.0-beta.1/ftw") || !strings.HasSuffix(launched[1], "v3.8.0-beta.1/ftw") {
		t.Fatalf("launch order = %v", launched)
	}
	state, err := manager.Read()
	if err != nil || state.Current != "v3.8.0-beta.1" || state.LastFailed != "v0.131.0-beta.1" {
		t.Fatalf("fallback slots: %+v %v", state, err)
	}
}

func containsEnv(env []string, value string) bool {
	for _, item := range env {
		if item == value {
			return true
		}
	}
	return false
}
