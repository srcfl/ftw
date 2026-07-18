// Package sitecontroller produces the versioned, read-only FTW Site Companion
// pairing and snapshot envelopes consumed by Nova Core.
package sitecontroller

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

const (
	PairingContractVersion  = "ftw-site-controller-pairing/v1"
	SnapshotContractVersion = "ftw-site-controller-snapshot/v1"
	ControllerKindFTW       = "ftw"

	ScopeStatusRead = "ftw.status.read"
	ScopeHealthRead = "ftw.health.read"
	ScopePlanRead   = "ftw.plan.read"

	PairingTTL  = 5 * time.Minute
	SnapshotTTL = time.Minute
	MaxActions  = 24
)

var (
	controllerIDRE = regexp.MustCompile(`^ftw-[0-9a-f]{32}$`)
	siteIDRE       = regexp.MustCompile(`^sit-[A-Za-z0-9-]{8,80}$`)
	anchorIDRE     = regexp.MustCompile(`^zap-[A-Za-z0-9-]{3,80}$`)
)

var readScopes = []string{ScopeHealthRead, ScopePlanRead, ScopeStatusRead}

func ReadScopes() []string {
	return append([]string(nil), readScopes...)
}

type Signer interface {
	PublicKeyHex() string
	SignRawHex(message string) (string, error)
}

type PairingPayload struct {
	ContractVersion string  `json:"contract_version"`
	ControllerKind  string  `json:"controller_kind"`
	PublicKey       string  `json:"public_key"`
	Nonce           string  `json:"nonce"`
	ExpiresAtMS     int64   `json:"expires_at_ms"`
	AnchorGatewayID *string `json:"anchor_gateway_id,omitempty"`
}

type PairingEnvelope struct {
	Payload   PairingPayload `json:"payload"`
	Signature string         `json:"signature"`
}

type SnapshotPayload struct {
	ContractVersion string         `json:"contract_version"`
	ControllerKind  string         `json:"controller_kind"`
	ControllerID    string         `json:"controller_id"`
	SiteID          string         `json:"site_id"`
	SnapshotID      string         `json:"snapshot_id"`
	ObservedAtMS    int64          `json:"observed_at_ms"`
	ExpiresAtMS     int64          `json:"expires_at_ms"`
	Status          StatusSnapshot `json:"status"`
	Health          HealthSnapshot `json:"health"`
	Plan            PlanSnapshot   `json:"plan"`
}

type StatusSnapshot struct {
	SoftwareVersion string `json:"software_version"`
	Mode            string `json:"mode"`
	PlanStale       bool   `json:"plan_stale"`
}

type HealthSnapshot struct {
	State           string `json:"state"`
	DriversOK       int    `json:"drivers_ok"`
	DriversDegraded int    `json:"drivers_degraded"`
	DriversOffline  int    `json:"drivers_offline"`
	DriversFaulted  int    `json:"drivers_faulted"`
}

type PlanSnapshot struct {
	Enabled       bool         `json:"enabled"`
	GeneratedAtMS int64        `json:"generated_at_ms,omitempty"`
	Mode          string       `json:"mode,omitempty"`
	ActionCount   int          `json:"action_count"`
	Actions       []PlanAction `json:"actions"`
}

type PlanAction struct {
	StartMS  int64 `json:"start_ms"`
	EndMS    int64 `json:"end_ms"`
	BatteryW int   `json:"battery_w"`
	GridW    int   `json:"grid_w"`
}

type SnapshotEnvelope struct {
	Payload   SnapshotPayload `json:"payload"`
	Signature string          `json:"signature"`
}

func ControllerID(publicKey string) (string, error) {
	raw, err := hex.DecodeString(publicKey)
	if err != nil || len(raw) != 64 {
		return "", errors.New("site controller: public key must be 64-byte X||Y hex")
	}
	sum := sha256.Sum256([]byte(publicKey))
	return "ftw-" + hex.EncodeToString(sum[:16]), nil
}

func NewPairing(signer Signer, anchorGatewayID *string, now time.Time, random io.Reader) (*PairingEnvelope, error) {
	if signer == nil {
		return nil, errors.New("site controller: identity unavailable")
	}
	if random == nil {
		random = rand.Reader
	}
	publicKey := signer.PublicKeyHex()
	if _, err := ControllerID(publicKey); err != nil {
		return nil, err
	}
	if anchorGatewayID != nil && !anchorIDRE.MatchString(*anchorGatewayID) {
		return nil, errors.New("site controller: invalid Zap anchor identity")
	}
	nonce := make([]byte, 32)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, fmt.Errorf("site controller: generate pairing nonce: %w", err)
	}
	payload := PairingPayload{
		ContractVersion: PairingContractVersion,
		ControllerKind:  ControllerKindFTW,
		PublicKey:       publicKey,
		Nonce:           base64.RawURLEncoding.EncodeToString(nonce),
		ExpiresAtMS:     now.Add(PairingTTL).UnixMilli(),
		AnchorGatewayID: anchorGatewayID,
	}
	message, err := PairingMessage(payload)
	if err != nil {
		return nil, err
	}
	signature, err := signer.SignRawHex(string(message))
	if err != nil {
		return nil, err
	}
	return &PairingEnvelope{Payload: payload, Signature: signature}, nil
}

func NewSnapshot(signer Signer, siteID string, status StatusSnapshot, health HealthSnapshot, plan PlanSnapshot, now time.Time, random io.Reader) (*SnapshotEnvelope, error) {
	if signer == nil {
		return nil, errors.New("site controller: identity unavailable")
	}
	if !siteIDRE.MatchString(siteID) {
		return nil, errors.New("site controller: invalid site_id")
	}
	if random == nil {
		random = rand.Reader
	}
	controllerID, err := ControllerID(signer.PublicKeyHex())
	if err != nil {
		return nil, err
	}
	if !controllerIDRE.MatchString(controllerID) {
		return nil, errors.New("site controller: invalid controller identity")
	}
	if len(plan.Actions) > MaxActions {
		plan.Actions = append([]PlanAction(nil), plan.Actions[:MaxActions]...)
	}
	if plan.ActionCount < len(plan.Actions) {
		plan.ActionCount = len(plan.Actions)
	}
	snapshotNonce := make([]byte, 18)
	if _, err := io.ReadFull(random, snapshotNonce); err != nil {
		return nil, fmt.Errorf("site controller: generate snapshot id: %w", err)
	}
	payload := SnapshotPayload{
		ContractVersion: SnapshotContractVersion,
		ControllerKind:  ControllerKindFTW,
		ControllerID:    controllerID,
		SiteID:          siteID,
		SnapshotID:      "snp-" + base64.RawURLEncoding.EncodeToString(snapshotNonce),
		ObservedAtMS:    now.UnixMilli(),
		ExpiresAtMS:     now.Add(SnapshotTTL).UnixMilli(),
		Status:          status,
		Health:          health,
		Plan:            plan,
	}
	message, err := SnapshotMessage(payload)
	if err != nil {
		return nil, err
	}
	signature, err := signer.SignRawHex(string(message))
	if err != nil {
		return nil, err
	}
	return &SnapshotEnvelope{Payload: payload, Signature: signature}, nil
}

func PairingMessage(payload PairingPayload) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append([]byte("srcful.site-controller.pairing.v1\n"), b...), nil
}

func SnapshotMessage(payload SnapshotPayload) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append([]byte("srcful.site-controller.snapshot.v1\n"), b...), nil
}
