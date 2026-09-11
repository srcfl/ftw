package pvmodel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
)

// storedModel reads a persisted blob the way the service does. Tests assert
// against the model inside the envelope, not the envelope itself.
func storedModel(t *testing.T, js string) Model {
	t.Helper()
	var m Model
	res := modelstate.Unwrap(js, FeatureHash(), legacyFeatureHash, &m)
	if !res.OK() {
		t.Fatalf("stored state did not restore: %s", res.Reason)
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

// TestFeatureHashPinned is a tripwire, not a rule. Changing the feature
// vector is allowed and sometimes necessary — the roofmodel work changes what
// clearSkyW means — but it makes every deployed PV twin throw away its
// coefficients and relearn. That should be a decision, not a surprise found
// in production.
//
// If this test fails: confirm the feature change is intended, update the
// literal below, and say in the changeset that the PV model cold-starts on
// upgrade. Do NOT touch legacyFeatureHash in service.go — that constant is
// frozen on purpose, and moving it would restore pre-envelope coefficients
// under the new features, which is the exact fault this guards against.
func TestFeatureHashPinned(t *testing.T) {
	const want = "fd42eb2a7c1e9f55"
	if got := FeatureHash(); got != want {
		t.Errorf("PV feature hash = %q, pinned at %q\n"+
			"the feature vector changed: every deployed twin will cold-start", got, want)
	}
}

// The hash must move on its own when the features do. This is the property
// that makes the guard hard to forget: nobody has to remember to bump a
// constant when they add a harmonic.
func TestFeatureHashTracksTheFeatureMath(t *testing.T) {
	base := modelstate.Fingerprint(featureSemantics, featureProbe())

	// Reviving the dead intercept slot — the #134 regression.
	revived := featureProbe()
	revived[0] = 1.0
	if modelstate.Fingerprint(featureSemantics, revived) == base {
		t.Error("reviving the intercept slot must move the hash")
	}

	// A third time-of-day harmonic.
	extended := append(append([]float64{}, featureProbe()...), 0.5)
	if modelstate.Fingerprint(featureSemantics, extended) == base {
		t.Error("adding a feature must move the hash")
	}

	// The semantics half: same math, different meaning for clearSkyW. This is
	// what the roofmodel family changes, and no probe can see it.
	if modelstate.Fingerprint("pvmodel/1 clearsky=plane_of_array_wm2", featureProbe()) == base {
		t.Error("redeclaring what an input means must move the hash")
	}
}

func TestPersistedStateRoundTrips(t *testing.T) {
	db := openTestDB(t)
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	svc := NewService(db, nil, cs, cl, 5000)
	svc.mu.Lock()
	svc.model.Samples = 137
	svc.model.MAE = 42
	svc.model.Beta[3] = 1.25
	svc.mu.Unlock()
	svc.persist()

	restored := NewService(db, nil, cs, cl, 5000).Model()
	if restored.Samples != 137 || restored.MAE != 42 || restored.Beta[3] != 1.25 {
		t.Fatalf("matching feature hash must restore the model, got %+v", restored)
	}
}

// The bug this PR exists for: coefficients fitted against one feature space
// must not be restored into a build that computes another. They parse, they
// look sane, and they steer the plan from a different world.
func TestStateFittedAgainstOtherFeaturesColdStarts(t *testing.T) {
	db := openTestDB(t)
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	trained := NewModel(5000)
	trained.Samples = 900
	trained.MAE = 7
	trained.Beta[3] = 99
	js, err := modelstate.Wrap("0000badfeature00", trained)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveConfig(stateKey, js); err != nil {
		t.Fatal(err)
	}

	got := NewService(db, nil, cs, cl, 5000).Model()

	if got.Samples != 0 {
		t.Errorf("samples = %d, want 0: stale coefficients must not survive a feature change", got.Samples)
	}
	if got.Beta[3] != 0 {
		t.Errorf("Beta[3] = %v, want 0", got.Beta[3])
	}
	if got.Beta[2] != 5000.0/1000 {
		t.Errorf("Beta[2] = %v, want the cold-start physics prior", got.Beta[2])
	}
}

// State written before the envelope existed is adopted, because the feature
// space it was fitted against is still the one this build computes. Upgrading
// must not cost every site its learned twin.
func TestUnversionedStateIsAdopted(t *testing.T) {
	requireLegacyAdoption(t)
	db := openTestDB(t)
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	trained := NewModel(5000)
	trained.Samples = 480
	trained.MAE = 31
	trained.Beta[3] = 0.75
	bare, err := json.Marshal(trained) // pre-envelope on-disk shape
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveConfig(stateKey, string(bare)); err != nil {
		t.Fatal(err)
	}

	got := NewService(db, nil, cs, cl, 5000).Model()

	if got.Samples != 480 || got.Beta[3] != 0.75 {
		t.Fatalf("unversioned state should be adopted, got %+v", got)
	}
}

// The pre-#134 migration: a drifted intercept is zeroed on load. It survives
// the move to the envelope, on both the versioned and the unversioned path.
func TestBetaZeroMigrationSurvivesEnvelope(t *testing.T) {
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	drifted := NewModel(5000)
	drifted.Samples = 300
	drifted.Beta[0] = 812 // intercept that drifted before #134

	versioned, err := modelstate.Wrap(FeatureHash(), drifted)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := json.Marshal(drifted)
	if err != nil {
		t.Fatal(err)
	}

	for name, js := range map[string]string{"versioned": versioned, "unversioned": string(bare)} {
		t.Run(name, func(t *testing.T) {
			if name == "unversioned" {
				requireLegacyAdoption(t)
			}
			db := openTestDB(t)
			if err := db.SaveConfig(stateKey, js); err != nil {
				t.Fatal(err)
			}
			got := NewService(db, nil, cs, cl, 5000).Model()
			if got.Samples != 300 {
				t.Fatalf("model should have restored, got %+v", got)
			}
			if got.Beta[0] != 0 {
				t.Errorf("Beta[0] = %v, want 0: the #134 migration must still run", got.Beta[0])
			}
		})
	}
}

// A damaged blob on the boot path of a control system cold-starts. It does
// not panic, and it does not leave a half-decoded model behind.
func TestCorruptStateColdStarts(t *testing.T) {
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	for name, js := range map[string]string{
		"truncated envelope": `{"schema_version":1,"feature_hash":"fd42eb2a7c1e9f55","model":{"samples":`,
		"not json":           "\x00\x01\x02 not json",
		"wrong shape":        `[1,2,3]`,
		"no model":           `{"schema_version":1,"feature_hash":"fd42eb2a7c1e9f55"}`,
		"future schema":      `{"schema_version":99,"feature_hash":"fd42eb2a7c1e9f55","model":{"samples":900,"forgetting":0.995}}`,
		"half-decoded":       `{"beta":"not-an-array","samples":900,"forgetting":0.995}`,
	} {
		t.Run(name, func(t *testing.T) {
			db := openTestDB(t)
			if err := db.SaveConfig(stateKey, js); err != nil {
				t.Fatal(err)
			}
			got := NewService(db, nil, cs, cl, 5000).Model()
			if got.Samples != 0 {
				t.Errorf("samples = %d, want 0 (cold start)", got.Samples)
			}
			if got.Forgetting != NewModel(5000).Forgetting {
				t.Errorf("forgetting = %v, want the cold-start value", got.Forgetting)
			}
		})
	}
}

// The planner reads RelMAE on every replan, so it has to survive a restart —
// otherwise every reboot drops a learned site back onto the flat haircut.
func TestRelMAERoundTrips(t *testing.T) {
	db := openTestDB(t)
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	svc := NewService(db, nil, cs, cl, 5000)
	svc.mu.Lock()
	svc.model.Samples = 400
	svc.model.RelMAE = 0.27
	svc.mu.Unlock()
	svc.persist()

	restored := NewService(db, nil, cs, cl, 5000)
	if got := restored.Model().RelMAE; got != 0.27 {
		t.Fatalf("restored RelMAE = %v, want 0.27", got)
	}
	if got := restored.RelativeUncertainty(); got != 0.27 {
		t.Fatalf("RelativeUncertainty() = %v, want 0.27", got)
	}
}

// State persisted before #1020 has no rel_mae. It must load as 0 — "not
// learned yet" — so the site keeps the flat haircut instead of refusing the
// blob or hedging on a garbage number.
func TestStateWithoutRelMAELoadsAsUnlearned(t *testing.T) {
	db := openTestDB(t)
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	trained := NewModel(5000)
	trained.Samples = 900
	trained.MAE = 180
	legacy := map[string]any{
		"beta":       trained.Beta,
		"p":          trained.P,
		"forgetting": trained.Forgetting,
		"samples":    trained.Samples,
		"last_ms":    trained.LastMs,
		"mae":        trained.MAE,
		"rated_w":    trained.RatedW,
	}
	js, err := modelstate.Wrap(FeatureHash(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveConfig(stateKey, js); err != nil {
		t.Fatal(err)
	}

	svc := NewService(db, nil, cs, cl, 5000)
	got := svc.Model()
	if got.Samples != 900 || got.MAE != 180 {
		t.Fatalf("pre-#1020 state must still restore, got %+v", got)
	}
	if got.RelMAE != 0 {
		t.Errorf("RelMAE = %v, want 0 when the field is absent", got.RelMAE)
	}
	if svc.RelativeUncertainty() != 0 {
		t.Errorf("RelativeUncertainty() = %v, want 0", svc.RelativeUncertainty())
	}
}
