package gatewayidentity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"strings"
)

const (
	SoftwareBindingVersion = 1
	maxBindingFileBytes    = 16 << 10
)

var (
	ErrBindingNotAdopted = errors.New("home link identity is not adopted")
	ErrBindingIncomplete = errors.New("home link identity binding is incomplete")
	ErrBindingMismatch   = errors.New("home link identity binding does not match")
)

type SoftwareBinding struct {
	Version         int    `json:"version"`
	Source          Source `json:"source"`
	InterfaceName   string `json:"interface_name"`
	InterfaceIndex  int    `json:"interface_index"`
	PermanentMAC    string `json:"permanent_mac"`
	GatewayID       string `json:"gateway_id"`
	PublicKeySHA256 string `json:"public_key_sha256"`
}

type BindingPaths struct {
	Key     string
	Sidecar string
	Marker  string
}

type bindingMarker struct {
	Version       int             `json:"version"`
	State         string          `json:"state"`
	Binding       SoftwareBinding `json:"binding"`
	SidecarSHA256 string          `json:"sidecar_sha256"`
}

type SoftwareBindingPreview struct {
	Binding        SoftwareBinding
	ThreeWordName  string
	KeyFingerprint string
	Confirmation   string
}

type bindingStorage interface {
	Lock() error
	Read(string) ([]byte, error)
	InstallNoReplace(string, []byte) error
	ReplaceExact(string, []byte, []byte) error
	Revalidate() error
	Close() error
}

type bindingFileOps struct {
	syncFile      func(fileSyncer) error
	syncDirectory func(fileSyncer) error
	link          func(string, string) error
	rename        func(string, string) error
	remove        func(string) error
	randomSuffix  func() (string, error)
}

type fileSyncer interface {
	Sync() error
}

func PathsForKey(keyPath string) (BindingPaths, error) {
	if strings.TrimSpace(keyPath) == "" {
		return BindingPaths{}, errors.New("canonical key path is empty")
	}
	abs, err := filepath.Abs(keyPath)
	if err != nil {
		return BindingPaths{}, fmt.Errorf("resolve canonical key path: %w", err)
	}
	base := filepath.Base(abs)
	if base == "." || base == string(filepath.Separator) || !fs.ValidPath(filepath.ToSlash(base)) {
		return BindingPaths{}, errors.New("canonical key path has no file name")
	}
	return BindingPaths{
		Key:     abs,
		Sidecar: abs + ".home-link.json",
		Marker:  abs + ".home-link.state",
	}, nil
}

// HasSoftwareBindingMetadata reports whether an adoption has started under a
// pinned binding directory. It does not decode the files.
func HasSoftwareBindingMetadata(keyPath string) (bool, error) {
	paths, err := PathsForKey(keyPath)
	if err != nil {
		return false, err
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		return false, err
	}
	defer store.Close()
	if err := store.Lock(); err != nil {
		return false, err
	}
	state, err := inspectBindingState(store, paths)
	if err != nil {
		return false, err
	}
	return state.hasAny(), nil
}

// PreviewSoftwareBinding returns the exact candidate without writing state.
func PreviewSoftwareBinding(ctx context.Context, keyPath string) (SoftwareBindingPreview, error) {
	paths, err := PathsForKey(keyPath)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	defer store.Close()
	return previewSoftwareBinding(ctx, store, paths, ResolveStableSoftwareInterface)
}

// ApplySoftwareBinding rechecks an exact preview before it writes any state.
func ApplySoftwareBinding(
	ctx context.Context,
	keyPath string,
	confirmation string,
) (SoftwareBinding, error) {
	paths, err := PathsForKey(keyPath)
	if err != nil {
		return SoftwareBinding{}, err
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		return SoftwareBinding{}, err
	}
	defer store.Close()
	if err := store.Lock(); err != nil {
		return SoftwareBinding{}, err
	}
	return applySoftwareBindingConfirmed(
		ctx, store, paths, confirmation, ResolveStableSoftwareInterface,
	)
}

