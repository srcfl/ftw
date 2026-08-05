package modelstate

import (
	"encoding/json"
	"strings"
	"testing"
)

type toyModel struct {
	Beta  []float64 `json:"beta"`
	Alpha float64   `json:"alpha"`
}

const (
	hashA = "aaaaaaaaaaaaaaaa"
	hashB = "bbbbbbbbbbbbbbbb"
)

func TestWrapUnwrapRoundTrip(t *testing.T) {
	want := toyModel{Beta: []float64{1, 2.5, -3}, Alpha: 0.1}
	js, err := Wrap(hashA, want)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	var got toyModel
	res := Unwrap(js, hashA, hashB, &got)
	if res.Outcome != Restored {
		t.Fatalf("outcome = %v (%s), want Restored", res.Outcome, res.Reason)
	}
	if res.StoredHash != hashA {
		t.Errorf("stored hash = %q, want %q", res.StoredHash, hashA)
	}
	if len(got.Beta) != 3 || got.Beta[1] != 2.5 || got.Alpha != 0.1 {
		t.Errorf("round trip lost the model: %+v", got)
	}
}

// A model fitted against one feature space must never be restored into a
// build that computes another. The whole point of the envelope: the
// coefficients still parse, still look sane, and are still wrong.
func TestUnwrapDiscardsOnFeatureHashMismatch(t *testing.T) {
	js, err := Wrap(hashA, toyModel{Beta: []float64{9, 9, 9}, Alpha: 0.1})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	got := toyModel{Beta: []float64{1}} // caller's cold-start value
	res := Unwrap(js, hashB, hashB, &got)

	if res.OK() {
		t.Fatalf("outcome = %v, want Discard", res.Outcome)
	}
	if res.StoredHash != hashA {
		t.Errorf("stored hash = %q, want %q so the log line can name both", res.StoredHash, hashA)
	}
	if res.Reason == "" {
		t.Error("a discard must carry a reason; it is the only thing the operator sees")
	}
	if len(got.Beta) != 1 || got.Beta[0] != 1 {
		t.Errorf("destination was written on a discard: %+v", got)
	}
}

// The envelope's own layout can change. When it does, everything written by
// the previous layout must cold-start rather than be read as this one.
func TestUnwrapDiscardsUnknownSchemaVersion(t *testing.T) {
	js := `{"schema_version":999,"feature_hash":"` + hashA + `","model":{"alpha":0.1}}`

	var got toyModel
	res := Unwrap(js, hashA, hashA, &got)

	if res.OK() {
		t.Fatalf("outcome = %v, want Discard", res.Outcome)
	}
	if !strings.Contains(res.Reason, "999") {
		t.Errorf("reason %q should name the schema it found", res.Reason)
	}
}

// Boot path of a control system: damaged state cold-starts, it does not
// panic and it does not half-populate the model.
func TestUnwrapCorruptBlobColdStarts(t *testing.T) {
	cases := map[string]string{
		"truncated":      `{"schema_version":1,"feature_hash":"aaaa`,
		"not json":       "this is not json at all",
		"empty":          "",
		"whitespace":     "   \n\t ",
		"wrong shape":    `[1,2,3]`,
		"model garbage":  `{"schema_version":1,"feature_hash":"` + hashA + `","model":"not-an-object"}`,
		"model null":     `{"schema_version":1,"feature_hash":"` + hashA + `","model":null}`,
		"nul bytes":      "\x00\x00\x00",
		"legacy garbage": `{"beta":"should-be-numbers","alpha":0.1}`,
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			var got toyModel
			res := Unwrap(js, hashA, hashA, &got)
			if res.OK() {
				t.Fatalf("outcome = %v, want Discard (reason %q)", res.Outcome, res.Reason)
			}
			if got.Alpha != 0 || got.Beta != nil {
				t.Errorf("destination written on a discard: %+v", got)
			}
		})
	}
}

