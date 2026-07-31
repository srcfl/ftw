package gatewayidentity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryBindingStorage struct {
	mu             sync.Mutex
	files          map[string][]byte
	installErrors  map[string][]error
	installWinners map[string]map[string][]byte
	afterInstall   map[string][]error
	replaceErrors  map[string][]error
	afterReplace   map[string][]error
	revalidateErrs []error
}

func newMemoryBindingStorage(key []byte) *memoryBindingStorage {
	return &memoryBindingStorage{
		files:          map[string][]byte{"nova.key": append([]byte(nil), key...)},
		installErrors:  make(map[string][]error),
		installWinners: make(map[string]map[string][]byte),
		afterInstall:   make(map[string][]error),
		replaceErrors:  make(map[string][]error),
		afterReplace:   make(map[string][]error),
	}
}

func (s *memoryBindingStorage) Lock() error { return nil }

func (s *memoryBindingStorage) Read(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (s *memoryBindingStorage) InstallNoReplace(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := popError(s.installErrors, name); err != nil {
		return err
	}
	if winner, ok := s.installWinners[name]; ok {
		delete(s.installWinners, name)
		for winnerName, winnerData := range winner {
			s.files[winnerName] = append([]byte(nil), winnerData...)
		}
		return fs.ErrExist
	}
	if _, ok := s.files[name]; ok {
		return fs.ErrExist
	}
	s.files[name] = append([]byte(nil), data...)
	return popError(s.afterInstall, name)
}

func (s *memoryBindingStorage) ReplaceExact(name string, oldData, newData []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := popError(s.replaceErrors, name); err != nil {
		return err
	}
	current, ok := s.files[name]
	if !ok {
		return fs.ErrNotExist
	}
	if bytes.Equal(current, newData) {
		return nil
	}
	if !bytes.Equal(current, oldData) {
		return ErrBindingMismatch
	}
	s.files[name] = append([]byte(nil), newData...)
	return popError(s.afterReplace, name)
}

func (s *memoryBindingStorage) Revalidate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.revalidateErrs) == 0 {
		return nil
	}
	err := s.revalidateErrs[0]
	s.revalidateErrs = s.revalidateErrs[1:]
	return err
}

func (s *memoryBindingStorage) Close() error { return nil }

func popError(values map[string][]error, key string) error {
	list := values[key]
	if len(list) == 0 {
		return nil
	}
	err := list[0]
	values[key] = list[1:]
	return err
}

func testBindingPaths() BindingPaths {
	return BindingPaths{
		Key:     "/data/nova.key",
		Sidecar: "/data/nova.key.home-link.json",
		Marker:  "/data/nova.key.home-link.state",
	}
}

func adoptSoftwareBinding(
	ctx context.Context,
	store bindingStorage,
	paths BindingPaths,
	resolve func(context.Context) (StableInterface, error),
) (SoftwareBinding, error) {
	preview, err := previewSoftwareBinding(ctx, store, paths, resolve)
	if err != nil {
		if state, inspectErr := inspectBindingState(store, paths); inspectErr == nil && state.hasAny() {
			return finishFrozenBinding(store, paths, state)
		}
		return SoftwareBinding{}, err
	}
	return applySoftwareBinding(store, paths, preview)
}

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func testStableInterface(mac string) StableInterface {
	return StableInterface{
		Name: "enp1s0", Index: 7, PermanentMAC: mustMAC(mac),
	}
}