func applySoftwareBindingConfirmed(
	ctx context.Context,
	store bindingStorage,
	paths BindingPaths,
	confirmation string,
	resolve func(context.Context) (StableInterface, error),
) (SoftwareBinding, error) {
	state, err := inspectBindingState(store, paths)
	if err != nil {
		return SoftwareBinding{}, err
	}
	if state.hasAny() {
		frozen, err := previewFrozenBinding(state)
		if err != nil {
			return SoftwareBinding{}, err
		}
		if confirmation == "" || confirmation != frozen.Confirmation {
			return SoftwareBinding{}, errors.New("home link adoption confirmation does not match the frozen candidate")
		}
		return finishFrozenBinding(store, paths, state)
	}
	preview, err := previewSoftwareBinding(ctx, store, paths, resolve)
	if err != nil {
		return SoftwareBinding{}, err
	}
	if confirmation == "" || confirmation != preview.Confirmation {
		return SoftwareBinding{}, errors.New("home link adoption confirmation does not match the current candidate")
	}
	return applySoftwareBinding(store, paths, preview)
}

// LoadSoftwareBinding validates or completes an already frozen binding. It
// never performs route lookup or chooses a MAC.
func LoadSoftwareBinding(keyPath string) (SoftwareBinding, error) {
	paths, err := PathsForKey(keyPath)
	if err != nil {
		return SoftwareBinding{}, err
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		return SoftwareBinding{}, err
	}
	defer store.Close()
	if err := store.Lock(); err != nil {
		return SoftwareBinding{}, err
	}
	return loadSoftwareBinding(store, paths)
}

func previewFrozenBinding(state bindingState) (SoftwareBindingPreview, error) {
	if state.markerBytes == nil {
		return SoftwareBindingPreview{}, fmt.Errorf("%w: sidecar lacks marker", ErrBindingIncomplete)
	}
	if state.keyBytes == nil {
		return SoftwareBindingPreview{}, fmt.Errorf("%w: canonical key is missing", ErrBindingIncomplete)
	}
	marker, err := decodeMarkerAnyState(state.markerBytes)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	if err := validateBindingAgainstKey(marker.Binding, state.keyBytes); err != nil {
		return SoftwareBindingPreview{}, err
	}
	expectedSidecar, err := encodeSidecar(marker.Binding)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	digest := sha256.Sum256(expectedSidecar)
	if marker.SidecarSHA256 != hex.EncodeToString(digest[:]) {
		return SoftwareBindingPreview{}, fmt.Errorf("%w: marker sidecar digest", ErrBindingMismatch)
	}
	if state.sidecarBytes == nil {
		if marker.State == "active" {
			return SoftwareBindingPreview{}, fmt.Errorf("%w: active marker lacks sidecar", ErrBindingIncomplete)
		}
	} else if !bytes.Equal(state.sidecarBytes, expectedSidecar) {
		return SoftwareBindingPreview{}, fmt.Errorf("%w: sidecar bytes", ErrBindingMismatch)
	}
	return bindingPreview(marker.Binding)
}

func previewSoftwareBinding(
	ctx context.Context,
	store bindingStorage,
	paths BindingPaths,
	resolve func(context.Context) (StableInterface, error),
) (SoftwareBindingPreview, error) {
	state, err := inspectBindingState(store, paths)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	if state.hasAny() {
		return SoftwareBindingPreview{}, errors.New("home link identity adoption has already started")
	}

	keyBytes, err := store.Read(filepath.Base(paths.Key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SoftwareBindingPreview{}, errors.New("canonical nova.key must exist before adoption")
		}
		return SoftwareBindingPreview{}, fmt.Errorf("read canonical key: %w", err)
	}
	publicKey, _, err := parseSoftwarePrivateKey(keyBytes)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	if resolve == nil {
		return SoftwareBindingPreview{}, errors.New("stable interface resolver is missing")
	}
	iface, err := resolve(ctx)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	confirmedKey, err := store.Read(filepath.Base(paths.Key))
	if err != nil {
		return SoftwareBindingPreview{}, fmt.Errorf("reopen canonical key before preview: %w", err)
	}
	if !bytes.Equal(confirmedKey, keyBytes) {
		return SoftwareBindingPreview{}, errors.New("canonical key changed during preview")
	}
	gatewayID, err := GatewayIDFromMAC(iface.PermanentMAC)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	publicHash := sha256.Sum256(publicKey)
	binding := SoftwareBinding{
		Version:         SoftwareBindingVersion,
		Source:          SourceSoftware,
		InterfaceName:   iface.Name,
		InterfaceIndex:  iface.Index,
		PermanentMAC:    iface.PermanentMAC.String(),
		GatewayID:       gatewayID,
		PublicKeySHA256: hex.EncodeToString(publicHash[:]),
	}
	if err := validateBinding(binding); err != nil {
		return SoftwareBindingPreview{}, err
	}
	return bindingPreview(binding)
}

