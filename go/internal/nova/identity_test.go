package nova

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestLoadOrCreate_RoundTrip covers the first-run + restart path:
// a fresh call generates + persists a key; a second call on the same
// path loads the identical public key.
func TestLoadOrCreate_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nova.key")
	id1, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if id1.PublicKeyHex() != id2.PublicKeyHex() {
		t.Fatal("public key changed on reload")
	}
	if len(id1.PublicKeyHex()) != 128 {
		t.Fatalf("pubkey hex must be 128 chars (64 bytes X||Y), got %d", len(id1.PublicKeyHex()))
	}
}

func TestLoadExistingIdentityMatchesWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nova.key")
	created, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExistingIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PublicKeyHex() != created.PublicKeyHex() ||
		!bytes.Equal(before, after) ||
		!os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("existing-key load changed or replaced the key")
	}
}

func TestLoadExistingIdentityMissingStateNeverMutates(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string) string
	}{
		{
			name: "parent",
			setup: func(_ *testing.T, root string) string {
				return filepath.Join(root, "missing", "keydir", "nova.key")
			},
		},
		{
			name: "keydir",
			setup: func(_ *testing.T, root string) string {
				return filepath.Join(root, "missing", "nova.key")
			},
		},
		{
			name: "key",
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "keydir")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "nova.key")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := test.setup(t, root)
			before := listTree(t, root)
			_, err := LoadExistingIdentity(path)
			if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("missing %s error = %v", test.name, err)
			}
			after := listTree(t, root)
			if !equalStrings(before, after) {
				t.Fatalf("missing load mutated tree: before=%v after=%v", before, after)
			}
		})
	}
}

func TestLoadExistingIdentityRejectsUnsafeFilesWithoutBlocking(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(target), path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo",
			mutate: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hardlink",
			mutate: func(t *testing.T, path string) {
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "mode",
			mutate: func(t *testing.T, path string) {
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nova.key")
			if _, err := LoadOrCreateIdentity(path); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, path)
			done := make(chan error, 1)
			go func() {
				_, err := LoadExistingIdentity(path)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unsafe existing key was accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("unsafe existing-key load blocked")
			}
		})
	}
}

func TestLoadExistingIdentityRejectsWrongOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("owner mutation needs root")
	}
	path := filepath.Join(t.TempDir(), "nova.key")
	if _, err := LoadOrCreateIdentity(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 1, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExistingIdentity(path); err == nil {
		t.Fatal("existing loader accepted the wrong key owner")
	}
}

func TestLoadExistingIdentityDetectsParentDirectoryAndKeySwaps(t *testing.T) {
	for _, target := range []string{"parent", "keydir", "key"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			keydir := filepath.Join(parent, "keydir")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(keydir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(keydir, "nova.key")
			if _, err := LoadOrCreateIdentity(path); err != nil {
				t.Fatal(err)
			}
			swap := func(path string, recreate func(string) error) func() error {
				return func() error {
					if err := os.Rename(path, path+".moved"); err != nil {
						return err
					}
					return recreate(path)
				}
			}
			hooks := existingIdentityHooks{}
			switch target {
			case "parent":
				hooks.afterParentOpen = swap(parent, func(path string) error {
					if err := os.Mkdir(path, 0o700); err != nil {
						return err
					}
					return os.Mkdir(filepath.Join(path, "keydir"), 0o700)
				})
			case "keydir":
				hooks.afterDirectoryOpen = swap(keydir, func(path string) error {
					return os.Mkdir(path, 0o700)
				})
			case "key":
				hooks.afterKeyOpen = swap(path, func(path string) error {
					return os.WriteFile(path, []byte("replacement"), 0o600)
				})
			}
			policy := testIdentityFilePolicy(t)
			if _, err := loadExistingIdentity(path, policy, hooks); err == nil {
				t.Fatalf("%s swap was accepted", target)
			}
		})
	}
}

func TestLoadExistingIdentityConcurrentMissingCreateIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	start := make(chan struct{})
	var loaded *Identity
	var loadErr error
	var created *Identity
	var createErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		loaded, loadErr = LoadExistingIdentity(path)
	}()
	go func() {
		defer wg.Done()
		<-start
		created, createErr = LoadOrCreateIdentity(path)
	}()
	close(start)
	wg.Wait()
	if createErr != nil {
		t.Fatal(createErr)
	}
	if loadErr == nil {
		if loaded.PublicKeyHex() != created.PublicKeyHex() {
			t.Fatal("concurrent existing loader returned a different key")
		}
	} else if !errors.Is(loadErr, fs.ErrNotExist) {
		t.Fatalf("concurrent missing load error = %v", loadErr)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("existing loader left temp entry %q", entry.Name())
		}
	}
}