// An unversioned blob is state written before this package existed. It is
// adopted only while the running build still computes the feature space it
// was fitted against.
func TestUnwrapAdoptsUnversionedBlobUnderMatchingLegacyHash(t *testing.T) {
	bare, err := json.Marshal(toyModel{Beta: []float64{4, 5}, Alpha: 0.2})
	if err != nil {
		t.Fatal(err)
	}

	var got toyModel
	res := Unwrap(string(bare), hashA, hashA, &got)

	if res.Outcome != RestoredLegacy {
		t.Fatalf("outcome = %v (%s), want RestoredLegacy", res.Outcome, res.Reason)
	}
	if !res.Legacy() {
		t.Error("Legacy() must report an adopted unversioned blob so the log can say so")
	}
	if res.StoredHash != "" {
		t.Errorf("stored hash = %q, want empty: an unversioned blob carries none", res.StoredHash)
	}
	if got.Alpha != 0.2 {
		t.Errorf("unversioned blob not restored: %+v", got)
	}
}

// The migration retires itself. Once the features move, the caller's frozen
// legacyFeatureHash no longer matches, and unversioned state is discarded
// like any other stale fit — with nobody having to remember to delete this
// path.
func TestUnwrapDiscardsUnversionedBlobOnceFeaturesMove(t *testing.T) {
	bare, err := json.Marshal(toyModel{Beta: []float64{4, 5}, Alpha: 0.2})
	if err != nil {
		t.Fatal(err)
	}

	var got toyModel
	res := Unwrap(string(bare), hashB, hashA, &got) // build moved on from hashA

	if res.OK() {
		t.Fatalf("outcome = %v, want Discard", res.Outcome)
	}
	if got.Alpha != 0 {
		t.Errorf("destination written on a discard: %+v", got)
	}
}

func TestFingerprintIsStableAndSensitive(t *testing.T) {
	base := []float64{0, 1.5, -2.25, 1e6}

	if a, b := Fingerprint("s", base), Fingerprint("s", base); a != b {
		t.Fatalf("fingerprint not stable: %q vs %q", a, b)
	}

	// The declared semantics is part of the identity: same numbers, different
	// meaning, different fingerprint.
	if Fingerprint("s", base) == Fingerprint("s2", base) {
		t.Error("semantics label must change the fingerprint")
	}

	// A change to the feature math moves the probe values.
	if Fingerprint("s", base) == Fingerprint("s", []float64{0, 1.5, -2.25, 1e6 + 1}) {
		t.Error("changed probe value must change the fingerprint")
	}

	// Adding a feature lengthens the probe.
	if Fingerprint("s", base) == Fingerprint("s", append(append([]float64{}, base...), 0)) {
		t.Error("added probe value must change the fingerprint")
	}

	// Reordering a slot is exactly the change that stays invisible in a
	// per-value check but invalidates every coefficient.
	if Fingerprint("s", base) == Fingerprint("s", []float64{1.5, 0, -2.25, 1e6}) {
		t.Error("reordered probe must change the fingerprint")
	}

	// The separator is what makes reordering visible even when neighbouring
	// values would otherwise concatenate to the same digits.
	if Fingerprint("s", []float64{1, 23}) == Fingerprint("s", []float64{12, 3}) {
		t.Error("probe values must not run together")
	}
}

// Rounding to twelve significant digits absorbs the sub-ULP difference the
// same expression can produce on a different CPU, so a state database stays
// portable between the build host and the Pi.
func TestFingerprintIgnoresSubULPDifference(t *testing.T) {
	v := 0.8660254037844386
	nudged := v + 1e-16 // ~1 ULP at this magnitude
	if v == nudged {
		t.Skip("nudge below float64 resolution on this platform")
	}
	if Fingerprint("s", []float64{v}) != Fingerprint("s", []float64{nudged}) {
		t.Error("a one-ULP difference must not invalidate learned state")
	}
}
