package nativeupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func release(t *testing.T, root, tag string) {
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
	receipt := `{"tag":"` + tag + `","arch":"` + runtime.GOARCH + `","archive_sha256":"` + string(makeDigest('a')) + `","state_schema":7}`
	if err := os.WriteFile(filepath.Join(base, receiptFile), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeDigest(char byte) []byte {
	digest := make([]byte, 64)
	for i := range digest {
		digest[i] = char
	}
	return digest
}

func TestTrialCommitAndRollback(t *testing.T) {
	root := t.TempDir()
	manager := Manager{Root: root}
	release(t, root, "v3.8.0-beta.1")
	release(t, root, "v0.131.0-beta.1")
	if err := manager.Init("v3.8.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare("v0.131.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	_, tag, trial, err := manager.Select()
	if err != nil || tag != "v0.131.0-beta.1" || !trial {
		t.Fatalf("trial selection: %q %t %v", tag, trial, err)
	}
	if err := manager.Commit(tag); err != nil {
		t.Fatal(err)
	}
	state, err := manager.Read()
	if err != nil || state.Current != tag || state.Previous != "v3.8.0-beta.1" || state.Trial != "" {
		t.Fatalf("committed slots: %+v %v", state, err)
	}
	if _, err := manager.PrepareRollback(); err != nil {
		t.Fatal(err)
	}
	_, tag, trial, err = manager.Select()
	if err != nil || tag != "v3.8.0-beta.1" || !trial {
		t.Fatalf("rollback trial: %q %t %v", tag, trial, err)
	}
	if err := manager.Commit(tag); err != nil {
		t.Fatal(err)
	}
	state, err = manager.Read()
	if err != nil || state.Current != "v3.8.0-beta.1" || state.Previous != "v0.131.0-beta.1" {
		t.Fatalf("rolled back slots: %+v %v", state, err)
	}
}

func TestUncommittedTrialFallsBackAfterRestart(t *testing.T) {
	root := t.TempDir()
	manager := Manager{Root: root}
	release(t, root, "v3.8.0-beta.1")
	release(t, root, "v0.131.0-beta.1")
	if err := manager.Init("v3.8.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare("v0.131.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.Select(); err != nil {
		t.Fatal(err)
	}
	_, tag, trial, err := manager.Select()
	if err != nil || tag != "v3.8.0-beta.1" || trial {
		t.Fatalf("fallback: %q %t %v", tag, trial, err)
	}
	state, err := manager.Read()
	if err != nil || state.Current != tag || state.LastFailed != "v0.131.0-beta.1" || state.Trial != "" {
		t.Fatalf("fallback slots: %+v %v", state, err)
	}
}

func TestMissingCandidateFallsBackWithoutLaunchingIt(t *testing.T) {
	root := t.TempDir()
	manager := Manager{Root: root}
	release(t, root, "v3.8.0-beta.1")
	release(t, root, "v0.131.0-beta.1")
	if err := manager.Init("v3.8.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare("v0.131.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "releases", "v0.131.0-beta.1", "ftw")); err != nil {
		t.Fatal(err)
	}
	_, tag, trial, err := manager.Select()
	if err != nil || tag != "v3.8.0-beta.1" || trial {
		t.Fatalf("missing candidate: %q %t %v", tag, trial, err)
	}
}

func TestCorruptStateAndTraversalFailClosed(t *testing.T) {
	root := t.TempDir()
	manager := Manager{Root: root}
	release(t, root, "v0.131.0-beta.1")
	if err := manager.Init("v0.131.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"current":"../../etc"}`, `{`, `{"current":"v0.131.0-beta.1","next":"../evil"}`} {
		if err := os.WriteFile(filepath.Join(root, stateFile), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := manager.Select(); err == nil {
			t.Fatalf("accepted corrupt state: %s", bad)
		}
	}
}

func TestPrepareRejectsStateSchemaChange(t *testing.T) {
	root := t.TempDir()
	m := Manager{Root: root}
	release(t, root, "v0.131.0-beta.1")
	release(t, root, "v0.132.0-beta.1")
	if err := m.Init("v0.131.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "releases", "v0.132.0-beta.1", receiptFile)
	data, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"state_schema":7`, `"state_schema":8`, 1))
	if err := os.WriteFile(other, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare("v0.132.0-beta.1"); err == nil {
		t.Fatal("schema-changing update became a trial")
	}
	state, err := m.Read()
	if err != nil || state.Next != "" || state.Current != "v0.131.0-beta.1" {
		t.Fatalf("slots changed after rejection: %+v %v", state, err)
	}
}

func TestTenUpdatesKeepOnlyCurrentAndPrevious(t *testing.T) {
	root := t.TempDir()
	m := Manager{Root: root}
	first := "v0.131.0-beta.1"
	release(t, root, first)
	if err := m.Init(first); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 11; i++ {
		tag := fmt.Sprintf("v0.131.0-beta.%d", i)
		release(t, root, tag)
		if err := m.Prepare(tag); err != nil {
			t.Fatal(err)
		}
		_, selected, trial, err := m.Select()
		if err != nil || selected != tag || !trial {
			t.Fatalf("trial %d: %s %t %v", i, selected, trial, err)
		}
		if err := m.Commit(tag); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Prune(); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "releases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "v0.131.0-beta.10" || entries[1].Name() != "v0.131.0-beta.11" {
		t.Fatalf("retained releases after ten updates: %v", entries)
	}
}
