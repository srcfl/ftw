package gatewayidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBoundSoftwareIdentitySignsWireFormat(t *testing.T) {
	keyBytes := testKeyPEM(t)
	publicKey, privateKey, err := parseSoftwarePrivateKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	identity := &boundSoftwareIdentity{
		gatewayID:  "012300112201334455",
		publicKey:  publicKey,
		privateKey: privateKey,
	}
	message := []byte("ftw-home-link-test/v1\nbound identity")
	signature, err := identity.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(publicKey, message, signature) {
		t.Fatal("bound identity signature did not verify")
	}
	returned := identity.PublicKey()
	returned[0] ^= 1
	if hex.EncodeToString(identity.PublicKey()) == hex.EncodeToString(returned) {
		t.Fatal("public key caller mutated the identity")
	}
}

func TestLoadBoundSoftwareIdentityUsesExactFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		_, _, err := LoadBoundSoftwareIdentity(filepath.Join(t.TempDir(), "nova.key"))
		if !errors.Is(err, ErrUnsupportedBinding) {
			t.Fatalf("non-Linux error = %v", err)
		}
		return
	}
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
	publicHash := sha256.Sum256(publicKey)
	binding := SoftwareBinding{
		Version:         SoftwareBindingVersion,
		Source:          SourceSoftware,
		InterfaceName:   "enp1s0",
		InterfaceIndex:  7,
		PermanentMAC:    "00:11:22:33:44:55",
		GatewayID:       "012300112201334455",
		PublicKeySHA256: hex.EncodeToString(publicHash[:]),
	}
	sidecar, err := encodeSidecar(binding)
	if err != nil {
		t.Fatal(err)
	}
	sidecarHash := sha256.Sum256(sidecar)
	active := bindingMarker{
		Version: SoftwareBindingVersion, State: "active", Binding: binding,
		SidecarSHA256: hex.EncodeToString(sidecarHash[:]),
	}
	for name, data := range map[string][]byte{
		keyPath + ".home-link.json":  sidecar,
		keyPath + ".home-link.state": mustEncodeMarker(active),
	} {
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	identity, got, err := LoadBoundSoftwareIdentity(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != binding || identity.GatewayID() != binding.GatewayID {
		t.Fatalf("loaded identity = %q, binding = %+v", identity.GatewayID(), got)
	}
	providerIdentity, err := (BoundSoftwareProvider{KeyPath: keyPath}).Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(providerIdentity.PublicKey()) != hex.EncodeToString(publicKey) {
		t.Fatal("bound provider returned a different key")
	}
}

func TestSoftwareProviderDoesNotCreateBesideBindingMetadata(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "nova.key")
	if err := os.WriteFile(
		keyPath+".home-link.state", []byte("binding in progress\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	_, err := (SoftwareProvider{
		KeyPath: keyPath,
		GatewayID: func(context.Context) (string, error) {
			return "012300112201334455", nil
		},
	}).Identity(context.Background())
	if !errors.Is(err, ErrBindingIncomplete) {
		t.Fatalf("provider error = %v", err)
	}
	if _, err := os.Lstat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider created a key beside binding metadata: %v", err)
	}
}
