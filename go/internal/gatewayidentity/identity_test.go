package gatewayidentity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

type testIdentity struct {
	id  string
	key *ecdsa.PrivateKey
}

func newTestIdentity(t *testing.T, id string) *testIdentity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testIdentity{id: id, key: key}
}

func (i *testIdentity) GatewayID() string { return i.id }
func (i *testIdentity) PublicKey() []byte {
	return append(i.key.X.FillBytes(make([]byte, 32)), i.key.Y.FillBytes(make([]byte, 32))...)
}
func (i *testIdentity) Sign(message []byte) ([]byte, error) {
	return signTestKey(i.key, message)
}

func TestResolvePrefersHardware(t *testing.T) {
	hardwareIdentity := newTestIdentity(t, "012300bcdec52af201")
	softwareCalled := false
	got, source, err := Resolve(context.Background(),
		ProviderFunc(func(context.Context) (Identity, error) { return hardwareIdentity, nil }),
		ProviderFunc(func(context.Context) (Identity, error) {
			softwareCalled = true
			return nil, errors.New("must not run")
		}))
	if err != nil {
		t.Fatal(err)
	}
	if got != hardwareIdentity || source != SourceHardware || softwareCalled {
		t.Fatalf("hardware selection failed: source=%q software_called=%t", source, softwareCalled)
	}
}

func TestResolveFallsBackOnlyWhenHardwareIsAbsent(t *testing.T) {
	softwareIdentity := newTestIdentity(t, "0123aabbcc01ddeeff")
	softwareCalls := 0
	software := ProviderFunc(func(context.Context) (Identity, error) {
		softwareCalls++
		return softwareIdentity, nil
	})

	got, source, err := Resolve(context.Background(),
		ProviderFunc(func(context.Context) (Identity, error) { return nil, ErrHardwareUnavailable }), software)
	if err != nil || got != softwareIdentity || source != SourceSoftware || softwareCalls != 1 {
		t.Fatalf("software fallback: got=%v source=%q calls=%d err=%v", got, source, softwareCalls, err)
	}

	softwareCalls = 0
	_, _, err = Resolve(context.Background(),
		ProviderFunc(func(context.Context) (Identity, error) { return nil, errors.New("chip read failed") }), software)
	if err == nil || softwareCalls != 0 {
		t.Fatalf("broken hardware fell back: calls=%d err=%v", softwareCalls, err)
	}

	softwareCalls = 0
	_, _, err = Resolve(context.Background(),
		ProviderFunc(func(context.Context) (Identity, error) {
			return &testIdentity{id: "bad", key: softwareIdentity.key}, nil
		}), software)
	if err == nil || softwareCalls != 0 {
		t.Fatalf("invalid hardware identity fell back: calls=%d err=%v", softwareCalls, err)
	}
}

func TestSoftwareProviderReusesCanonicalKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "nova.key")
	provider := SoftwareProvider{
		KeyPath: keyPath,
		GatewayID: func(context.Context) (string, error) {
			mac, err := net.ParseMAC("dc:a6:32:f8:38:f7")
			if err != nil {
				return "", err
			}
			return GatewayIDFromMAC(mac)
		},
	}
	first, err := provider.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.GatewayID() != "0123dca63201f838f7" {
		t.Fatalf("gateway id = %q", first.GatewayID())
	}
	if string(first.PublicKey()) != string(second.PublicKey()) {
		t.Fatal("public key changed after reload")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(keyPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "nova.key" {
		t.Fatalf("identity created unexpected files: %v", entries)
	}

	message := []byte("ftw-home-link-test/v1\nhello")
	signature, err := first.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	if len(signature) != SignatureBytes || !Verify(first.PublicKey(), message, signature) {
		t.Fatal("software identity did not produce the gateway wire format")
	}
}

func TestNormalizeGatewayID(t *testing.T) {
	got, err := NormalizeGatewayID(" 0123DCA63201F838F7 ")
	if err != nil || got != "0123dca63201f838f7" {
		t.Fatalf("normalized = %q, err=%v", got, err)
	}
	for _, bad := range []string{"", "0123", "0123dca63201f838fg", "01:23:dc:a6:32:01:f8:38:f7"} {
		if _, err := NormalizeGatewayID(bad); err == nil {
			t.Fatalf("accepted gateway id %q", bad)
		}
	}
}

func signTestKey(key *ecdsa.PrivateKey, message []byte) ([]byte, error) {
	digest := sha256Sum(message)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return nil, err
	}
	return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...), nil
}