func TestGuardedCreateRejectsReservedStateBeforeAnyKeyWrite(t *testing.T) {
	for _, reserved := range []string{
		"nova.key.home-link.state",
		"nova.key.home-link.json",
		".nova.key.home-link.state.tmp-install",
		".nova.key.home-link.json.tmp-transition",
	} {
		t.Run(reserved, func(t *testing.T) {
			keydir := t.TempDir()
			path := filepath.Join(keydir, "nova.key")
			policy := testIdentityFilePolicy(t)
			_, err := loadOrCreateIdentityGuarded(
				path,
				[]string{"nova.key.home-link.state", "nova.key.home-link.json"},
				func(*os.File) error { return nil },
				policy,
				defaultIdentityFileOps(),
				guardedIdentityHooks{
					afterAbsence: func() error {
						return os.WriteFile(filepath.Join(keydir, reserved), []byte("race"), 0o600)
					},
				},
			)
			if !errors.Is(err, ErrIdentityCreationBlocked) {
				t.Fatalf("guarded create error = %v", err)
			}
			entries, err := os.ReadDir(keydir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != reserved {
				t.Fatalf("guarded create wrote entries: %v", entryNames(entries))
			}
		})
	}
}

func TestGuardedCreateRejectsKeyDirectorySwapWithoutWritingEitherDirectory(t *testing.T) {
	root := t.TempDir()
	keydir := filepath.Join(root, "state")
	moved := filepath.Join(root, "validated-state")
	if err := os.Mkdir(keydir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(keydir, "nova.key")
	policy := testIdentityFilePolicy(t)
	_, err := loadOrCreateIdentityGuarded(
		path,
		[]string{"nova.key.home-link.state", "nova.key.home-link.json"},
		func(*os.File) error { return nil },
		policy,
		defaultIdentityFileOps(),
		guardedIdentityHooks{
			afterAbsence: func() error {
				if err := os.Rename(keydir, moved); err != nil {
					return err
				}
				if err := os.Mkdir(keydir, 0o700); err != nil {
					return err
				}
				return os.WriteFile(
					filepath.Join(keydir, "nova.key.home-link.state"),
					[]byte("race"),
					0o600,
				)
			},
		},
	)
	if err == nil {
		t.Fatal("guarded create accepted a swapped key directory")
	}
	for _, dir := range []string{moved, keydir} {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if entry.Name() == "nova.key" ||
				strings.HasPrefix(entry.Name(), ".nova.key.tmp-") {
				t.Fatalf("guarded create wrote %q in %s", entry.Name(), dir)
			}
		}
	}
}

func TestGuardedCreateRollsBackLinkTimeKeyDirectorySwap(t *testing.T) {
	for iteration := range 20 {
		t.Run(fmt.Sprintf("iteration-%02d", iteration), func(t *testing.T) {
			root := t.TempDir()
			keydir := filepath.Join(root, "state")
			moved := filepath.Join(root, "validated-state")
			if err := os.Mkdir(keydir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(keydir, "nova.key")
			policy := testIdentityFilePolicy(t)
			ops := defaultIdentityFileOps()
			defaultLink := ops.link
			ops.link = func(directory *os.Root, oldName, newName string) error {
				if err := os.Rename(keydir, moved); err != nil {
					return err
				}
				if err := os.Mkdir(keydir, 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(
					filepath.Join(keydir, "nova.key.home-link.state"),
					[]byte("race"),
					0o600,
				); err != nil {
					return err
				}
				return defaultLink(directory, oldName, newName)
			}
			identity, err := loadOrCreateIdentityGuarded(
				path,
				[]string{"nova.key.home-link.state", "nova.key.home-link.json"},
				func(*os.File) error { return nil },
				policy,
				ops,
				guardedIdentityHooks{},
			)
			if err == nil || identity != nil {
				t.Fatalf("link-time swap result: identity=%v error=%v", identity, err)
			}
			assertNoIdentityKeyArtifacts(t, moved)
			assertNoIdentityKeyArtifacts(t, keydir)
		})
	}
}

func TestGuardedCreateRollsBackLinkTimeSwapWithInstallTemp(t *testing.T) {
	for iteration := range 20 {
		t.Run(fmt.Sprintf("iteration-%02d", iteration), func(t *testing.T) {
			root := t.TempDir()
			keydir := filepath.Join(root, "state")
			moved := filepath.Join(root, "validated-state")
			if err := os.Mkdir(keydir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(keydir, "nova.key")
			policy := testIdentityFilePolicy(t)
			ops := defaultIdentityFileOps()
			defaultLink := ops.link
			defaultRemove := ops.remove
			defaultSync := ops.syncDirectory
			var installTemp string
			rollbackStarted := false
			rollbackSyncs := 0
			ops.link = func(directory *os.Root, oldName, newName string) error {
				if err := os.Rename(keydir, moved); err != nil {
					return err
				}
				if err := os.Mkdir(keydir, 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(
					filepath.Join(keydir, "nova.key.home-link.state"),
					[]byte("race"),
					0o600,
				); err != nil {
					return err
				}
				return defaultLink(directory, oldName, newName)
			}
			ops.remove = func(directory *os.Root, name string) error {
				if installTemp == "" &&
					strings.HasPrefix(name, ".nova.key.tmp-") {
					installTemp = name
					return nil
				}
				if name == installTemp {
					rollbackStarted = true
				}
				return defaultRemove(directory, name)
			}
			ops.syncDirectory = func(directory *os.File) error {
				if rollbackStarted {
					rollbackSyncs++
				}
				return defaultSync(directory)
			}
			identity, err := loadOrCreateIdentityGuarded(
				path,
				[]string{"nova.key.home-link.state", "nova.key.home-link.json"},
				func(*os.File) error { return nil },
				policy,
				ops,
				guardedIdentityHooks{},
			)
			if err == nil || identity != nil {
				t.Fatalf("link-time swap result: identity=%v error=%v", identity, err)
			}
			if installTemp == "" {
				t.Fatal("test did not retain the install temp")
			}
			if rollbackSyncs != 1 {
				t.Fatalf("rollback directory syncs = %d, want 1", rollbackSyncs)
			}
			assertNoIdentityKeyArtifacts(t, moved)
			assertNoIdentityKeyArtifacts(t, keydir)
			marker, readErr := os.ReadFile(
				filepath.Join(keydir, "nova.key.home-link.state"),
			)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(marker) != "race" {
				t.Fatalf("replacement marker = %q", marker)
			}
		})
	}
}

func TestRollbackInstalledIdentitySupportedLinkStates(t *testing.T) {
	for _, links := range []int{1, 2} {
		t.Run(fmt.Sprintf("nlink-%d", links), func(t *testing.T) {
			dir, identity, policy, path := newRollbackTestIdentity(t)
			tempPath := filepath.Join(filepath.Dir(path), ".nova.key.tmp-review")
			if links == 2 {
				if err := os.Link(path, tempPath); err != nil {
					t.Fatal(err)
				}
			}
			ops := defaultIdentityFileOps()
			defaultSync := ops.syncDirectory
			defaultStat := ops.stat
			syncs := 0
			var linkCounts []uint64
			ops.syncDirectory = func(directory *os.File) error {
				syncs++
				return defaultSync(directory)
			}
			ops.stat = func(file *os.File) (os.FileInfo, error) {
				info, err := defaultStat(file)
				if err == nil {
					count, countErr := identityStatUint(info, "Nlink")
					if countErr != nil {
						return nil, countErr
					}
					linkCounts = append(linkCounts, count)
				}
				return info, err
			}
			if err := rollbackInstalledIdentity(
				dir, filepath.Base(path), identity.priv, policy, ops,
			); err != nil {
				t.Fatal(err)
			}
			if syncs != 1 {
				t.Fatalf("directory syncs = %d, want 1", syncs)
			}
			var wantCounts []uint64
			if links == 1 {
				wantCounts = []uint64{1, 1, 1, 0, 0}
			} else {
				wantCounts = []uint64{2, 2, 2, 1, 1, 0, 0}
			}
			if !equalUint64s(linkCounts, wantCounts) {
				t.Fatalf("held-FD link counts = %v, want %v", linkCounts, wantCounts)
			}
			assertNoIdentityKeyArtifacts(t, filepath.Dir(path))
		})
	}
}

func TestRollbackInstalledIdentityRejectsUnexplainedLinks(t *testing.T) {
	for _, test := range []struct {
		name      string
		linkNames []string
		newTemp   string
	}{
		{
			name:      "multiple install links",
			linkNames: []string{".nova.key.tmp-one", ".nova.key.tmp-two"},
		},
		{
			name:      "unexplained hard link",
			linkNames: []string{"unexpected-hard-link"},
		},
		{
			name:    "unrelated install temp",
			newTemp: ".nova.key.tmp-other",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, identity, policy, path := newRollbackTestIdentity(t)
			keydir := filepath.Dir(path)
			for _, name := range test.linkNames {
				if err := os.Link(path, filepath.Join(keydir, name)); err != nil {
					t.Fatal(err)
				}
			}
			if test.newTemp != "" {
				if err := os.WriteFile(
					filepath.Join(keydir, test.newTemp), []byte("other"), 0o600,
				); err != nil {
					t.Fatal(err)
				}
			}
			ops := defaultIdentityFileOps()
			removes := 0
			syncs := 0
			ops.remove = func(*os.Root, string) error {
				removes++
				return nil
			}
			ops.syncDirectory = func(*os.File) error {
				syncs++
				return nil
			}
			if err := rollbackInstalledIdentity(
				dir, filepath.Base(path), identity.priv, policy, ops,
			); err == nil {
				t.Fatal("rollback accepted unexplained link state")
			}
			if removes != 0 || syncs != 0 {
				t.Fatalf("rollback mutated unsafe state: removes=%d syncs=%d", removes, syncs)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("rollback removed target from unsafe state: %v", err)
			}
		})
	}
}

func TestRollbackInstalledIdentityRejectsUnknownLinkDuringRemoval(t *testing.T) {
	for _, phase := range []string{"temp", "target"} {
		for _, location := range []string{"same directory", "other directory"} {
			t.Run(phase+"/"+location, func(t *testing.T) {
				dir, identity, policy, path := newRollbackTestIdentity(t)
				keydir := filepath.Dir(path)
				tempName := ".nova.key.tmp-review"
				tempPath := filepath.Join(keydir, tempName)
				if err := os.Link(path, tempPath); err != nil {
					t.Fatal(err)
				}
				unknownDir := keydir
				if location == "other directory" {
					unknownDir = filepath.Join(keydir, "other")
					if err := os.Mkdir(unknownDir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				unknownPath := filepath.Join(unknownDir, "unknown-key-link")
				ops := defaultIdentityFileOps()
				defaultRemove := ops.remove
				defaultSync := ops.syncDirectory
				syncs := 0
				injected := false
				ops.remove = func(directory *os.Root, name string) error {
					inject := phase == "temp" && name == tempName ||
						phase == "target" && name == filepath.Base(path)
					if inject {
						if err := os.Link(path, unknownPath); err != nil {
							return err
						}
						injected = true
					}
					return defaultRemove(directory, name)
				}
				ops.syncDirectory = func(directory *os.File) error {
					syncs++
					return defaultSync(directory)
				}
				err := rollbackInstalledIdentity(
					dir, filepath.Base(path), identity.priv, policy, ops,
				)
				if err == nil {
					t.Fatal("rollback accepted an unknown same-inode link")
				}
				if !injected {
					t.Fatal("test did not inject the unknown link")
				}
				if syncs != 1 {
					t.Fatalf("directory syncs = %d, want 1", syncs)
				}
				unknownInfo, statErr := os.Stat(unknownPath)
				if statErr != nil {
					t.Fatalf("unknown link = %v", statErr)
				}
				if phase == "temp" {
					targetInfo, targetErr := os.Stat(path)
					if targetErr != nil {
						t.Fatalf("target was removed after temp link mismatch: %v", targetErr)
					}
					if !os.SameFile(targetInfo, unknownInfo) {
						t.Fatal("unknown link is not the held key inode")
					}
				} else if _, targetErr := os.Stat(path); !errors.Is(targetErr, fs.ErrNotExist) {
					t.Fatalf("target state after target unlink = %v", targetErr)
				}
				if _, tempErr := os.Stat(tempPath); !errors.Is(tempErr, fs.ErrNotExist) {
					t.Fatalf("temp state after injected unlink = %v", tempErr)
				}
			})
		}
	}
}

func TestRollbackInstalledIdentityRemoveErrors(t *testing.T) {
	for _, failedName := range []string{".nova.key.tmp-review", "nova.key"} {
		t.Run(failedName, func(t *testing.T) {
			dir, identity, policy, path := newRollbackTestIdentity(t)
			tempName := ".nova.key.tmp-review"
			tempPath := filepath.Join(filepath.Dir(path), tempName)
			if err := os.Link(path, tempPath); err != nil {
				t.Fatal(err)
			}
			ops := defaultIdentityFileOps()
			defaultRemove := ops.remove
			defaultSync := ops.syncDirectory
			wantErr := errors.New("forced rollback remove failure")
			syncs := 0
			ops.remove = func(directory *os.Root, name string) error {
				if name == failedName {
					return wantErr
				}
				return defaultRemove(directory, name)
			}
			ops.syncDirectory = func(directory *os.File) error {
				syncs++
				return defaultSync(directory)
			}
			err := rollbackInstalledIdentity(
				dir, filepath.Base(path), identity.priv, policy, ops,
			)
			if !errors.Is(err, wantErr) {
				t.Fatalf("rollback remove error = %v", err)
			}
			if syncs != 1 {
				t.Fatalf("directory syncs = %d, want 1", syncs)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("target should remain after remove error: %v", err)
			}
			if failedName == tempName {
				if _, err := os.Stat(tempPath); err != nil {
					t.Fatalf("failed temp should remain for recovery: %v", err)
				}
			} else if _, err := os.Stat(tempPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("removed temp state = %v", err)
			}
		})
	}
}

func TestRollbackInstalledIdentityFstatErrors(t *testing.T) {
	for _, failAt := range []int{1, 4, 6, 7} {
		t.Run(fmt.Sprintf("call-%d", failAt), func(t *testing.T) {
			dir, identity, policy, path := newRollbackTestIdentity(t)
			tempPath := filepath.Join(filepath.Dir(path), ".nova.key.tmp-review")
			if err := os.Link(path, tempPath); err != nil {
				t.Fatal(err)
			}
			ops := defaultIdentityFileOps()
			defaultStat := ops.stat
			defaultSync := ops.syncDirectory
			wantErr := errors.New("forced rollback fstat failure")
			statCalls := 0
			syncs := 0
			ops.stat = func(file *os.File) (os.FileInfo, error) {
				statCalls++
				if statCalls == failAt {
					return nil, wantErr
				}
				return defaultStat(file)
			}
			ops.syncDirectory = func(directory *os.File) error {
				syncs++
				return defaultSync(directory)
			}
			err := rollbackInstalledIdentity(
				dir, filepath.Base(path), identity.priv, policy, ops,
			)
			if !errors.Is(err, wantErr) {
				t.Fatalf("rollback fstat error = %v", err)
			}
			if failAt == 1 {
				if syncs != 0 {
					t.Fatalf("initial fstat failure synced %d times", syncs)
				}
			} else if syncs != 1 {
				t.Fatalf("directory syncs = %d, want 1", syncs)
			}
		})
	}
}

func TestRollbackInstalledIdentityDirectorySyncError(t *testing.T) {
	dir, identity, policy, path := newRollbackTestIdentity(t)
	tempPath := filepath.Join(filepath.Dir(path), ".nova.key.tmp-review")
	if err := os.Link(path, tempPath); err != nil {
		t.Fatal(err)
	}
	ops := defaultIdentityFileOps()
	wantErr := errors.New("forced rollback sync failure")
	syncs := 0
	ops.syncDirectory = func(*os.File) error {
		syncs++
		return wantErr
	}
	err := rollbackInstalledIdentity(
		dir, filepath.Base(path), identity.priv, policy, ops,
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("rollback sync error = %v", err)
	}
	if syncs != 1 {
		t.Fatalf("directory syncs = %d, want 1", syncs)
	}
	assertNoIdentityKeyArtifacts(t, filepath.Dir(path))
}

func newRollbackTestIdentity(
	t *testing.T,
) (*identityDirectory, *Identity, identityFilePolicy, string) {
	t.Helper()
	keydir := t.TempDir()
	path := filepath.Join(keydir, "nova.key")
	identity, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := testIdentityFilePolicy(t)
	dir, err := openIdentityDirectoryPath(
		keydir, policy.directoryOwnerUID, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dir.close)
	return dir, identity, policy, path
}

func assertNoIdentityKeyArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "nova.key" ||
			strings.HasPrefix(entry.Name(), ".nova.key.tmp-") {
			t.Fatalf("guarded create left %q in %s", entry.Name(), dir)
		}
	}
}

func TestGuardedCreateRejectsInvalidReservedNamesWithoutMutation(t *testing.T) {
	keydir := t.TempDir()
	path := filepath.Join(keydir, "nova.key")
	for _, names := range [][]string{
		nil,
		{"nova.key"},
		{"../state"},
		{"state", "state"},
	} {
		_, err := LoadOrCreateIdentityGuarded(
			path, names, func(*os.File) error { return nil },
		)
		if err == nil {
			t.Fatalf("reserved names %q were accepted", names)
		}
	}
	entries, err := os.ReadDir(keydir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid guard call wrote entries: %v", entryNames(entries))
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, relative+":"+entry.Type().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestLoadOrCreateRejectsUnsafeExistingKey(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.key")
		if _, err := LoadOrCreateIdentity(target); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "nova.key")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateIdentity(path); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink key = %v", err)
		}
	})

	t.Run("special file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nova.key")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateIdentity(path); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("special key = %v", err)
		}
	})

	for _, mode := range []os.FileMode{0o640, 0o602} {
		t.Run("mode "+mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nova.key")
			if _, err := LoadOrCreateIdentity(path); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreateIdentity(path); err == nil || !strings.Contains(err.Error(), "mode") {
				t.Fatalf("unsafe key mode = %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || info.Mode().Perm() != mode {
				t.Fatal("unsafe key was changed instead of rejected")
			}
		})
	}

	t.Run("wrong owner", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nova.key")
		if _, err := LoadOrCreateIdentity(path); err != nil {
			t.Fatal(err)
		}
		policy := testIdentityFilePolicy(t)
		policy.fileOwnerUID++
		if _, err := loadOrCreateIdentity(path, policy, defaultIdentityFileOps()); err == nil || !strings.Contains(err.Error(), "key owner") {
			t.Fatalf("wrong key owner = %v", err)
		}
	})

	t.Run("hard link", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nova.key")
		if _, err := LoadOrCreateIdentity(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path, path+".copy"); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateIdentity(path); err == nil || !strings.Contains(err.Error(), "2 links") {
			t.Fatalf("hard-linked key = %v", err)
		}
	})

	t.Run("untrusted restore bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nova.key")
		want := []byte("not a private key\n")
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateIdentity(path); err == nil || !strings.Contains(err.Error(), "no PEM block") {
			t.Fatalf("invalid restored key = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("invalid restored key was changed")
		}
	})
}

