package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The .env belongs to the operator. An update records two tags in it and must
// leave everything else exactly as it found it.
func TestMergeEnvFilePreservesEverythingElse(t *testing.T) {
	existing := `# Site settings — do not reformat
COMPOSE_PROJECT_NAME=myhome

# pinned by hand last winter
FTW_IMAGE_TAG=v1.10.0
export FTW_OPTIMIZER_IMAGE_TAG=v1.3.2
SOME_SECRET=a=b=c
`
	got := mergeEnvFile(existing, map[string]string{
		mainTagEnv:    "v1.13.3-beta.1",
		updaterTagEnv: "v1.13.3-beta.1",
	})

	for _, want := range []string{
		"# Site settings — do not reformat",
		"COMPOSE_PROJECT_NAME=myhome",
		"# pinned by hand last winter",
		"export FTW_OPTIMIZER_IMAGE_TAG=v1.3.2",
		"SOME_SECRET=a=b=c",
		"FTW_IMAGE_TAG=v1.13.3-beta.1",
		"FTW_UPDATER_IMAGE_TAG=v1.13.3-beta.1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("merged .env lost or missed %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "FTW_IMAGE_TAG=v1.10.0") {
		t.Errorf("stale pin survived:\n%s", got)
	}
	// Rewritten in place, not appended alongside the old line.
	if n := strings.Count(got, "\nFTW_IMAGE_TAG="); n != 1 {
		t.Errorf("FTW_IMAGE_TAG assigned %d times, want exactly 1\n%s", n, got)
	}
	if n := strings.Count(got, "FTW_UPDATER_IMAGE_TAG="); n != 1 {
		t.Errorf("FTW_UPDATER_IMAGE_TAG assigned %d times, want exactly 1\n%s", n, got)
	}
	// The rewritten pin should stay where the operator had it, under its comment.
	if strings.Index(got, "# pinned by hand last winter") > strings.Index(got, "\nFTW_IMAGE_TAG=") {
		t.Errorf("rewrite moved the key away from its comment\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("merged .env must end with a newline")
	}
}

func TestMergeEnvFileCreatesFromNothing(t *testing.T) {
	got := mergeEnvFile("", map[string]string{mainTagEnv: "v1.0.0", updaterTagEnv: "v1.0.0"})
	if got != "FTW_IMAGE_TAG=v1.0.0\nFTW_UPDATER_IMAGE_TAG=v1.0.0\n" {
		t.Fatalf("new .env = %q", got)
	}
	// Order is fixed so repeated updates do not reshuffle the file.
	if got != mergeEnvFile("", map[string]string{updaterTagEnv: "v1.0.0", mainTagEnv: "v1.0.0"}) {
		t.Error("output depends on map iteration order")
	}
}

func TestEnvPinScriptWritesAtomicallyInPlace(t *testing.T) {
	script := envPinScript("/srv/ftw", "FTW_IMAGE_TAG=v1.2.3\n")
	if !strings.Contains(script, "base64 -d") {
		t.Error("payload should be decoded, not interpolated as shell text")
	}
	// Temp file beside the target: a rename across filesystems is not atomic.
	if !strings.Contains(script, "/srv/ftw/.env.ftw-update-tmp") || !strings.Contains(script, "mv") {
		t.Errorf("expected an atomic write next to the target\n%s", script)
	}
	if !strings.Contains(script, "'/srv/ftw/.env'") {
		t.Errorf("target path should be quoted\n%s", script)
	}
	// A failed write must say so rather than pass silently.
	if !strings.Contains(script, ">&2") {
		t.Errorf("failure should reach stderr\n%s", script)
	}

	start := strings.Index(script, "'") + 1
	end := strings.Index(script[start:], "'") + start
	decoded, err := base64.StdEncoding.DecodeString(script[start:end])
	if err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
	if string(decoded) != "FTW_IMAGE_TAG=v1.2.3\n" {
		t.Fatalf("payload = %q", decoded)
	}
}

// A quote or newline in a tag must not be able to escape the helper's sh -c.
func TestEnvPinScriptIsInertToShellMetacharacters(t *testing.T) {
	script := envPinScript("/srv/ftw", "FTW_IMAGE_TAG='; rm -rf / #\n")
	if strings.Contains(script, "rm -rf") {
		t.Fatalf("payload leaked into the script text:\n%s", script)
	}
}

func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	if got, err := readEnvFile(dir); err != nil || got != "" {
		t.Fatalf("missing .env should be empty and fine, got %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readEnvFile(dir); err != nil || got != "A=1\n" {
		t.Fatalf("readEnvFile = %q, %v", got, err)
	}
}

// The constants here must track componentSpec, or the updater would write a
// variable the compose file does not read.
func TestTagEnvConstantsMatchComponentSpecs(t *testing.T) {
	s, _ := newTestServer(t)
	core, err := s.componentSpec("core")
	if err != nil {
		t.Fatal(err)
	}
	if core.tagEnv != mainTagEnv {
		t.Errorf("core tagEnv = %q, mainTagEnv = %q", core.tagEnv, mainTagEnv)
	}
}

func TestHelperPersistsTagsBeforeRecreating(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	dir := filepath.Dir(s.composeFile)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("COMPOSE_PROJECT_NAME=myhome\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.replaceUpdater(context.Background(), "v1.13.3-beta.1"); err != nil {
		t.Fatalf("replaceUpdater: %v", err)
	}
	run := strings.Join(runner.snapshot()[2], " ")

	if !strings.Contains(run, "-v "+dir+":"+dir+" ") {
		t.Errorf("helper needs the project writable to persist .env\ngot: %s", run)
	}
	if strings.Contains(run, dir+":"+dir+":ro") {
		t.Errorf("project is still mounted read-only\ngot: %s", run)
	}
	// Write the pin first, then recreate: the new sidecar should come up with
	// the file already correct.
	pinAt, upAt := strings.Index(run, "base64 -d"), strings.Index(run, "up -d --no-deps")
	if pinAt < 0 || upAt < 0 || pinAt > upAt {
		t.Errorf("expected the .env write before the recreate\ngot: %s", run)
	}

	start := strings.Index(run, "printf %s '") + len("printf %s '")
	end := strings.Index(run[start:], "'") + start
	decoded, err := base64.StdEncoding.DecodeString(run[start:end])
	if err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
	for _, want := range []string{
		"COMPOSE_PROJECT_NAME=myhome",
		"FTW_IMAGE_TAG=v1.13.3-beta.1",
		"FTW_UPDATER_IMAGE_TAG=v1.13.3-beta.1",
	} {
		if !strings.Contains(string(decoded), want) {
			t.Errorf("persisted .env missing %q\ngot:\n%s", want, decoded)
		}
	}
}

// An unreadable .env must not cost the site its update.
func TestUnreadableEnvStillReplacesTheUpdater(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	// A directory where the file should be: readable path, unreadable content.
	if err := os.Mkdir(filepath.Join(filepath.Dir(s.composeFile), ".env"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := s.replaceUpdater(context.Background(), "v1.13.3-beta.1"); err != nil {
		t.Fatalf("replaceUpdater must still run: %v", err)
	}
	run := strings.Join(runner.snapshot()[2], " ")
	if strings.Contains(run, "base64 -d") {
		t.Errorf("must not overwrite an .env it could not read\ngot: %s", run)
	}
	if !strings.Contains(run, "up -d --no-deps ftw-updater") {
		t.Errorf("the recreate itself must still happen\ngot: %s", run)
	}
}
