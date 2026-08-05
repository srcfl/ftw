// Package modelstate versions the learned-model state FTW keeps on disk.
//
// A learned model is a set of coefficients that means nothing on its own. It
// means something only against the feature vector it was fitted to. Restore a
// PV Beta fitted against two time-of-day harmonics into a build that computes
// three, or fitted against horizontal irradiance into one that projects onto
// the array plane, and the model keeps predicting — with a plausible-looking
// MAE — from coefficients that describe a different world. Nothing in the
// numbers gives it away, and the plan is steered by the result.
//
// FTW has paid for this class of fault twice: commit 3255deba, where
// local-time bucket indexing misaligned the learned models across a DST
// change, and 41e59efb, where the models locked themselves out. Both were
// found from their effects, long after the change that caused them.
//
// So each persisted model is wrapped in {schema_version, feature_hash,
// model}, where feature_hash fingerprints the feature space its coefficients
// belong to. On any mismatch the stored state is discarded and the model cold
// starts. Both learned models recover from a cold start in bounded time.
// Neither recovers from silently wrong coefficients.
package modelstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Version is the envelope's own schema version. Bump it when the envelope
// layout changes: every stored model then fails to restore and cold starts,
// which is the right outcome for a record this build can no longer read.
const Version = 1

type envelope struct {
	SchemaVersion int             `json:"schema_version"`
	FeatureHash   string          `json:"feature_hash"`
	Model         json.RawMessage `json:"model"`
}

// Wrap serialises a model together with the fingerprint of the feature space
// it was fitted against.
func Wrap(featureHash string, model any) (string, error) {
	raw, err := json.Marshal(model)
	if err != nil {
		return "", err
	}
	js, err := json.Marshal(envelope{
		SchemaVersion: Version,
		FeatureHash:   featureHash,
		Model:         raw,
	})
	if err != nil {
		return "", err
	}
	return string(js), nil
}

// Outcome is what Unwrap decided about a stored blob.
type Outcome int

const (
	// Discard means the state could not be trusted; the caller cold starts
	// and the destination is left as the caller supplied it.
	Discard Outcome = iota
	// Restored means the envelope matched and the destination is populated.
	Restored
	// RestoredLegacy means an unversioned blob was adopted — see Unwrap.
	RestoredLegacy
)

// Result carries what Unwrap did, with enough detail to log it.
type Result struct {
	Outcome Outcome
	// StoredHash is the fingerprint found on disk. Empty for an unversioned
	// blob, which carries none.
	StoredHash string
	// Reason says why the state was discarded. Empty when it was restored.
	Reason string
}

// OK reports whether the destination was populated.
func (r Result) OK() bool { return r.Outcome != Discard }

// Legacy reports whether the state restored was an unversioned blob.
func (r Result) Legacy() bool { return r.Outcome == RestoredLegacy }

// Unwrap restores stored state into out.
//
// featureHash is the fingerprint of the feature space the running build
// computes. legacyFeatureHash is the fingerprint that was in force when the
// caller adopted the envelope: state written before then carries no
// fingerprint of its own, but we know which feature space produced it, so it
// is still worth restoring — and only while the running build still computes
// that same space. The first change to the features moves featureHash away
// from the caller's frozen legacyFeatureHash, and unversioned state is
// discarded from then on like any other stale fit. Nobody has to remember to
// retire the migration; it retires itself.
//
// Corrupt input is a discard, never a panic, and a discard never writes to
// out — the caller's cold-start value survives intact. This is
// operator-writable state read on the boot path of a control system, so a
// half-decoded model must not be able to reach the planner.
func Unwrap(js, featureHash, legacyFeatureHash string, out any) Result {
	trimmed := strings.TrimSpace(js)
	if trimmed == "" || trimmed == "null" {
		return Result{Reason: "empty"}
	}
	var env envelope
	if err := json.Unmarshal([]byte(js), &env); err != nil {
		return Result{Reason: "unreadable: " + err.Error()}
	}
	if env.SchemaVersion == 0 && len(env.Model) == 0 {
		// No envelope fields at all: an unversioned blob written before this
		// package existed. json.Unmarshal ignores unknown fields, so a bare
		// model lands here with every envelope field at its zero value.
		if featureHash != legacyFeatureHash {
			return Result{Reason: "unversioned state predates the current feature space"}
		}
		if err := decodeInto([]byte(js), out); err != nil {
			return Result{Reason: "unreadable: " + err.Error()}
		}
		return Result{Outcome: RestoredLegacy}
	}
	if env.SchemaVersion != Version {
		return Result{
			StoredHash: env.FeatureHash,
			Reason:     fmt.Sprintf("envelope schema %d, this build reads %d", env.SchemaVersion, Version),
		}
	}
	if env.FeatureHash != featureHash {
		return Result{StoredHash: env.FeatureHash, Reason: "fitted against a different feature space"}
	}
	if len(env.Model) == 0 || string(env.Model) == "null" {
		return Result{StoredHash: env.FeatureHash, Reason: "envelope carries no model"}
	}
	if err := decodeInto(env.Model, out); err != nil {
		return Result{StoredHash: env.FeatureHash, Reason: "unreadable: " + err.Error()}
	}
	return Result{Outcome: Restored, StoredHash: env.FeatureHash}
}

// decodeInto is json.Unmarshal with all-or-nothing semantics. Plain
// Unmarshal populates the fields it understands before it reaches the one it
// cannot, so a blob truncated or damaged halfway through leaves a model that
// is part restored state and part cold start — a mixture nothing downstream
// can detect. Decode into a scratch value of the same type and copy over only
// on success.
func decodeInto(raw []byte, out any) error {
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("destination must be a non-nil pointer, got %T", out)
	}
	scratch := reflect.New(rv.Elem().Type())
	if err := json.Unmarshal(raw, scratch.Interface()); err != nil {
		return err
	}
	rv.Elem().Set(scratch.Elem())
	return nil
}

// Fingerprint derives a stable identifier for a feature space from a declared
// semantics label and the output of the feature functions over a fixed probe.
//
// The probe is what makes this hard to get wrong. Add a harmonic, reorder a
// slot, change an exponent, and the probe values move, so the fingerprint
// moves, with nobody having to remember to bump a constant — which is exactly
// the step that gets forgotten. The semantics label covers the one thing a
// probe cannot see: a caller feeding a differently defined quantity into an
// unchanged feature function.
func Fingerprint(semantics string, probe []float64) string {
	h := sha256.New()
	h.Write([]byte(semantics))
	for _, v := range probe {
		// Separator, so that concatenated digits cannot make two different
		// probes hash alike.
		h.Write([]byte{0})
		// Twelve significant digits: far finer than any real change to a
		// feature produces, far coarser than the sub-ULP spread the same
		// expression can have between the amd64 host that built the release
		// and the arm64 Pi that runs the plant. A state database stays
		// portable between them.
		h.Write([]byte(strconv.FormatFloat(v, 'g', 12, 64)))
	}
	// 64 bits is ample for detecting a change and short enough to read in a
	// log line next to the hash it failed to match.
	return hex.EncodeToString(h.Sum(nil))[:16]
}