func TestLoadOrCreateRejectsUnsafeKeyDirectory(t *testing.T) {
	t.Run("writable parent", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "parent")
		dir := filepath.Join(parent, "state")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o770); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "nova.key")
		if _, err := LoadOrCreateIdentity(path); err == nil || !strings.Contains(err.Error(), "directory mode") {
			t.Fatalf("unsafe key directory parent = %v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("key created under unsafe parent: %v", err)
		}
	})

	t.Run("writable mode", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o770); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "nova.key")
		if _, err := LoadOrCreateIdentity(path); err == nil || !strings.Contains(err.Error(), "directory mode") {
			t.Fatalf("unsafe key directory = %v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("key created in unsafe directory: %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "state")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "linked-state")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateIdentity(filepath.Join(link, "nova.key")); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("symlink key directory = %v", err)
		}
	})

	t.Run("wrong owner", func(t *testing.T) {
		dir := t.TempDir()
		policy := testIdentityFilePolicy(t)
		policy.directoryOwnerUID++
		if _, err := loadOrCreateIdentity(filepath.Join(dir, "nova.key"), policy, defaultIdentityFileOps()); err == nil || !strings.Contains(err.Error(), "directory owner") {
			t.Fatalf("wrong directory owner = %v", err)
		}
	})
}

func TestIdentityOpenDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	file, err := openIdentityFileNoFollow(link)
	if file != nil {
		file.Close()
	}
	if err == nil {
		t.Fatal("no-follow open accepted a symlink")
	}
}

func TestLoadOrCreateRejectsKeyDirectorySwap(t *testing.T) {
	for _, phase := range []string{"parent barrier", "link"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "state")
			moved := filepath.Join(root, "validated-state")
			attacker := filepath.Join(root, "attacker-state")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(attacker, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "nova.key")
			policy := testIdentityFilePolicy(t)
			ops := defaultIdentityFileOps()
			swapped := false
			swap := func() error {
				if swapped {
					return nil
				}
				swapped = true
				if err := os.Rename(dir, moved); err != nil {
					return err
				}
				return os.Symlink(attacker, dir)
			}
			if phase == "parent barrier" {
				defaultSync := ops.syncDirectory
				ops.syncDirectory = func(file *os.File) error {
					if err := swap(); err != nil {
						return err
					}
					return defaultSync(file)
				}
			} else {
				defaultLink := ops.link
				ops.link = func(directory *os.Root, oldName, newName string) error {
					if err := swap(); err != nil {
						return err
					}
					return defaultLink(directory, oldName, newName)
				}
			}

			_, err := loadOrCreateIdentity(path, policy, ops)
			if err == nil || !strings.Contains(err.Error(), "key directory entry changed") {
				t.Fatalf("directory swap = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(attacker, "nova.key")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("key followed replacement directory: %v", err)
			}
			if phase == "parent barrier" {
				if _, err := os.Lstat(filepath.Join(moved, "nova.key")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("key written after failed revalidation: %v", err)
				}
			} else if _, err := os.Lstat(filepath.Join(moved, "nova.key")); err != nil {
				t.Fatalf("dirfd-bound install missing from validated directory: %v", err)
			}
		})
	}
}

