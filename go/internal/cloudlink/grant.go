package cloudlink

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The link and the grant live in the box's state store, not in the config
// document, so a whole-document config write cannot reset them. A missing
// record reads as off.
const (
	linkKey          = "cloud/link_v1"
	grantKey         = "cloud/grant_v1"
	rosterKey        = "cloud/roster_v1"
	purgedKey        = "cloud/purged_v1"
	disabledKey      = "cloud/disabled_v1"
	statsDecisionKey = "cloud/statistics_decision_v1"
	// statsShareKey is the one statistics answer of a box without FTW Cloud:
	// "on" (the owner said yes), "default" (on, never answered) or "off"
	// (MigrateStatistics, SharesStatistics).
	statsShareKey = "statistics/share_v1"
)

// linkRecord is the box's link to FTW Cloud: which cloud.key it holds. The
// link carries the roster and the phones' tunnels, and a grant adds the
// readable copy on top.
type linkRecord struct {
	// KeyFp is SHA-256 of cloud.key's public point. A record whose
	// fingerprint does not match the key on disk reads as off.
	KeyFp string `json:"keyFp"`
	// Ending means the box asked the service to delete the home and waits
	// for the receipt. The key stays until it arrives.
	Ending bool `json:"ending,omitempty"`
	// Host is the instance of the service that holds this home, as a
	// wrong-shard close named it. Empty is Endpoint's own host.
	Host string `json:"host,omitempty"`
}

// KV is the part of the state store the grant needs.
type KV interface {
	LoadConfig(key string) (string, bool)
	SaveConfig(key, value string) error
}

// grantRecord is what an owner turned on.
type grantRecord struct {
	ID       []byte   `json:"id"`
	Scopes   []string `json:"scopes"`
	Purposes []string `json:"purposes"`
	// KeyFp is SHA-256 of cloud.key's public point. A record whose
	// fingerprint does not match the key on disk reads as off: a grant
	// restored without its key must not start a link under a new key.
	KeyFp string `json:"keyFp"`
	// GrantKey cuts the opaque ids. A new grant draws a new one.
	GrantKey []byte `json:"grantKey"`
	By       string `json:"by"`
	Policy   string `json:"policy"`
	// AtMs is the wall time of the grant, or 0 when the clock was not set.
	AtMs       int64  `json:"atMs"`
	Controller string `json:"controller"`
	// StatsID names this box's statistics rows at the service. Drawn when an
	// owner first turns statistics on, kept while they stay on, and gone
	// when they are turned off.
	StatsID []byte `json:"statsId,omitempty"`
	// StatsPolicy names the consent text the owner read for statistics.
	StatsPolicy string `json:"statsPolicy,omitempty"`
	StatsAtMs   int64  `json:"statsAtMs,omitempty"`
	// Revoked means the owner turned the readable copy off and the service
	// has not yet confirmed that the copy is gone. The record stays until it
	// has; the link and its key stay after.
	Revoked     bool  `json:"revoked,omitempty"`
	RevokedAtMs int64 `json:"revokedAtMs,omitempty"`
}

func (g *grantRecord) has(scope string) bool {
	return g != nil && slices.Contains(g.Scopes, scope)
}

// receipt is the service's purge receipt, kept so the box can say when the
// copy was erased.
type receipt struct {
	// What is the receipt's: copy for a grant, home for the link.
	What    string `json:"what,omitempty"`
	AtMs    int64  `json:"atMs"`
	GrantID []byte `json:"grantId"`
}

// rosterState is the roster number and what it was computed from. The number
// rises when the digest changes, so changes made while the link was down
// still reach the service.
type rosterState struct {
	Seq    uint64 `json:"seq"`
	Digest string `json:"digest"`
}

func loadJSON[T any](kv KV, key string) (*T, error) {
	raw, ok := kv.LoadConfig(key)
	if !ok || raw == "" {
		return nil, nil
	}
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("cloudlink: reading %s: %w", key, err)
	}
	return &v, nil
}

// saveJSON writes v, or clears the key when v is nil. The store has no
// delete, and an empty value reads as missing.
func saveJSON(kv KV, key string, v any) error {
	if v == nil {
		return kv.SaveConfig(key, "")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return kv.SaveConfig(key, string(raw))
}

// ErrGrantRequest is a grant asked for with something FTW Cloud cannot have.
var ErrGrantRequest = errors.New("cloudlink: not a grant FTW Cloud may hold")

// checkScopes refuses anything outside cloud_scopes, including every write
// scope, and an empty list.
func checkScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, fmt.Errorf("%w: name at least one scope", ErrGrantRequest)
	}
	out := make([]string, 0, len(scopes))
	for _, s := range protocol.CloudScopes {
		if slices.Contains(scopes, s) {
			out = append(out, s)
		}
	}
	for _, s := range scopes {
		if !slices.Contains(protocol.CloudScopes, s) {
			return nil, fmt.Errorf("%w: %q", ErrGrantRequest, s)
		}
	}
	return out, nil
}

// --------------------------------------------------------------------------
// cloud.key
// --------------------------------------------------------------------------

// The key file is PEM SEC 1, 0600, beside state.db. Backups skip it: a box
// restored onto other hardware links again under a new home id, and has the
// readable copy off until an owner turns it on again.

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("cloudlink: %s is not an EC private key", path)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cloudlink: %s: %w", path, err)
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("cloudlink: %s is not P-256", path)
	}
	return key, nil
}

// createKey makes cloud.key, and never replaces one: a key on disk is a
// home id phones may already dial, and a silent new one would strand them.
// os.Link fails when the file exists, so a race with another writer cannot
// replace it either.
func createKey(path string) (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, fmt.Errorf("cloudlink: writing %s: %w", tmp, err)
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		return nil, fmt.Errorf("cloudlink: %s: %w", path, err)
	}
	return key, nil
}

// fingerprint is SHA-256 of the key's public point, in hex.
func fingerprint(key *ecdsa.PrivateKey) (string, []byte, error) {
	pk, err := cloudwire.PublicKeyBytesOf(&key.PublicKey)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(pk)
	return hex.EncodeToString(sum[:]), pk, nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
