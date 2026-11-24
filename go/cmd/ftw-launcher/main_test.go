package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
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

// damagedCurrentWithPendingUpdate leaves v0.132.0 current but damaged,
// v0.131.0 previous and healthy, and v0.133.0 waiting to start.
func damagedCurrentWithPendingUpdate(t *testing.T) (string, nativeupdate.Manager) {
	t.Helper()
	root := t.TempDir()
	manager := nativeupdate.Manager{Root: root}
	for _, tag := range []string{"v0.131.0", "v0.132.0", "v0.133.0"} {
		testRelease(t, root, tag)
	}
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
	if err := manager.Prepare("v0.133.0"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "releases", "v0.132.0", "web", "index.html")); err != nil {
		t.Fatal(err)
	}
	return root, manager
}

func TestTrialExecFailureWithDamagedCurrentRunsPrevious(t *testing.T) {
	root, manager := damagedCurrentWithPendingUpdate(t)
	var launched []string
	err := launch(root, "/data/config.yaml", "/data/drivers", nil, func(binary string, _ []string, env []string) error {
		launched = append(launched, binary)
		if strings.HasSuffix(binary, "v0.133.0/ftw") {
			return errors.New("cannot exec")
		}
		if !containsEnv(env, "FTW_NATIVE_TRIAL_TAG=v0.131.0") {
			t.Fatal("the fallback must start as a trial")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(launched) != 2 || !strings.HasSuffix(launched[0], "v0.133.0/ftw") || !strings.HasSuffix(launched[1], "v0.131.0/ftw") {
		t.Fatalf("launch order = %v", launched)
	}
	state, err := manager.Read()
	if err != nil || state.Current != "v0.132.0" || state.Trial != "v0.131.0" || state.LastFailed != "v0.133.0" {
		t.Fatalf("slots: %+v %v", state, err)
	}
}

func TestFallbackExecFailureIsRecordedAndNotRetried(t *testing.T) {
	root, manager := damagedCurrentWithPendingUpdate(t)
	var launched []string
	failAll := func(binary string, _ []string, _ []string) error {
		launched = append(launched, binary)
		return errors.New("cannot exec")
	}
	if err := launch(root, "/data/config.yaml", "/data/drivers", nil, failAll); err == nil {
		t.Fatal("want the fallback exec error")
	}
	if len(launched) != 2 || !strings.HasSuffix(launched[1], "v0.131.0/ftw") {
		t.Fatalf("launch order = %v", launched)
	}
	state, err := manager.Read()
	if err != nil || state.Trial != "" || state.LastFailed != "v0.131.0" {
		t.Fatalf("slots: %+v %v", state, err)
	}
	launched = nil
	if err := launch(root, "/data/config.yaml", "/data/drivers", nil, failAll); err == nil || len(launched) != 0 {
		t.Fatalf("second start err=%v launched=%v; want the damaged-release error", err, launched)
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
		err := run(root, "/data/config.yaml", "/data/drivers", "", args, func(string, []string, []string) error {
			started = true
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "unknown command") || !strings.Contains(err.Error(), "usage:") || started {
			t.Fatalf("%v: err=%v started=%v", args, err, started)
		}
	}
	if err := run(root, "/data/config.yaml", "/data/drivers", "", nil, func(binary string, _ []string, _ []string) error {
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
	if err := run(root, "", "", "", []string{"rollback"}, noExec); err != nil {
		t.Fatal(err)
	}
	var started string
	if err := run(root, "/data/config.yaml", "/data/drivers", "", nil, func(binary string, _ []string, env []string) error {
		started = binary
		if !containsEnv(env, "FTW_NATIVE_TRIAL_TAG=v0.131.0") {
			t.Fatal("the previous release must start as a trial")
		}
		return nil
	}); err != nil || started != filepath.Join(root, "releases", "v0.131.0", "ftw") {
		t.Fatalf("after rollback started %q: %v", started, err)
	}
	if err := run(root, "", "", "", []string{"rollback"}, noExec); err == nil {
		t.Fatal("a second rollback while one is pending must fail")
	}
}

// imageDir writes the package a container image carries, as the Dockerfile
// leaves it in /app/image.
func imageDir(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	version := `{"version":"` + tag + `","arch":"` + runtime.GOARCH + `","state_schema":7}`
	for _, f := range []struct{ name, data string }{
		{"ftw", "binary"}, {"web/index.html", "web"}, {"drivers/BUNDLED_SOURCE.json", "{}"},
		{"optimizer/native/bundle/manifest.json", "{}"}, {"release-version.json", version},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "ftw-linux-"+runtime.GOARCH+".tar.gz")
	sum := sha256.Sum256(buf.Bytes())
	for path, data := range map[string][]byte{
		archive:             buf.Bytes(),
		archive + ".sha256": []byte(hex.EncodeToString(sum[:]) + "  " + filepath.Base(archive) + "\n"),
		filepath.Join(dir, "release-version.json"): []byte(version),
	} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunWithImageStartsAnEmptyVolumeAndTakesANewerImage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opt", "ftw") // a container's first start: no root yet
	var started string
	var trial bool
	start := func(binary string, _ []string, env []string) error {
		started, trial = binary, containsEnv(env, "FTW_NATIVE_TRIAL_TAG="+filepath.Base(filepath.Dir(binary)))
		return nil
	}
	if err := run(root, "/data/config.yaml", "/data/drivers", imageDir(t, "v0.165.0-beta.1"), nil, start); err != nil {
		t.Fatal(err)
	}
	if started != filepath.Join(root, "releases", "v0.165.0-beta.1", "ftw") || trial {
		t.Fatalf("first start ran %s (trial %t)", started, trial)
	}
	// A rebuild with a newer FTW_VERSION starts it once as a trial.
	if err := run(root, "/data/config.yaml", "/data/drivers", imageDir(t, "v0.165.0-beta.2"), nil, start); err != nil {
		t.Fatal(err)
	}
	if started != filepath.Join(root, "releases", "v0.165.0-beta.2", "ftw") || !trial {
		t.Fatalf("newer image ran %s (trial %t)", started, trial)
	}
	// A trial that does not commit falls back, and the same image is not
	// tried again on the next start.
	for range 2 {
		if err := run(root, "/data/config.yaml", "/data/drivers", imageDir(t, "v0.165.0-beta.2"), nil, start); err != nil {
			t.Fatal(err)
		}
		if started != filepath.Join(root, "releases", "v0.165.0-beta.1", "ftw") || trial {
			t.Fatalf("after the failed trial ran %s (trial %t)", started, trial)
		}
	}
	state, err := (nativeupdate.Manager{Root: root}).Read()
	if err != nil || state.Current != "v0.165.0-beta.1" || state.LastFailed != "v0.165.0-beta.2" || state.Next != "" || state.Trial != "" {
		t.Fatalf("slots: %+v %v", state, err)
	}
}

func TestRunWithABrokenImageKeepsTheInstalledRelease(t *testing.T) {
	root := t.TempDir()
	testRelease(t, root, "v0.165.0")
	if err := (nativeupdate.Manager{Root: root}).Init("v0.165.0"); err != nil {
		t.Fatal(err)
	}
	broken := imageDir(t, "v0.166.0")
	if err := os.WriteFile(filepath.Join(broken, "ftw-linux-"+runtime.GOARCH+".tar.gz"), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	var started string
	if err := run(root, "/data/config.yaml", "/data/drivers", broken, nil, func(binary string, _ []string, _ []string) error {
		started = binary
		return nil
	}); err != nil || started != filepath.Join(root, "releases", "v0.165.0", "ftw") {
		t.Fatalf("started %q: %v", started, err)
	}
	// Without slots there is nothing to run, so the start fails.
	empty := t.TempDir()
	if err := run(empty, "/data/config.yaml", "/data/drivers", broken, nil, func(string, []string, []string) error {
		t.Fatal("started Core without a release")
		return nil
	}); err == nil || !strings.Contains(err.Error(), "image release") {
		t.Fatalf("empty slots with a broken image: %v", err)
	}
}
