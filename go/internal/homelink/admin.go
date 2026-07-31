package homelink

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/state"
)

const HomeLinkBrowserURL = "https://home.sourceful.energy/home-link.html"

type RemoteRuntimeStatus struct {
	mu        sync.RWMutex
	connected bool
	lastError string
	changedAt time.Time
}

type RuntimeSnapshot struct {
	Connected bool      `json:"connected"`
	LastError string    `json:"last_error,omitempty"`
	ChangedAt time.Time `json:"changed_at,omitempty"`
}

func (s *RemoteRuntimeStatus) SetConnected(connected bool, err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.connected = connected
	s.lastError = ""
	if err != nil {
		s.lastError = "connection-failed"
	}
	s.changedAt = time.Now().UTC()
	s.mu.Unlock()
}

func (s *RemoteRuntimeStatus) Snapshot() RuntimeSnapshot {
	if s == nil {
		return RuntimeSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return RuntimeSnapshot{
		Connected: s.connected, LastError: s.lastError, ChangedAt: s.changedAt,
	}
}

type CredentialSummary struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	CreatedAtMS int64  `json:"created_at_ms"`
	UpdatedAtMS int64  `json:"updated_at_ms"`
}

type AdminStatus struct {
	Enabled       bool                `json:"enabled"`
	IdentityReady bool                `json:"identity_ready"`
	GatewayID     string              `json:"gateway_id,omitempty"`
	RouteHandle   string              `json:"route_handle,omitempty"`
	InviteURL     string              `json:"invite_url,omitempty"`
	Runtime       RuntimeSnapshot     `json:"runtime"`
	Credentials   []CredentialSummary `json:"credentials"`
}

type PairingSetup struct {
	ID        string    `json:"id"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
}

type LocalAdmin struct {
	enabled     bool
	gatewayID   string
	routeHandle string
	publicKey   string
	pairing     *LocalPairingManager
	authority   *PersistentCredentialAuthority
	store       *state.Store
	runtime     *RemoteRuntimeStatus
}

func NewLocalAdmin(
	enabled bool,
	identity gatewayidentity.Identity,
	store *state.Store,
	pairing *LocalPairingManager,
	authority *PersistentCredentialAuthority,
	runtime *RemoteRuntimeStatus,
) (*LocalAdmin, error) {
	if identity == nil || store == nil || pairing == nil || authority == nil {
		return nil, errors.New("Home Link local setup is incomplete")
	}
	if err := gatewayidentity.Validate(identity); err != nil {
		return nil, err
	}
	if authority.CredentialSite().ID() != identity.GatewayID() {
		return nil, errors.New("Home Link local setup belongs to another gateway")
	}
	routeHandle, err := gatewayidentity.RouteHandle(identity.PublicKey())
	if err != nil {
		return nil, err
	}
	return &LocalAdmin{
		enabled: enabled, gatewayID: identity.GatewayID(),
		routeHandle: routeHandle,
		publicKey:   base64.RawURLEncoding.EncodeToString(identity.PublicKey()),
		pairing:     pairing, authority: authority, store: store, runtime: runtime,
	}, nil
}

func (a *LocalAdmin) Status(ctx context.Context) (AdminStatus, error) {
	if a == nil {
		return AdminStatus{Credentials: []CredentialSummary{}}, nil
	}
	records, err := a.store.ActiveHomeLinkCredentials(ctx, a.gatewayID)
	if err != nil {
		return AdminStatus{}, errors.New("read Home Link credentials")
	}
	credentials := make([]CredentialSummary, len(records))
	for index, record := range records {
		credentials[index] = CredentialSummary{
			ID:    base64.RawURLEncoding.EncodeToString(record.CredentialID),
			Label: record.Label, CreatedAtMS: record.CreatedAtMS,
			UpdatedAtMS: record.UpdatedAtMS,
		}
	}
	invite, err := url.Parse(HomeLinkBrowserURL)
	if err != nil {
		return AdminStatus{}, err
	}
	query := invite.Query()
	query.Set("gateway", a.gatewayID)
	query.Set("route", a.routeHandle)
	query.Set("key", a.publicKey)
	invite.RawQuery = query.Encode()
	return AdminStatus{
		Enabled: a.enabled, IdentityReady: true, GatewayID: a.gatewayID,
		RouteHandle: a.routeHandle, InviteURL: invite.String(),
		Runtime: a.runtime.Snapshot(), Credentials: credentials,
	}, nil
}

func (a *LocalAdmin) CreatePairing() (PairingSetup, error) {
	if a == nil || !a.enabled {
		return PairingSetup{}, ErrRemoteDisabled
	}
	challenge, err := a.pairing.Create(PairingGrantMaxTTL)
	if err != nil {
		return PairingSetup{}, err
	}
	return PairingSetup{
		ID:        challenge.ID,
		Secret:    base64.RawURLEncoding.EncodeToString(challenge.Secret),
		ExpiresAt: challenge.ExpiresAt,
	}, nil
}

func (a *LocalAdmin) BeginRegistration(
	ctx context.Context,
	pairingID string,
	pairingSecret string,
	label string,
) (RegistrationChallenge, error) {
	if a == nil || !a.enabled {
		return RegistrationChallenge{}, ErrRemoteDisabled
	}
	secret, err := base64.RawURLEncoding.DecodeString(pairingSecret)
	if err != nil || base64.RawURLEncoding.EncodeToString(secret) != pairingSecret {
		return RegistrationChallenge{}, ErrRegistrationDenied
	}
	return a.authority.BeginRegistration(ctx, LocalPairingProof{
		Challenge: []byte(pairingID), Response: secret,
	}, label)
}

func (a *LocalAdmin) FinishRegistration(
	ctx context.Context,
	expectationID string,
	responseJSON []byte,
) (CredentialSummary, error) {
	if a == nil || !a.enabled {
		return CredentialSummary{}, ErrRemoteDisabled
	}
	credential, err := a.authority.FinishRegistration(
		ctx, expectationID,
		PasskeyRegistration{ResponseJSON: slices.Clone(responseJSON)},
	)
	if err != nil {
		return CredentialSummary{}, err
	}
	return CredentialSummary{
		ID:    base64.RawURLEncoding.EncodeToString(credential.CredentialID),
		Label: credential.Label,
	}, nil
}

func (a *LocalAdmin) RevokeCredential(
	ctx context.Context,
	credentialID string,
) error {
	if a == nil {
		return ErrCredentialUnknown
	}
	raw, err := base64.RawURLEncoding.DecodeString(credentialID)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != credentialID {
		return ErrCredentialUnknown
	}
	return a.authority.RevokeCredential(ctx, raw)
}
