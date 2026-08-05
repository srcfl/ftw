package loadmodel

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func openTestDB(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// trainedModel returns a model with a recognisable amount of learning in it.
func trainedModel(t *testing.T) *Model {
	t.Helper()
	m := newProfileModel(4000, ProfileHome)
	m.HeatingW_per_degC = 275
	t0 := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		m.Update(t0.AddDate(0, 0, 7*i), 2400, HeatingReferenceC)
	}
	if m.Samples != 12 {
		t.Fatalf("fixture did not train: samples = %d", m.Samples)
	}
	return m
}

// requireLegacyAdoption skips a case that depends on pre-envelope state still
// being adopted. Once the features move, that state is correctly discarded and
// the case stops existing — the migration and the tests that cover it retire
// together, so a feature change never leaves a test demanding the old
// behaviour back.
func requireLegacyAdoption(t *testing.T) {
	t.Helper()
	if FeatureHash() != legacyFeatureHash {
		t.Skip("features have moved on; unversioned state is discarded, as designed")
	}
}

// TestFeatureHashPinned is a tripwire, not a rule. Changing the bucket
// indexing or the heating shape is allowed, but it makes every deployed site
// discard its learned week — and unlike the PV twin, which relearns in an
// afternoon, bucket coverage is rebuilt over weeks. That should be a
// decision, not a surprise found in production.
//
// If this test fails: confirm the change is intended, update the literal
// below, and say in the changeset that the load model cold-starts on upgrade.
// Do NOT touch legacyFeatureHash in service.go — that constant is frozen on
// purpose, and moving it would restore pre-envelope coefficients under the
// new features, which is the exact fault this guards against.
func TestFeatureHashPinned(t *testing.T) {
	const want = "f79385ff0412d66b"
	if got := FeatureHash(); got != want {
		t.Errorf("load feature hash = %q, pinned at %q\n"+
			"the feature definition changed: every deployed site will cold-start", got, want)
	}
}

// The hash must move on its own when the model's definitions do, so nobody
// has to remember to bump a constant.
func TestFeatureHashTracksTheFeatureDefinition(t *testing.T) {
	base := modelstate.Fingerprint(featureSemantics, featureProbe())
	if p := featureProbe(); p[0] != float64(Buckets) || p[1] != HeatingReferenceC {
		t.Fatalf("featureProbe layout changed; update this test: %v", p[:2])
	}

	// Bucket indexing moving off UTC — the 3255deba regression, which
	// silently misaligned every learned bucket across a DST change.
	shifted := featureProbe()
	shifted[2]++ // the first HourOfWeek probe value
	if modelstate.Fingerprint(featureSemantics, shifted) == base {
		t.Error("a change to bucket indexing must move the hash")
	}

	// The heating shape the slope is measured against.
	steeper := featureProbe()
	steeper[1] = HeatingReferenceC + 2
	if modelstate.Fingerprint(featureSemantics, steeper) == base {
		t.Error("a change to the heating reference must move the hash")
	}

	// The semantics half: same math, different meaning for the sampled load.
	if modelstate.Fingerprint("loadmodel/1 load=site_w_less_pv_bat", featureProbe()) == base {
		t.Error("redeclaring what an input means must move the hash")
	}
}

// Retuning typicalPrior must NOT cold-start a site. A bucket mean is measured
// watts and stays meaningful when the prior it started from changes; the
// prior only supplies the fallback for buckets nobody has observed. Weighed
// against weeks of lost coverage, that trade is deliberate — see featureProbe.
func TestPriorIsNotPartOfTheFeatureIdentity(t *testing.T) {
	probe := featureProbe()
	for bucket := 0; bucket < Buckets; bucket++ {
		p := typicalPrior(bucket)
		for i, v := range probe {
			if v == p {
				t.Fatalf("probe[%d] carries typicalPrior(%d) = %v: retuning the "+
					"prior would cold-start every site for no safety gain", i, bucket, p)
			}
		}
	}
}

func TestPersistedStateRoundTrips(t *testing.T) {
	st := openTestDB(t)

	s := NewService(st, telemetry.NewStore(), "site", 4000, 17250)
	s.mu.Lock()
	s.models[ProfileHome] = trainedModel(t)
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	restored := NewService(st, telemetry.NewStore(), "site", 4000, 17250).Model()
	if restored.Samples != 12 {
		t.Fatalf("samples = %d, want 12: matching feature hash must restore", restored.Samples)
	}
	if restored.HeatingW_per_degC != 275 {
		t.Errorf("heating slope = %v, want 275", restored.HeatingW_per_degC)
	}
}

