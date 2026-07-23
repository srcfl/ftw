package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/nova"
)

type cliIdentity struct {
	id     string
	public []byte
}

func (i cliIdentity) GatewayID() string { return i.id }
func (i cliIdentity) PublicKey() []byte { return append([]byte(nil), i.public...) }
func (i cliIdentity) Sign([]byte) ([]byte, error) {
	return nil, errors.New("not used")
}

func TestHomeLinkAdoptCLIPreviewsBeforeExactApply(t *testing.T) {
	publicKey, err := hex.DecodeString(
		"6b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296" +
			"4fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5",
	)
	if err != nil {
		t.Fatal(err)
	}
	binding := gatewayidentity.SoftwareBinding{
		Version:         gatewayidentity.SoftwareBindingVersion,
		Source:          gatewayidentity.SourceSoftware,
		InterfaceName:   "enp1s0",
		InterfaceIndex:  7,
		PermanentMAC:    "00:11:22:33:44:55",
		GatewayID:       "012300112201334455",
		PublicKeySHA256: hex.EncodeToString(sha256Bytes(publicKey)),
	}
	preview := gatewayidentity.SoftwareBindingPreview{
		Binding: binding, ThreeWordName: "calm-blue-fox",
		KeyFingerprint: binding.PublicKeySHA256, Confirmation: "exact-candidate",
	}
	var out bytes.Buffer
	err = adoptHomeLinkIdentityWith(
		[]string{"--key=/data/nova.key"},
		&out,
		func(_ context.Context, path string) (gatewayidentity.SoftwareBindingPreview, error) {
			if path != "/data/nova.key" {
				t.Fatalf("preview key path = %q", path)
			}
			return preview, nil
		},
		func(context.Context, string, string) (gatewayidentity.SoftwareBinding, error) {
			t.Fatal("apply called during preview")
			return gatewayidentity.SoftwareBinding{}, nil
		},
		func(path string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			t.Fatal("load called during preview")
			return nil, gatewayidentity.SoftwareBinding{}, nil
		},
		func(got []byte) (string, error) {
			t.Fatal("route handle called during preview")
			return "", nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantPreview := "gateway_id=012300112201334455\n" +
		"three_word_name=calm-blue-fox\n" +
		"key_fingerprint_sha256=" + binding.PublicKeySHA256 + "\n" +
		"confirmation=exact-candidate\n"
	if got := out.String(); got != wantPreview {
		t.Fatalf("CLI output = %q", got)
	}

	out.Reset()
	err = adoptHomeLinkIdentityWith(
		[]string{"--key=/data/nova.key", "--confirm=exact-candidate"},
		&out,
		func(context.Context, string) (gatewayidentity.SoftwareBindingPreview, error) {
			t.Fatal("CLI repeated preview outside apply")
			return gatewayidentity.SoftwareBindingPreview{}, nil
		},
		func(_ context.Context, path, confirmation string) (gatewayidentity.SoftwareBinding, error) {
			if path != "/data/nova.key" || confirmation != "exact-candidate" {
				t.Fatalf("apply = %q, %q", path, confirmation)
			}
			return binding, nil
		},
		func(path string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			return cliIdentity{id: binding.GatewayID, public: publicKey}, binding, nil
		},
		func(got []byte) (string, error) {
			if !bytes.Equal(got, publicKey) {
				t.Fatal("CLI passed a different public key")
			}
			return "route-handle", nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("route_handle=route-handle\n")) {
		t.Fatalf("apply output = %q", out.String())
	}
}

func TestHomeLinkAdoptCLIRequiresKeyPathAndExactReload(t *testing.T) {
	noCallPreview := func(context.Context, string) (gatewayidentity.SoftwareBindingPreview, error) {
		t.Fatal("preview called without a key path")
		return gatewayidentity.SoftwareBindingPreview{}, nil
	}
	noCallApply := func(context.Context, string, string) (gatewayidentity.SoftwareBinding, error) {
		t.Fatal("apply called without a key path")
		return gatewayidentity.SoftwareBinding{}, nil
	}
	noCallLoad := func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
		t.Fatal("load called without adoption")
		return nil, gatewayidentity.SoftwareBinding{}, nil
	}
	noCallHandle := func([]byte) (string, error) {
		t.Fatal("route handle called without adoption")
		return "", nil
	}
	if err := adoptHomeLinkIdentityWith(
		nil, &bytes.Buffer{}, noCallPreview, noCallApply, noCallLoad, noCallHandle,
	); err == nil {
		t.Fatal("CLI accepted a missing key path")
	}

	first := gatewayidentity.SoftwareBinding{GatewayID: "012300112201334455"}
	second := gatewayidentity.SoftwareBinding{GatewayID: "012300112201334456"}
	err := adoptHomeLinkIdentityWith(
		[]string{"--key=/data/nova.key", "--confirm=candidate"},
		&bytes.Buffer{},
		func(context.Context, string) (gatewayidentity.SoftwareBindingPreview, error) {
			return gatewayidentity.SoftwareBindingPreview{}, nil
		},
		func(context.Context, string, string) (gatewayidentity.SoftwareBinding, error) {
			return first, nil
		},
		func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			return cliIdentity{}, second, nil
		},
		noCallHandle,
	)
	if err == nil {
		t.Fatal("CLI accepted a changed binding after reopen")
	}
}

func TestStartupIdentityFailsClosedWithoutCreatingReplacementKey(t *testing.T) {
	existingCalls, createCalls := 0, 0
	_, err := loadSiteIdentityWith(
		"/data/nova.key",
		func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			return nil, gatewayidentity.SoftwareBinding{}, gatewayidentity.ErrBindingMismatch
		},
		func(string) (*nova.Identity, error) {
			existingCalls++
			return &nova.Identity{}, nil
		},
		func(string) (*nova.Identity, error) {
			createCalls++
			return &nova.Identity{}, nil
		},
	)
	if !errors.Is(err, gatewayidentity.ErrBindingMismatch) {
		t.Fatalf("startup binding error = %v", err)
	}
	if existingCalls != 0 || createCalls != 0 {
		t.Fatalf("Nova loaders called: existing=%d create=%d", existingCalls, createCalls)
	}
}