func bindingPreview(binding SoftwareBinding) (SoftwareBindingPreview, error) {
	sidecar, err := encodeSidecar(binding)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	name, err := ThreeWordName(binding.GatewayID)
	if err != nil {
		return SoftwareBindingPreview{}, err
	}
	confirmDigest := sha256.New()
	_, _ = confirmDigest.Write([]byte("ftw-home-link-adoption-v1"))
	_, _ = confirmDigest.Write(sidecar)
	return SoftwareBindingPreview{
		Binding:        binding,
		ThreeWordName:  name,
		KeyFingerprint: binding.PublicKeySHA256,
		Confirmation:   hex.EncodeToString(confirmDigest.Sum(nil)),
	}, nil
}

func applySoftwareBinding(
	store bindingStorage,
	paths BindingPaths,
	preview SoftwareBindingPreview,
) (SoftwareBinding, error) {
	state, err := inspectBindingState(store, paths)
	if err != nil {
		return SoftwareBinding{}, err
	}
	if state.hasAny() {
		return finishConfirmedFrozenBinding(store, paths, state, preview)
	}
	if err := validateBindingAgainstKey(preview.Binding, state.keyBytes); err != nil {
		return SoftwareBinding{}, err
	}
	sidecar, err := encodeSidecar(preview.Binding)
	if err != nil {
		return SoftwareBinding{}, err
	}
	sidecarHash := sha256.Sum256(sidecar)
	pending, err := encodeMarker(bindingMarker{
		Version: SoftwareBindingVersion, State: "pending", Binding: preview.Binding,
		SidecarSHA256: hex.EncodeToString(sidecarHash[:]),
	})
	if err != nil {
		return SoftwareBinding{}, err
	}
	if err := store.InstallNoReplace(filepath.Base(paths.Marker), pending); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return SoftwareBinding{}, fmt.Errorf("install pending binding marker: %w", err)
		}
		// A concurrent winner froze its exact inputs. Do not repeat discovery.
		state, err = inspectBindingState(store, paths)
		if err != nil {
			return SoftwareBinding{}, err
		}
		return finishConfirmedFrozenBinding(store, paths, state, preview)
	}
	state, err = inspectBindingState(store, paths)
	if err != nil {
		return SoftwareBinding{}, err
	}
	return finishFrozenBinding(store, paths, state)
}

func finishConfirmedFrozenBinding(
	store bindingStorage,
	paths BindingPaths,
	state bindingState,
	confirmed SoftwareBindingPreview,
) (SoftwareBinding, error) {
	frozen, err := previewFrozenBinding(state)
	if err != nil {
		return SoftwareBinding{}, err
	}
	frozenSidecar, err := encodeSidecar(frozen.Binding)
	if err != nil {
		return SoftwareBinding{}, err
	}
	confirmedSidecar, err := encodeSidecar(confirmed.Binding)
	if err != nil {
		return SoftwareBinding{}, err
	}
	if frozen.Confirmation != confirmed.Confirmation ||
		frozen.Binding != confirmed.Binding ||
		!bytes.Equal(frozenSidecar, confirmedSidecar) {
		return SoftwareBinding{}, fmt.Errorf(
			"%w: concurrent binding does not match confirmed candidate",
			ErrBindingMismatch,
		)
	}
	return finishFrozenBinding(store, paths, state)
}

type bindingState struct {
	markerBytes  []byte
	sidecarBytes []byte
	keyBytes     []byte
}

func (s bindingState) hasAny() bool {
	return s.markerBytes != nil || s.sidecarBytes != nil
}

func inspectBindingState(store bindingStorage, paths BindingPaths) (bindingState, error) {
	var state bindingState
	files := []struct {
		path string
		dst  *[]byte
	}{
		{paths.Marker, &state.markerBytes},
		{paths.Sidecar, &state.sidecarBytes},
		{paths.Key, &state.keyBytes},
	}
	for _, file := range files {
		data, err := store.Read(filepath.Base(file.path))
		switch {
		case err == nil:
			*file.dst = data
		case errors.Is(err, fs.ErrNotExist):
		default:
			return bindingState{}, fmt.Errorf("read %s: %w", filepath.Base(file.path), err)
		}
	}
	if err := store.Revalidate(); err != nil {
		return bindingState{}, err
	}
	return state, nil
}

func loadSoftwareBinding(store bindingStorage, paths BindingPaths) (SoftwareBinding, error) {
	state, err := inspectBindingState(store, paths)
	if err != nil {
		return SoftwareBinding{}, err
	}
	if !state.hasAny() {
		return SoftwareBinding{}, ErrBindingNotAdopted
	}
	return finishFrozenBinding(store, paths, state)
}

