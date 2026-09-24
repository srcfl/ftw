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

func TestUnknownCommandsDoNotStartCore(t *testing.T) {
	root := t.TempDir()
	testRelease(t, root, "v0.131.0")
	if err := (nativeupdate.Manager{Root: root}).Init("v0.131.0"); err != nil {
		t.Fatal(err)
	}
	started := false
	for _, args := range [][]string{{"rolback"}, {"start"}, {"--help"}} {
		err := run(root, "/data/config.yaml", "/data/drivers", args, func(string, []string, []string) error {
			started = true
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "unknown command") || !strings.Contains(err.Error(), "usage:") || started {
			t.Fatalf("%v: err=%v started=%v", args, err, started)
		}
	}
	if err := run(root, "/data/config.yaml", "/data/drivers", nil, func(binary string, _ []string, _ []string) error {
		started = binary == filepath.Join(root, "releases", "v0.131.0", "ftw")
		return nil
	}); err != nil || !started {
		t.Fatalf("no arguments must start Core: %v %v", err, started)
	}
}

func TestRollbackCommandStagesThePreviousReleaseForTheNextStart(t *testing.T) {
	root := t.TempDir()
	manager := nativeupdate.Manager{Root: root}
	testRelease(t, root, "v0.131.0")
	testRelease(t, root, "v0.132.0")
	if err := manager.Init("v0.131.0"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare("v0.132.0"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.Select(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Commit("v0.132.0"); err != nil {
		t.Fatal(err)
	}
	noExec := func(string, []string, []string) error { t.Fatal("rollback must not start Core"); return nil }
	if err := run(root, "", "", []string{"rollback"}, noExec); err != nil {
		t.Fatal(err)
	}
	var started string
	if err := run(root, "/data/config.yaml", "/data/drivers", nil, func(binary string, _ []string, env []string) error {
		started = binary
		if !containsEnv(env, "FTW_NATIVE_TRIAL_TAG=v0.131.0") {
			t.Fatal("the previous release must start as a trial")
		}
		return nil
	}); err != nil || started != filepath.Join(root, "releases", "v0.131.0", "ftw") {
		t.Fatalf("after rollback started %q: %v", started, err)
	}
	if err := run(root, "", "", []string{"rollback"}, noExec); err == nil {
		t.Fatal("a second rollback while one is pending must fail")
	}
}