func TestStartupIdentityUsesLegacyKeyOnlyBeforeAdoption(t *testing.T) {
	legacy := testNovaIdentity(t)
	createCalls := 0
	got, err := loadSiteIdentityWith(
		"/data/nova.key",
		func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			return nil, gatewayidentity.SoftwareBinding{},
				gatewayidentity.ErrBindingNotAdopted
		},
		func(string) (*nova.Identity, error) {
			return nil, fs.ErrNotExist
		},
		func(string) (*nova.Identity, error) {
			createCalls++
			return legacy, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nova != legacy || got.HomeLink != nil || createCalls != 1 {
		t.Fatalf("legacy result = %+v, create calls=%d", got, createCalls)
	}
}

func TestStartupUnadoptedUnsafeExistingKeyNeverCreates(t *testing.T) {
	createCalls := 0
	wantErr := errors.New("unsafe existing key")
	_, err := loadSiteIdentityWith(
		"/data/nova.key",
		func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			return nil, gatewayidentity.SoftwareBinding{},
				gatewayidentity.ErrBindingNotAdopted
		},
		func(string) (*nova.Identity, error) { return nil, wantErr },
		func(string) (*nova.Identity, error) {
			createCalls++
			return nil, nil
		},
	)
	if !errors.Is(err, wantErr) || createCalls != 0 {
		t.Fatalf("error=%v create calls=%d", err, createCalls)
	}
}