func finishFrozenBinding(
	store bindingStorage,
	paths BindingPaths,
	state bindingState,
) (SoftwareBinding, error) {
	if state.markerBytes == nil {
		if state.sidecarBytes != nil {
			return SoftwareBinding{}, fmt.Errorf("%w: sidecar lacks marker", ErrBindingIncomplete)
		}
		return SoftwareBinding{}, ErrBindingNotAdopted
	}
	if state.keyBytes == nil {
		return SoftwareBinding{}, fmt.Errorf("%w: canonical key is missing", ErrBindingIncomplete)
	}
	marker, err := decodeMarkerAnyState(state.markerBytes)
	if err != nil {
		return SoftwareBinding{}, err
	}
	if err := validateBindingAgainstKey(marker.Binding, state.keyBytes); err != nil {
		return SoftwareBinding{}, err
	}
	expectedSidecar, err := encodeSidecar(marker.Binding)
	if err != nil {
		return SoftwareBinding{}, err
	}
	digest := sha256.Sum256(expectedSidecar)
	if marker.SidecarSHA256 != hex.EncodeToString(digest[:]) {
		return SoftwareBinding{}, fmt.Errorf("%w: marker sidecar digest", ErrBindingMismatch)
	}

	if state.sidecarBytes == nil {
		if marker.State == "active" {
			return SoftwareBinding{}, fmt.Errorf("%w: active marker lacks sidecar", ErrBindingIncomplete)
		}
		if err := store.InstallNoReplace(filepath.Base(paths.Sidecar), expectedSidecar); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return SoftwareBinding{}, fmt.Errorf("install binding sidecar: %w", err)
			}
		}
	} else if !bytes.Equal(state.sidecarBytes, expectedSidecar) {
		return SoftwareBinding{}, fmt.Errorf("%w: sidecar bytes", ErrBindingMismatch)
	}

	// Reopen every file after the sidecar install before making active state
	// durable. This also catches a swapped directory entry.
	state, err = inspectBindingState(store, paths)
	if err != nil {
		return SoftwareBinding{}, err
	}
	if state.markerBytes == nil || state.sidecarBytes == nil || state.keyBytes == nil {
		return SoftwareBinding{}, fmt.Errorf("%w: binding changed before activation", ErrBindingIncomplete)
	}
	reopenedMarker, err := decodeMarkerAnyState(state.markerBytes)
	if err != nil {
		return SoftwareBinding{}, err
	}
	active := marker
	active.State = "active"
	if (!bytes.Equal(state.markerBytes, mustEncodeMarker(marker)) &&
		!bytes.Equal(state.markerBytes, mustEncodeMarker(active))) ||
		reopenedMarker.Binding != marker.Binding ||
		reopenedMarker.SidecarSHA256 != marker.SidecarSHA256 ||
		!bytes.Equal(state.sidecarBytes, expectedSidecar) {
		return SoftwareBinding{}, fmt.Errorf("%w: binding changed before activation", ErrBindingMismatch)
	}
	if err := validateBindingAgainstKey(marker.Binding, state.keyBytes); err != nil {
		return SoftwareBinding{}, err
	}
	if reopenedMarker.State == "pending" {
		activeBytes, err := encodeMarker(active)
		if err != nil {
			return SoftwareBinding{}, err
		}
		if err := store.ReplaceExact(
			filepath.Base(paths.Marker), state.markerBytes, activeBytes,
		); err != nil {
			return SoftwareBinding{}, fmt.Errorf("activate binding marker: %w", err)
		}
	}

	finalState, err := inspectBindingState(store, paths)
	if err != nil {
		return SoftwareBinding{}, err
	}
	activeBytes := mustEncodeMarker(active)
	if !bytes.Equal(finalState.markerBytes, activeBytes) ||
		!bytes.Equal(finalState.sidecarBytes, expectedSidecar) ||
		finalState.keyBytes == nil {
		return SoftwareBinding{}, fmt.Errorf("%w: final binding files", ErrBindingMismatch)
	}
	if err := validateBindingAgainstKey(active.Binding, finalState.keyBytes); err != nil {
		return SoftwareBinding{}, err
	}
	if err := store.Revalidate(); err != nil {
		return SoftwareBinding{}, err
	}
	return active.Binding, nil
}