// The bug this PR exists for: bucket means and a heating slope fitted against
// one feature definition must not be restored into a build that computes
// another. They parse, they look sane, and they steer the plan.
func TestStateFittedAgainstOtherFeaturesColdStarts(t *testing.T) {
	st := openTestDB(t)

	js, err := modelstate.Wrap("0000badfeature00", trainedModel(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(stateKey(ProfileHome), js); err != nil {
		t.Fatal(err)
	}

	got := NewService(st, telemetry.NewStore(), "site", 4000, 17250).Model()

	if got.Samples != 0 {
		t.Errorf("samples = %d, want 0: stale coefficients must not survive a feature change", got.Samples)
	}
	if got.HeatingW_per_degC != 0 {
		t.Errorf("heating slope = %v, want 0 on a cold start", got.HeatingW_per_degC)
	}
}

// State written before the envelope existed is adopted, on both the
// per-profile key and the pre-profile one. Upgrading must not cost every site
// its learned week.
func TestUnversionedStateIsAdopted(t *testing.T) {
	requireLegacyAdoption(t)
	for name, key := range map[string]string{
		"per-profile key": stateKey(ProfileHome),
		"pre-profile key": legacyStateKey,
	} {
		t.Run(name, func(t *testing.T) {
			st := openTestDB(t)
			bare, err := json.Marshal(trainedModel(t)) // pre-envelope on-disk shape
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SaveConfig(key, string(bare)); err != nil {
				t.Fatal(err)
			}

			got := NewService(st, telemetry.NewStore(), "site", 4000, 17250).Model()

			if got.Samples != 12 {
				t.Fatalf("samples = %d, want 12: unversioned state should be adopted", got.Samples)
			}
			if got.PeakW != 4000 || got.MaxPlausibleW != 17250 {
				t.Errorf("config-owned fields not reapplied: peak %v, max %v", got.PeakW, got.MaxPlausibleW)
			}
		})
	}
}

// The pre-profile key is the oldest state on any box. It now runs the same
// bucket repair the per-profile path has always run, so a model poisoned by
// the pre-guard heating-subtraction bug is repaired whichever key it sits in.
func TestLegacyKeyGetsTheBucketRepair(t *testing.T) {
	requireLegacyAdoption(t)
	st := openTestDB(t)

	poisoned := trainedModel(t)
	poisoned.Bucket[0].Mean = 15 // prior for bucket 0 is ~300 W
	poisoned.Bucket[0].Samples = 400
	bare, err := json.Marshal(poisoned)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(legacyStateKey, string(bare)); err != nil {
		t.Fatal(err)
	}

	got := NewService(st, telemetry.NewStore(), "site", 4000, 17250).Model()

	if got.Bucket[0].Mean != got.prior(0) {
		t.Errorf("bucket 0 mean = %.1f, want the prior %.1f", got.Bucket[0].Mean, got.prior(0))
	}
}

// A damaged blob on the boot path of a control system cold-starts. It does
// not panic, and it does not leave a half-decoded model behind.
func TestCorruptStateColdStarts(t *testing.T) {
	for name, js := range map[string]string{
		"truncated envelope": `{"schema_version":1,"feature_hash":"f79385ff0412d66b","model":{"samples":`,
		"not json":           "\x00\x01\x02 not json",
		"wrong shape":        `[1,2,3]`,
		"no model":           `{"schema_version":1,"feature_hash":"f79385ff0412d66b"}`,
		"future schema":      `{"schema_version":99,"feature_hash":"f79385ff0412d66b","model":{"samples":900,"alpha":0.1}}`,
		"half-decoded":       `{"bucket":"not-an-array","samples":900,"alpha":0.1}`,
		"no alpha":           `{"samples":900}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := openTestDB(t)
			if err := st.SaveConfig(stateKey(ProfileHome), js); err != nil {
				t.Fatal(err)
			}
			got := NewService(st, telemetry.NewStore(), "site", 4000, 17250).Model()
			if got.Samples != 0 {
				t.Errorf("samples = %d, want 0 (cold start)", got.Samples)
			}
			if got.Alpha != newProfileModel(4000, ProfileHome).Alpha {
				t.Errorf("alpha = %v, want the cold-start value", got.Alpha)
			}
		})
	}
}