func TestSoftwareBindingAdoptionAndRestart(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	var resolves atomic.Int32
	resolve := func(context.Context) (StableInterface, error) {
		resolves.Add(1)
		return testStableInterface("00:11:22:33:44:55"), nil
	}
	got, err := adoptSoftwareBinding(
		context.Background(), store, testBindingPaths(), resolve,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.GatewayID != "012300112201334455" || got.Source != SourceSoftware {
		t.Fatalf("binding = %+v", got)
	}
	if resolves.Load() != 1 {
		t.Fatalf("route resolver calls = %d", resolves.Load())
	}
	reloaded, err := loadSoftwareBinding(store, testBindingPaths())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded != got {
		t.Fatalf("reloaded binding = %+v, want %+v", reloaded, got)
	}
	if resolves.Load() != 1 {
		t.Fatal("restart repeated route discovery")
	}
	for _, name := range []string{
		filepath.Base(testBindingPaths().Marker),
		filepath.Base(testBindingPaths().Sidecar),
	} {
		if _, err := store.Read(name); err != nil {
			t.Fatalf("%s was not installed: %v", name, err)
		}
	}
	store.mu.Lock()
	fileCount := len(store.files)
	store.mu.Unlock()
	if fileCount != 3 {
		t.Fatalf("binding backup set has %d files, want key+sidecar+marker", fileCount)
	}
}

func TestSoftwareBindingPreviewIsReadOnlyAndApplyRechecksCandidate(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	paths := testBindingPaths()
	resolveMAC := "00:11:22:33:44:55"
	resolve := func(context.Context) (StableInterface, error) {
		return testStableInterface(resolveMAC), nil
	}
	preview, err := previewSoftwareBinding(context.Background(), store, paths, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Binding.GatewayID != "012300112201334455" ||
		preview.ThreeWordName == "" ||
		preview.KeyFingerprint != preview.Binding.PublicKeySHA256 ||
		len(preview.Confirmation) != sha256.Size*2 {
		t.Fatalf("preview = %+v", preview)
	}
	store.mu.Lock()
	if len(store.files) != 1 {
		t.Fatalf("preview wrote files: %v", store.files)
	}
	store.mu.Unlock()

	resolveMAC = "10:20:30:40:50:60"
	if _, err := applySoftwareBindingConfirmed(
		context.Background(), store, paths, preview.Confirmation, resolve,
	); err == nil {
		t.Fatal("apply accepted a route or MAC change")
	}
	store.mu.Lock()
	if len(store.files) != 1 {
		t.Fatalf("rejected apply wrote files: %v", store.files)
	}
	store.mu.Unlock()

	resolveMAC = "00:11:22:33:44:55"
	store.mu.Lock()
	store.files["nova.key"] = testKeyPEM(t)
	store.mu.Unlock()
	if _, err := applySoftwareBindingConfirmed(
		context.Background(), store, paths, preview.Confirmation, resolve,
	); err == nil {
		t.Fatal("apply accepted a key change")
	}
	store.mu.Lock()
	if len(store.files) != 1 {
		t.Fatalf("key-change rejection wrote files: %v", store.files)
	}
	store.mu.Unlock()
}

func TestWrongConfirmationCannotFinishPendingBinding(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	paths := testBindingPaths()
	preview, err := previewSoftwareBinding(
		context.Background(), store, paths,
		func(context.Context) (StableInterface, error) {
			return testStableInterface("00:11:22:33:44:55"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := encodeSidecar(preview.Binding)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(sidecar)
	pending := mustEncodeMarker(bindingMarker{
		Version: SoftwareBindingVersion, State: "pending", Binding: preview.Binding,
		SidecarSHA256: hexString(digest[:]),
	})
	if err := store.InstallNoReplace(filepath.Base(paths.Marker), pending); err != nil {
		t.Fatal(err)
	}
	if _, err := applySoftwareBindingConfirmed(
		context.Background(), store, paths, "wrong-confirmation",
		func(context.Context) (StableInterface, error) {
			t.Fatal("pending apply repeated discovery")
			return StableInterface{}, nil
		},
	); err == nil {
		t.Fatal("wrong confirmation finished pending binding")
	}
	marker, err := store.Read(filepath.Base(paths.Marker))
	if err != nil || !bytes.Equal(marker, pending) {
		t.Fatalf("pending marker changed: %q, %v", marker, err)
	}
	if _, err := store.Read(filepath.Base(paths.Sidecar)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("wrong confirmation installed sidecar: %v", err)
	}
}

func TestPendingRetryNeverRepeatsDiscovery(t *testing.T) {
	for _, crash := range []struct {
		name         string
		afterInstall string
		afterReplace string
	}{
		{name: "pending", afterInstall: "nova.key.home-link.state"},
		{name: "sidecar", afterInstall: "nova.key.home-link.json"},
		{name: "active", afterReplace: "nova.key.home-link.state"},
	} {
		t.Run(crash.name, func(t *testing.T) {
			store := newMemoryBindingStorage(testKeyPEM(t))
			if crash.afterInstall != "" {
				store.afterInstall[crash.afterInstall] = []error{errors.New("power loss")}
			}
			if crash.afterReplace != "" {
				store.afterReplace[crash.afterReplace] = []error{errors.New("power loss")}
			}
			var resolves atomic.Int32
			_, err := adoptSoftwareBinding(
				context.Background(), store, testBindingPaths(),
				func(context.Context) (StableInterface, error) {
					resolves.Add(1)
					return testStableInterface("00:11:22:33:44:55"), nil
				},
			)
			if err == nil {
				t.Fatal("adoption survived injected crash")
			}
			got, err := loadSoftwareBinding(store, testBindingPaths())
			if err != nil {
				t.Fatal(err)
			}
			if got.GatewayID != "012300112201334455" {
				t.Fatalf("recovered binding = %+v", got)
			}
			if resolves.Load() != 1 {
				t.Fatalf("route resolver calls = %d", resolves.Load())
			}
		})
	}
}

func TestPendingMissingSidecarUsesFrozenInputs(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	store.afterInstall["nova.key.home-link.json"] = []error{errors.New("power loss")}
	_, err := adoptSoftwareBinding(
		context.Background(), store, testBindingPaths(),
		func(context.Context) (StableInterface, error) {
			return testStableInterface("00:11:22:33:44:55"), nil
		},
	)
	if err == nil {
		t.Fatal("adoption survived injected crash")
	}
	store.mu.Lock()
	delete(store.files, "nova.key.home-link.json")
	store.mu.Unlock()
	got, err := loadSoftwareBinding(store, testBindingPaths())
	if err != nil {
		t.Fatal(err)
	}
	if got.PermanentMAC != "00:11:22:33:44:55" {
		t.Fatalf("recovered binding = %+v", got)
	}
}

func TestAdoptionDoesNotFreezeAKeyChangedDuringRouteLookup(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	_, err := adoptSoftwareBinding(
		context.Background(), store, testBindingPaths(),
		func(context.Context) (StableInterface, error) {
			store.mu.Lock()
			store.files["nova.key"] = testKeyPEM(t)
			store.mu.Unlock()
			return testStableInterface("00:11:22:33:44:55"), nil
		},
	)
	if err == nil {
		t.Fatal("adoption accepted a key changed during route lookup")
	}
	if _, err := store.Read("nova.key.home-link.state"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pending marker exists after key race: %v", err)
	}
}

func TestBindingInconsistentStatesFailClosed(t *testing.T) {
	makeAdopted := func(t *testing.T) (*memoryBindingStorage, SoftwareBinding) {
		t.Helper()
		store := newMemoryBindingStorage(testKeyPEM(t))
		binding, err := adoptSoftwareBinding(
			context.Background(), store, testBindingPaths(),
			func(context.Context) (StableInterface, error) {
				return testStableInterface("00:11:22:33:44:55"), nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return store, binding
	}
	tests := []struct {
		name   string
		mutate func(*memoryBindingStorage)
	}{
		{
			name: "active without sidecar",
			mutate: func(store *memoryBindingStorage) {
				delete(store.files, "nova.key.home-link.json")
			},
		},
		{
			name: "sidecar without marker",
			mutate: func(store *memoryBindingStorage) {
				delete(store.files, "nova.key.home-link.state")
			},
		},
		{
			name: "missing key",
			mutate: func(store *memoryBindingStorage) {
				delete(store.files, "nova.key")
			},
		},
		{
			name: "different key",
			mutate: func(store *memoryBindingStorage) {
				store.files["nova.key"] = testKeyPEM(t)
			},
		},
		{
			name: "mutated marker",
			mutate: func(store *memoryBindingStorage) {
				data := store.files["nova.key.home-link.state"]
				data[len(data)/2] ^= 1
				store.files["nova.key.home-link.state"] = data
			},
		},
		{
			name: "mutated sidecar",
			mutate: func(store *memoryBindingStorage) {
				data := store.files["nova.key.home-link.json"]
				data[len(data)/2] ^= 1
				store.files["nova.key.home-link.json"] = data
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := makeAdopted(t)
			store.mu.Lock()
			test.mutate(store)
			store.mu.Unlock()
			if _, err := loadSoftwareBinding(store, testBindingPaths()); err == nil {
				t.Fatal("inconsistent state was accepted")
			}
		})
	}
}

func TestBindingWithoutMetadataIsNotAdopted(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	_, err := loadSoftwareBinding(store, testBindingPaths())
	if !errors.Is(err, ErrBindingNotAdopted) {
		t.Fatalf("unadopted state error = %v", err)
	}
}

func TestConfirmedApplyRejectsDifferentMarkerBeforeStateInspection(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	paths := testBindingPaths()
	confirmed, err := previewSoftwareBinding(
		context.Background(), store, paths,
		func(context.Context) (StableInterface, error) {
			return testStableInterface("00:11:22:33:44:55"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	winnerKey := testKeyPEM(t)
	winner, pending := frozenTestCandidate(
		t, winnerKey, "10:20:30:40:50:60",
	)
	if winner.Binding.GatewayID == confirmed.Binding.GatewayID ||
		winner.ThreeWordName == confirmed.ThreeWordName ||
		winner.Binding.PublicKeySHA256 == confirmed.Binding.PublicKeySHA256 {
		t.Fatal("test candidates are not distinct")
	}
	store.mu.Lock()
	store.files[filepath.Base(paths.Key)] = winnerKey
	store.files[filepath.Base(paths.Marker)] = pending
	store.mu.Unlock()

	if _, err := applySoftwareBinding(store, paths, confirmed); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("different frozen winner error = %v", err)
	}
	assertLosingApplyDidNotActivate(t, store, paths, pending)
}

func TestConfirmedApplyRejectsDifferentInstallNoReplaceWinner(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	paths := testBindingPaths()
	confirmed, err := previewSoftwareBinding(
		context.Background(), store, paths,
		func(context.Context) (StableInterface, error) {
			return testStableInterface("00:11:22:33:44:55"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	winnerKey := testKeyPEM(t)
	winner, pending := frozenTestCandidate(
		t, winnerKey, "10:20:30:40:50:60",
	)
	if winner.Binding.GatewayID == confirmed.Binding.GatewayID ||
		winner.ThreeWordName == confirmed.ThreeWordName ||
		winner.Binding.PublicKeySHA256 == confirmed.Binding.PublicKeySHA256 {
		t.Fatal("test candidates are not distinct")
	}
	store.installWinners[filepath.Base(paths.Marker)] = map[string][]byte{
		filepath.Base(paths.Key):    winnerKey,
		filepath.Base(paths.Marker): pending,
	}

	if _, err := applySoftwareBinding(store, paths, confirmed); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("different no-replace winner error = %v", err)
	}
	assertLosingApplyDidNotActivate(t, store, paths, pending)
}

func TestBindingWriterGuardBlocksUnboundNovaKeyInstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("binding writer is supported only on Linux")
	}
	parent := t.TempDir()
	keydir := filepath.Join(parent, "state")
	if err := os.Mkdir(keydir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(keydir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Lock(); err != nil {
		store.Close()
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := LoadOrCreateUnboundNovaIdentity(paths.Key)
		result <- err
	}()
	select {
	case err := <-result:
		store.Close()
		t.Fatalf("guarded create did not wait for binding writer: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := store.InstallNoReplace(
		filepath.Base(paths.Marker), []byte("reserved\n"),
	); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrBindingIncomplete) {
		t.Fatalf("guarded create error = %v", err)
	}
	entries, err := os.ReadDir(keydir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == filepath.Base(paths.Key) ||
			strings.HasPrefix(entry.Name(), "."+filepath.Base(paths.Key)+".tmp-") {
			t.Fatalf("guarded create wrote %q", entry.Name())
		}
	}
}

func frozenTestCandidate(
	t *testing.T,
	key []byte,
	mac string,
) (SoftwareBindingPreview, []byte) {
	t.Helper()
	store := newMemoryBindingStorage(key)
	paths := testBindingPaths()
	preview, err := previewSoftwareBinding(
		context.Background(), store, paths,
		func(context.Context) (StableInterface, error) {
			return testStableInterface(mac), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := encodeSidecar(preview.Binding)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(sidecar)
	pending, err := encodeMarker(bindingMarker{
		Version:       SoftwareBindingVersion,
		State:         "pending",
		Binding:       preview.Binding,
		SidecarSHA256: hexString(digest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	return preview, pending
}

func assertLosingApplyDidNotActivate(
	t *testing.T,
	store *memoryBindingStorage,
	paths BindingPaths,
	pending []byte,
) {
	t.Helper()
	marker, err := store.Read(filepath.Base(paths.Marker))
	if err != nil || !bytes.Equal(marker, pending) {
		t.Fatalf("losing apply changed pending marker: %q, %v", marker, err)
	}
	if _, err := store.Read(filepath.Base(paths.Sidecar)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("losing apply installed sidecar: %v", err)
	}
}

func TestBindingBackupRestoreUnit(t *testing.T) {
	store := newMemoryBindingStorage(testKeyPEM(t))
	want, err := adoptSoftwareBinding(
		context.Background(), store, testBindingPaths(),
		func(context.Context) (StableInterface, error) {
			return testStableInterface("00:11:22:33:44:55"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	backup := make(map[string][]byte)
	store.mu.Lock()
	for name, data := range store.files {
		backup[name] = append([]byte(nil), data...)
	}
	store.mu.Unlock()
	restored := &memoryBindingStorage{
		files: backup, installErrors: make(map[string][]error),
		afterInstall: make(map[string][]error), replaceErrors: make(map[string][]error),
		afterReplace: make(map[string][]error),
	}
	got, err := loadSoftwareBinding(restored, testBindingPaths())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("restored binding = %+v, want %+v", got, want)
	}
}

func writeLinuxFrozenBinding(t *testing.T) (string, SoftwareBinding) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "nova.key")
	keyBytes := testKeyPEM(t)
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	publicKey, _, err := parseSoftwarePrivateKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	publicHash := sha256Sum(publicKey)
	binding := SoftwareBinding{
		Version:         SoftwareBindingVersion,
		Source:          SourceSoftware,
		InterfaceName:   "enp1s0",
		InterfaceIndex:  7,
		PermanentMAC:    "00:11:22:33:44:55",
		GatewayID:       "012300112201334455",
		PublicKeySHA256: hexString(publicHash[:]),
	}
	sidecar, err := encodeSidecar(binding)
	if err != nil {
		t.Fatal(err)
	}
	sidecarHash := sha256Sum(sidecar)
	active := bindingMarker{
		Version: SoftwareBindingVersion, State: "active", Binding: binding,
		SidecarSHA256: hexString(sidecarHash[:]),
	}
	for name, data := range map[string][]byte{
		keyPath + ".home-link.json":  sidecar,
		keyPath + ".home-link.state": mustEncodeMarker(active),
	} {
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return keyPath, binding
}

func hexString(data []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(data)*2)
	for i, value := range data {
		out[i*2] = digits[value>>4]
		out[i*2+1] = digits[value&0xf]
	}
	return string(out)
}

func TestLinuxBindingTrustChecks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, keyPath string) {
				target := keyPath + ".home-link.json"
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(keyPath+".home-link.state"), target); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "special file",
			mutate: func(t *testing.T, keyPath string) {
				target := keyPath + ".home-link.json"
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := makeBindingFIFO(target, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "group readable",
			mutate: func(t *testing.T, keyPath string) {
				if err := os.Chmod(keyPath+".home-link.state", 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hard link",
			mutate: func(t *testing.T, keyPath string) {
				if err := os.Link(keyPath, keyPath+".other-link"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsafe directory",
			mutate: func(t *testing.T, keyPath string) {
				if err := os.Chmod(filepath.Dir(keyPath), 0o777); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keyPath, _ := writeLinuxFrozenBinding(t)
			test.mutate(t, keyPath)
			if _, err := LoadSoftwareBinding(keyPath); err == nil {
				t.Fatal("unsafe binding state was accepted")
			}
		})
	}
}

func TestLinuxBindingRejectsWrongOwner(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("owner mutation needs Linux root")
	}
	keyPath, _ := writeLinuxFrozenBinding(t)
	if err := os.Chown(keyPath+".home-link.state", 1, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSoftwareBinding(keyPath); err == nil {
		t.Fatal("wrong owner was accepted")
	}
}

func TestLinuxBindingInstallFsyncRecovery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("directory-sync-%d", failAt), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
			if err != nil {
				t.Fatal(err)
			}
			ops := defaultBindingFileOps()
			syncCalls := 0
			ops.syncDirectory = func(file fileSyncer) error {
				syncCalls++
				if syncCalls == failAt {
					return errors.New("injected directory sync failure")
				}
				return file.Sync()
			}
			store, err := openBindingStorage(paths, ops)
			if err != nil {
				t.Fatal(err)
			}
			err = store.InstallNoReplace("state", []byte("durable\n"))
			store.Close()
			if err == nil {
				t.Fatal("install survived injected directory sync failure")
			}

			reopened, err := openBindingStorage(paths, defaultBindingFileOps())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err := reopened.Read("state")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "durable\n" {
				t.Fatalf("recovered state = %q", got)
			}
		})
	}
}

func TestLinuxBindingInstallFailsBeforeLink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	ops := defaultBindingFileOps()
	ops.syncFile = func(fileSyncer) error { return errors.New("injected file sync failure") }
	store, err := openBindingStorage(paths, ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InstallNoReplace("state", []byte("not durable\n")); err == nil {
		t.Fatal("install survived injected file sync failure")
	}
	store.Close()
	if _, err := os.Lstat(filepath.Join(dir, "state")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target exists after pre-link failure: %v", err)
	}
}

func TestLinuxBindingInstallRemoveFailureRecovery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	ops := defaultBindingFileOps()
	ops.remove = func(string) error { return errors.New("injected cleanup failure") }
	store, err := openBindingStorage(paths, ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InstallNoReplace("state", []byte("linked\n")); err == nil {
		t.Fatal("install survived injected cleanup failure")
	}
	store.Close()

	reopened, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Read("state")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "linked\n" {
		t.Fatalf("recovered state = %q", got)
	}
}

func TestLinuxBindingInstallCleansPreLinkCrashTemp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".state.tmp-crashed")
	if err := os.WriteFile(stale, []byte("partial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InstallNoReplace("state", []byte("winner\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pre-link crash temp remains: %v", err)
	}
}

func TestLinuxBindingConcurrentNoReplace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := openBindingStorage(paths, defaultBindingFileOps())
			if err != nil {
				errs <- err
				return
			}
			defer store.Close()
			err = store.InstallNoReplace("state", []byte("one winner\n"))
			if err != nil && !errors.Is(err, fs.ErrExist) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.Read("state")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one winner\n" {
		t.Fatalf("winner data = %q", got)
	}
}

func TestLinuxBindingDirectorySwapFailsBeforeReturn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "identity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	moved := filepath.Join(parent, "identity-moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.InstallNoReplace("state", []byte("detached\n")); err == nil {
		t.Fatal("directory swap was accepted")
	}
}

func TestLinuxBindingFileSwapFailsBeforeReturn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state")
	if err := os.WriteFile(statePath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	ops := defaultBindingFileOps()
	swapped := false
	ops.syncDirectory = func(file fileSyncer) error {
		if !swapped {
			swapped = true
			if err := os.Rename(statePath, statePath+".old"); err != nil {
				return err
			}
			if err := os.WriteFile(statePath, []byte("second\n"), 0o600); err != nil {
				return err
			}
		}
		return file.Sync()
	}
	store, err := openBindingStorage(paths, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Read("state"); err == nil {
		t.Fatal("file swap before return was accepted")
	}
}

func TestLinuxBindingParentSwapFailsClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	grandparent := t.TempDir()
	parent := filepath.Join(grandparent, "parent")
	keydir := filepath.Join(parent, "identity")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(keydir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(keydir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	moved := filepath.Join(grandparent, "parent-moved")
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, "identity"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Revalidate(); err == nil {
		t.Fatal("parent directory swap was accepted")
	}
}

func TestLinuxBindingKeydirSpecialFilesDoNotBlock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	for _, test := range []struct {
		name string
		make func(string) error
	}{
		{name: "fifo", make: func(path string) error { return makeBindingFIFO(path, 0o600) }},
		{name: "regular", make: func(path string) error {
			return os.WriteFile(path, []byte("not a directory"), 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			keydir := filepath.Join(parent, "identity")
			if err := test.make(keydir); err != nil {
				t.Fatal(err)
			}
			paths, err := PathsForKey(filepath.Join(keydir, "nova.key"))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				store, err := openBindingStorage(paths, defaultBindingFileOps())
				if store != nil {
					_ = store.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("special keydir was accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("special keydir open blocked")
			}
		})
	}
}

func TestLinuxBindingMarkerTransitionCrashRecovery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	for _, test := range []struct {
		name   string
		mutate func(*bindingFileOps)
	}{
		{
			name: "temp-file-sync",
			mutate: func(ops *bindingFileOps) {
				ops.syncFile = func(fileSyncer) error {
					return errors.New("injected transition file sync failure")
				}
			},
		},
		{
			name: "before-rename",
			mutate: func(ops *bindingFileOps) {
				ops.rename = func(string, string) error {
					return errors.New("injected transition rename failure")
				}
			},
		},
		{
			name: "after-rename-before-directory-sync",
			mutate: func(ops *bindingFileOps) {
				calls := 0
				ops.syncDirectory = func(file fileSyncer) error {
					calls++
					if calls == 3 {
						return errors.New("injected transition directory sync failure")
					}
					return file.Sync()
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
			if err != nil {
				t.Fatal(err)
			}
			base, err := openBindingStorage(paths, defaultBindingFileOps())
			if err != nil {
				t.Fatal(err)
			}
			if err := base.InstallNoReplace("state", []byte("pending\n")); err != nil {
				t.Fatal(err)
			}
			_ = base.Close()

			ops := defaultBindingFileOps()
			test.mutate(&ops)
			store, err := openBindingStorage(paths, ops)
			if err != nil {
				t.Fatal(err)
			}
			err = store.ReplaceExact("state", []byte("pending\n"), []byte("active\n"))
			_ = store.Close()
			if err == nil {
				t.Fatal("transition survived injected crash")
			}

			reopened, err := openBindingStorage(paths, defaultBindingFileOps())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.ReplaceExact(
				"state", []byte("pending\n"), []byte("active\n"),
			); err != nil {
				t.Fatal(err)
			}
			got, err := reopened.Read("state")
			if err != nil || string(got) != "active\n" {
				t.Fatalf("recovered marker = %q, %v", got, err)
			}
		})
	}
}

func TestLinuxBindingConcurrentMarkerTransition(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InstallNoReplace("state", []byte("pending\n")); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := openBindingStorage(paths, defaultBindingFileOps())
			if err == nil {
				err = store.ReplaceExact("state", []byte("pending\n"), []byte("active\n"))
				_ = store.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestLinuxBindingMarkerTransitionCleansCrashTemp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux binding storage only")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "state"), []byte("active\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".state.tmp-crashed")
	if err := os.WriteFile(stale, []byte("active\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.ReplaceExact(
		"state", []byte("pending\n"), []byte("active\n"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transition crash temp remains: %v", err)
	}
}