func validateBinding(binding SoftwareBinding) error {
	if binding.Version != SoftwareBindingVersion {
		return fmt.Errorf("binding version %d is unsupported", binding.Version)
	}
	if binding.Source != SourceSoftware {
		return fmt.Errorf("%w: source %q", ErrBindingMismatch, binding.Source)
	}
	if strings.TrimSpace(binding.InterfaceName) == "" || binding.InterfaceIndex <= 0 {
		return errors.New("binding interface identity is incomplete")
	}
	mac, err := net.ParseMAC(binding.PermanentMAC)
	if err != nil || len(mac) != 6 {
		return errors.New("binding permanent MAC is invalid")
	}
	if err := validatePhysicalInterface(physicalInterface{
		Name: binding.InterfaceName, Index: binding.InterfaceIndex, PermanentMAC: mac,
	}); err != nil {
		return err
	}
	wantID, err := GatewayIDFromMAC(mac)
	if err != nil {
		return err
	}
	gotID, err := NormalizeGatewayID(binding.GatewayID)
	if err != nil || gotID != wantID {
		return fmt.Errorf("%w: gateway id", ErrBindingMismatch)
	}
	publicHash, err := hex.DecodeString(binding.PublicKeySHA256)
	if err != nil || len(publicHash) != sha256.Size ||
		binding.PublicKeySHA256 != strings.ToLower(binding.PublicKeySHA256) {
		return errors.New("binding public key hash is invalid")
	}
	return nil
}

func validateBindingAgainstKey(binding SoftwareBinding, keyBytes []byte) error {
	if err := validateBinding(binding); err != nil {
		return err
	}
	publicKey, _, err := parseSoftwarePrivateKey(keyBytes)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(publicKey)
	if binding.PublicKeySHA256 != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("%w: canonical public key", ErrBindingMismatch)
	}
	return nil
}

func encodeSidecar(binding SoftwareBinding) ([]byte, error) {
	if err := validateBinding(binding); err != nil {
		return nil, err
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func encodeMarker(marker bindingMarker) ([]byte, error) {
	if marker.Version != SoftwareBindingVersion {
		return nil, errors.New("binding marker version is unsupported")
	}
	if marker.State != "pending" && marker.State != "active" {
		return nil, errors.New("binding marker state is invalid")
	}
	if err := validateBinding(marker.Binding); err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(marker.SidecarSHA256)
	if err != nil || len(digest) != sha256.Size ||
		marker.SidecarSHA256 != strings.ToLower(marker.SidecarSHA256) {
		return nil, errors.New("binding marker sidecar digest is invalid")
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func mustEncodeMarker(marker bindingMarker) []byte {
	data, err := encodeMarker(marker)
	if err != nil {
		panic(err)
	}
	return data
}

func decodeMarker(data []byte, state string) (bindingMarker, error) {
	marker, err := decodeMarkerAnyState(data)
	if err != nil {
		return bindingMarker{}, err
	}
	if marker.State != state {
		return bindingMarker{}, fmt.Errorf("%w: marker state %q", ErrBindingMismatch, marker.State)
	}
	return marker, nil
}

func decodeMarkerAnyState(data []byte) (bindingMarker, error) {
	if len(data) == 0 || len(data) > maxBindingFileBytes {
		return bindingMarker{}, errors.New("binding marker has invalid size")
	}
	var marker bindingMarker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return bindingMarker{}, fmt.Errorf("decode binding marker: %w", err)
	}
	canonical, err := encodeMarker(marker)
	if err != nil {
		return bindingMarker{}, err
	}
	if !bytes.Equal(data, canonical) {
		return bindingMarker{}, errors.New("binding marker is not canonical")
	}
	return marker, nil
}

func parseSoftwarePrivateKey(data []byte) ([]byte, *ecdsa.PrivateKey, error) {
	if len(data) == 0 || len(data) > maxBindingFileBytes {
		return nil, nil, errors.New("canonical key has invalid size")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "EC PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errors.New("canonical key is not one EC private key")
	}
	privateKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse canonical key: %w", err)
	}
	if privateKey.Curve != elliptic.P256() {
		return nil, nil, errors.New("canonical key is not P-256")
	}
	publicKey := append(
		privateKey.PublicKey.X.FillBytes(make([]byte, 32)),
		privateKey.PublicKey.Y.FillBytes(make([]byte, 32))...,
	)
	if err := ValidatePublicKey(publicKey); err != nil {
		return nil, nil, err
	}
	return publicKey, privateKey, nil
}