func TestLoadOrCreateConcurrentFirstWriterWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	const callers = 16
	start := make(chan struct{})
	identities := make(chan *Identity, callers)
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			identity, err := LoadOrCreateIdentity(path)
			identities <- identity
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(identities)
	close(errorsSeen)

	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	persisted, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	for identity := range identities {
		if identity == nil || identity.PublicKeyHex() != persisted.PublicKeyHex() {
			t.Fatal("concurrent creator returned a key that did not persist")
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "nova.key" {
		t.Fatalf("identity creation left unexpected files: %v", entries)
	}
}

func TestIdentityInstallDurabilityOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	policy := testIdentityFilePolicy(t)
	ops := defaultIdentityFileOps()
	var events []string
	ops.link = func(directory *os.Root, oldName, newName string) error {
		events = append(events, "link")
		return directory.Link(oldName, newName)
	}
	ops.remove = func(directory *os.Root, name string) error {
		events = append(events, "remove")
		return directory.Remove(name)
	}
	ops.syncDirectory = func(directory *os.File) error {
		events = append(events, "sync")
		return directory.Sync()
	}
	if _, err := loadOrCreateIdentity(path, policy, ops); err != nil {
		t.Fatal(err)
	}
	wantSuffix := []string{"link", "sync", "remove", "sync"}
	if len(events) < len(wantSuffix) || !equalStrings(events[len(events)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("install order = %v, want suffix %v", events, wantSuffix)
	}
}

func TestIdentityInstallSyncFailuresKeepOnePersistentKey(t *testing.T) {
	for _, test := range []struct {
		name   string
		failAt int
		phase  string
	}{
		{"after link", 2, "sync installed key link"},
		{"after cleanup", 3, "sync installed key cleanup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nova.key")
			policy := testIdentityFilePolicy(t)
			ops := defaultIdentityFileOps()
			calls := 0
			wantErr := errors.New("forced sync failure")
			ops.syncDirectory = func(directory *os.File) error {
				calls++
				if calls == test.failAt {
					return wantErr
				}
				return directory.Sync()
			}
			if _, err := loadOrCreateIdentity(path, policy, ops); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), test.phase) {
				t.Fatalf("sync failure = %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreateIdentity(path); err != nil {
				t.Fatalf("retry after sync failure: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("retry replaced the key left by a sync failure")
			}
		})
	}
}