func TestStartupIdentityKeepsNovaOnUnsupportedPlatform(t *testing.T) {
	legacy := testNovaIdentity(t)
	existingCalls, createCalls := 0, 0
	got, err := loadSiteIdentityWith(
		"/data/nova.key",
		func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			return nil, gatewayidentity.SoftwareBinding{},
				gatewayidentity.ErrUnsupportedBinding
		},
		func(string) (*nova.Identity, error) {
			existingCalls++
			return legacy, nil
		},
		func(string) (*nova.Identity, error) {
			createCalls++
			return nil, errors.New("unexpected create")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nova != legacy || existingCalls != 1 || createCalls != 0 {
		t.Fatalf("unsupported result=%+v existing=%d create=%d", got, existingCalls, createCalls)
	}
}

func TestStartupIdentityUsesSameBoundKeyForNovaWithoutCreate(t *testing.T) {
	legacy := testNovaIdentity(t)
	public, err := hex.DecodeString(legacy.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	bound := cliIdentity{id: "012300112201334455", public: public}
	binding := gatewayidentity.SoftwareBinding{
		GatewayID: bound.id, PublicKeySHA256: hex.EncodeToString(sha256Bytes(public)),
	}
	createCalls := 0
	got, err := loadSiteIdentityWith(
		"/data/nova.key",
		func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			return bound, binding, nil
		},
		func(string) (*nova.Identity, error) {
			return legacy, nil
		},
		func(string) (*nova.Identity, error) {
			createCalls++
			return nil, errors.New("unexpected create")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.HomeLink == nil || got.Nova != legacy || got.Binding != binding || createCalls != 0 {
		t.Fatalf("bound result = %+v, create calls=%d", got, createCalls)
	}
}

func TestStartupBrokenOrMissingBoundKeyNeverCreates(t *testing.T) {
	for _, existingErr := range []error{fs.ErrNotExist, gatewayidentity.ErrBindingMismatch} {
		t.Run(existingErr.Error(), func(t *testing.T) {
			createCalls := 0
			_, err := loadSiteIdentityWith(
				"/data/nova.key",
				func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
					return cliIdentity{id: "012300112201334455"}, gatewayidentity.SoftwareBinding{}, nil
				},
				func(string) (*nova.Identity, error) { return nil, existingErr },
				func(string) (*nova.Identity, error) {
					createCalls++
					return nil, nil
				},
			)
			if !errors.Is(err, existingErr) || createCalls != 0 {
				t.Fatalf("error=%v create calls=%d", err, createCalls)
			}
		})
	}
}

func TestUnsupportedMissingKeyCreatesOnlyAfterMetadataAbsence(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata-%t", metadata), func(t *testing.T) {
			legacy := testNovaIdentity(t)
			createCalls := 0
			got, err := loadSiteIdentityWith(
				"/data/nova.key",
				func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
					return nil, gatewayidentity.SoftwareBinding{}, gatewayidentity.ErrUnsupportedBinding
				},
				func(string) (*nova.Identity, error) { return nil, fs.ErrNotExist },
				func(string) (*nova.Identity, error) {
					createCalls++
					if metadata {
						return nil, gatewayidentity.ErrBindingIncomplete
					}
					return legacy, nil
				},
			)
			if metadata {
				if !errors.Is(err, gatewayidentity.ErrBindingIncomplete) || createCalls != 1 {
					t.Fatalf("metadata error=%v create calls=%d", err, createCalls)
				}
				return
			}
			if err != nil || got.Nova != legacy || createCalls != 1 {
				t.Fatalf("result=%+v error=%v create calls=%d", got, err, createCalls)
			}
		})
	}
}

func TestStartupGuardedCreateRejectsConcurrentBinding(t *testing.T) {
	createCalls := 0
	_, err := loadSiteIdentityWith(
		"/data/nova.key",
		func(string) (gatewayidentity.Identity, gatewayidentity.SoftwareBinding, error) {
			return nil, gatewayidentity.SoftwareBinding{},
				gatewayidentity.ErrBindingNotAdopted
		},
		func(string) (*nova.Identity, error) { return nil, fs.ErrNotExist },
		func(string) (*nova.Identity, error) {
			createCalls++
			return nil, gatewayidentity.ErrBindingIncomplete
		},
	)
	if !errors.Is(err, gatewayidentity.ErrBindingIncomplete) || createCalls != 1 {
		t.Fatalf("error=%v create calls=%d", err, createCalls)
	}
}

func testNovaIdentity(t *testing.T) *nova.Identity {
	t.Helper()
	identity, err := nova.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func sha256Bytes(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}
