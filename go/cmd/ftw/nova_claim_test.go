package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/nova"
)

func TestNovaClaimRejectsHomeLinkBindingStateWithoutCreatingKey(t *testing.T) {
	for _, reserved := range []string{
		"nova.key.home-link.state",
		"nova.key.home-link.json",
		".nova.key.home-link.state.tmp-install",
		".nova.key.home-link.json.tmp-transition",
	} {
		t.Run(reserved, func(t *testing.T) {
			root := t.TempDir()
			keydir := filepath.Join(root, "state")
			if err := os.Mkdir(keydir, 0o700); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(keydir, "nova.key")
			if err := os.WriteFile(
				filepath.Join(keydir, reserved), []byte("binding state\n"), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			configPath := writeNovaClaimTestConfig(t, root, keyPath)
			err := claimAndProvision(
				configPath,
				"https://127.0.0.1:1",
				"",
				1883,
				false,
				"org-test",
				"site-test",
				"claimer-test",
				"gateway-test",
				"operator-token",
				false,
			)
			if !errors.Is(err, gatewayidentity.ErrBindingIncomplete) ||
				!strings.Contains(err.Error(), "key material") {
				t.Fatalf("nova-claim error = %v", err)
			}
			assertNovaClaimLeftNoKey(t, keydir)
		})
	}
}

func TestNovaClaimRejectsKeyDirectorySwapWithoutFallback(t *testing.T) {
	root := t.TempDir()
	keydir := filepath.Join(root, "state")
	moved := filepath.Join(root, "validated-state")
	if err := os.Mkdir(keydir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(keydir, "nova.key")
	configPath := writeNovaClaimTestConfig(t, root, keyPath)

	originalLoader := loadNovaClaimIdentity
	t.Cleanup(func() { loadNovaClaimIdentity = originalLoader })
	loadNovaClaimIdentity = func(path string) (*nova.Identity, error) {
		if err := os.Rename(keydir, moved); err != nil {
			return nil, err
		}
		if err := os.Mkdir(keydir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(
			filepath.Join(keydir, "nova.key.home-link.state"),
			[]byte("race"),
			0o600,
		); err != nil {
			return nil, err
		}
		return gatewayidentity.LoadOrCreateUnboundNovaIdentity(path)
	}

	err := claimAndProvision(
		configPath,
		"https://127.0.0.1:1",
		"",
		1883,
		false,
		"org-test",
		"site-test",
		"claimer-test",
		"gateway-test",
		"operator-token",
		false,
	)
	if !errors.Is(err, gatewayidentity.ErrBindingIncomplete) ||
		!strings.Contains(err.Error(), "key material") {
		t.Fatalf("nova-claim error = %v", err)
	}
	assertNovaClaimLeftNoKey(t, moved)
	assertNovaClaimLeftNoKey(t, keydir)
}

func writeNovaClaimTestConfig(t *testing.T, root, keyPath string) string {
	t.Helper()
	configPath := filepath.Join(root, "config.yaml")
	statePath := filepath.Join(root, "state.db")
	data := []byte(fmt.Sprintf(
		"fuse:\n  max_amps: 16\nstate:\n  path: %q\nnova:\n  key_path: %q\n",
		statePath,
		keyPath,
	))
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func assertNovaClaimLeftNoKey(t *testing.T, keydir string) {
	t.Helper()
	entries, err := os.ReadDir(keydir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "nova.key" ||
			strings.HasPrefix(entry.Name(), ".nova.key.tmp-") {
			t.Fatalf("nova-claim left %q", entry.Name())
		}
	}
}