func TestIdentityRetryRequiresDurabilityBarrier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	policy := testIdentityFilePolicy(t)
	initialOps := defaultIdentityFileOps()
	calls := 0
	wantErr := errors.New("forced sync failure")
	initialOps.syncDirectory = func(directory *os.File) error {
		calls++
		if calls >= 2 {
			return wantErr
		}
		return directory.Sync()
	}
	if _, err := loadOrCreateIdentity(path, policy, initialOps); !errors.Is(err, wantErr) {
		t.Fatalf("initial sync failures = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	retryOps := defaultIdentityFileOps()
	retryOps.syncDirectory = func(*os.File) error { return wantErr }
	if _, err := loadOrCreateIdentity(path, policy, retryOps); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "sync existing key") {
		t.Fatalf("retry without barrier = %v", err)
	}
	if _, err := LoadOrCreateIdentity(path); err != nil {
		t.Fatalf("retry with barrier: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("retry after failed barriers replaced the key")
	}
}

func TestIdentityCleanupFailureLeavesUnusableLinkedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	policy := testIdentityFilePolicy(t)
	policy.barrierTimeout = 0
	ops := defaultIdentityFileOps()
	wantErr := errors.New("forced cleanup failure")
	ops.remove = func(*os.Root, string) error { return wantErr }
	if _, err := loadOrCreateIdentity(path, policy, ops); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "remove installed key temp file") {
		t.Fatalf("cleanup failure = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateIdentity(path, policy, defaultIdentityFileOps()); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("linked install state = %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nova.key.tmp-") {
			if err := os.Remove(filepath.Join(filepath.Dir(path), entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := syncTestDirectory(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(path); err != nil {
		t.Fatalf("load after durable cleanup: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("cleanup recovery replaced the installed key")
	}
}

func TestIdentityInstallEEXISTPerformsOwnBarriers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	if _, err := LoadOrCreateIdentity(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := testIdentityFilePolicy(t)
	ops := defaultIdentityFileOps()
	syncCalls := 0
	ops.syncDirectory = func(directory *os.File) error {
		syncCalls++
		return directory.Sync()
	}
	directory, _, err := openOrCreateIdentityDirectory(filepath.Dir(path), policy, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.close()
	installed, err := installIdentityFileNoReplace(directory, filepath.Base(path), []byte("losing key"), 0o600, policy, ops)
	if err != nil {
		t.Fatal(err)
	}
	if installed || syncCalls != 2 {
		t.Fatalf("EEXIST result: installed=%t sync_calls=%d, want false/2", installed, syncCalls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("EEXIST loser changed the persisted key")
	}
}

func TestIdentityDirectoryCreationSyncsParent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	path := filepath.Join(dir, "nova.key")
	policy := testIdentityFilePolicy(t)
	ops := defaultIdentityFileOps()
	var synced []string
	ops.syncDirectory = func(directory *os.File) error {
		info, err := directory.Stat()
		if err != nil {
			return err
		}
		rootInfo, err := os.Stat(root)
		if err != nil {
			return err
		}
		if os.SameFile(info, rootInfo) {
			synced = append(synced, "root")
		} else {
			synced = append(synced, "other")
		}
		return directory.Sync()
	}
	if _, err := loadOrCreateIdentity(path, policy, ops); err != nil {
		t.Fatal(err)
	}
	if len(synced) == 0 || synced[0] != "root" {
		t.Fatalf("first directory sync = %v, want root parent", synced)
	}
}

func TestIdentityDirectoryCreationRequiresExistingParent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "first", "second", "nova.key")
	if _, err := LoadOrCreateIdentity(path); err == nil || !strings.Contains(err.Error(), "parent must exist") {
		t.Fatalf("two missing directory levels = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "first")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial directory tree created: %v", err)
	}
}

func TestIdentityDirectoryCreationBarrierFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		prepare    func(string) error
		directory  func(string) string
		parentName string
	}{
		{
			name:       "root to first",
			prepare:    func(string) error { return nil },
			directory:  func(root string) string { return filepath.Join(root, "first") },
			parentName: "root",
		},
		{
			name: "first to second",
			prepare: func(root string) error {
				return os.Mkdir(filepath.Join(root, "first"), 0o700)
			},
			directory:  func(root string) string { return filepath.Join(root, "first", "second") },
			parentName: "first",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := test.prepare(root); err != nil {
				t.Fatal(err)
			}
			dir := test.directory(root)
			path := filepath.Join(dir, "nova.key")
			policy := testIdentityFilePolicy(t)
			ops := defaultIdentityFileOps()
			wantErr := errors.New("forced parent barrier failure")
			ops.syncDirectory = func(*os.File) error { return wantErr }
			if _, err := loadOrCreateIdentity(path, policy, ops); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "sync new key directory parent") {
				t.Fatalf("%s barrier = %v", test.parentName, err)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("created directory missing after barrier error: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("key written after parent barrier error: %v", err)
			}
			if _, err := LoadOrCreateIdentity(path); err != nil {
				t.Fatalf("retry after %s barrier: %v", test.parentName, err)
			}
		})
	}
}

func TestIdentityCreationFailsClosedWithoutHardLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	policy := testIdentityFilePolicy(t)
	ops := defaultIdentityFileOps()
	ops.link = func(*os.Root, string, string) error { return syscall.EOPNOTSUPP }
	if _, err := loadOrCreateIdentity(path, policy, ops); err == nil || !strings.Contains(err.Error(), "without replace") {
		t.Fatalf("unsupported hard links = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback key was created: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed install left temporary key files: %v", entries)
	}
}

func testIdentityFilePolicy(t *testing.T) identityFilePolicy {
	t.Helper()
	policy, err := currentIdentityFilePolicy()
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func syncTestDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalUint64s(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSignRawHex_VerifiesAsNovaDoes reproduces Nova's
// verifyES256Signature exactly to confirm the wire format is
// byte-compatible (64-byte R||S hex, SHA-256 of message).
func TestSignRawHex_VerifiesAsNovaDoes(t *testing.T) {
	id, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "k.pem"))
	if err != nil {
		t.Fatal(err)
	}
	msg := "idt-op123|nonce-abc|1713610245|gw-f42w-1"
	sigHex, err := id.SignRawHex(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sigHex) != 128 {
		t.Fatalf("signature must be 128 hex chars (64 bytes R||S), got %d", len(sigHex))
	}

	// Decode pub + sig exactly as Nova's ownership.verifyES256Signature does.
	pubBytes, err := hex.DecodeString(id.PublicKeyHex())
	if err != nil || len(pubBytes) != 64 {
		t.Fatalf("pubkey decode: err=%v len=%d", err, len(pubBytes))
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) != 64 {
		t.Fatalf("sig decode: err=%v len=%d", err, len(sigBytes))
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(pubBytes[:32]),
		Y:     new(big.Int).SetBytes(pubBytes[32:]),
	}
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])
	hash := sha256.Sum256([]byte(msg))
	if !ecdsa.Verify(pub, hash[:], r, s) {
		t.Fatal("signature did not verify with Nova's verification recipe")
	}
}

// TestSignJWT_FormatMatchesAuthCallout confirms the JWT has three
// base64url segments, ES256 header with device claim, and verifies
// against the identity's own public key.
func TestSignJWT_FormatMatchesAuthCallout(t *testing.T) {
	id, _ := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "k.pem"))
	const serial = "f42w-gw-abc"
	tok, err := id.SignJWT(serial, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 parts, got %d", len(parts))
	}

	// Header checks
	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var hdr map[string]string
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr["alg"] != "ES256" {
		t.Fatalf("alg: got %s want ES256", hdr["alg"])
	}
	if hdr["typ"] != "JWT" {
		t.Fatalf("typ: got %s want JWT", hdr["typ"])
	}
	if hdr["device"] != serial {
		t.Fatalf("device claim: got %s want %s", hdr["device"], serial)
	}

	// Payload has iat, exp, jti
	payloadBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var payload map[string]any
	_ = json.Unmarshal(payloadBytes, &payload)
	for _, k := range []string{"iat", "exp", "jti"} {
		if _, ok := payload[k]; !ok {
			t.Fatalf("payload missing %q: %v", k, payload)
		}
	}

	// Verify signature against our own pubkey (mirrors auth-callout's recipe).
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("sig decode: err=%v len=%d", err, len(sig))
	}
	pubBytes, _ := hex.DecodeString(id.PublicKeyHex())
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(pubBytes[:32]),
		Y:     new(big.Int).SetBytes(pubBytes[32:]),
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], r, s) {
		t.Fatal("JWT signature did not verify")
	}
}
