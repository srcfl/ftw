package homelink

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestCredentialVerifierContainsOnlyPublicVerifierData(t *testing.T) {
	typeOf := reflect.TypeOf(CredentialVerifier{})
	fields := make([]string, 0, typeOf.NumField())
	for i := range typeOf.NumField() {
		fields = append(fields, typeOf.Field(i).Name)
	}
	want := []string{"CredentialID", "PublicKey", "Counter", "Label"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("credential verifier fields = %v, want %v", fields, want)
	}

	verifier := CredentialVerifier{
		CredentialID: []byte{1, 2, 3}, PublicKey: []byte{4, 5, 6}, Counter: 7, Label: "phone",
	}
	if err := verifier.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(verifier)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"credential_id", "public_key", "counter", "label"} {
		if _, ok := object[key]; !ok {
			t.Fatalf("verifier JSON lacks %q: %s", key, raw)
		}
	}
	if len(object) != 4 {
		t.Fatalf("verifier JSON has extra fields: %s", raw)
	}
}

func TestCredentialVerifierRejectsMissingPublicData(t *testing.T) {
	for _, verifier := range []CredentialVerifier{
		{PublicKey: []byte{1}, Label: "phone"},
		{CredentialID: []byte{1}, Label: "phone"},
		{CredentialID: []byte{1}, PublicKey: []byte{2}},
	} {
		if err := verifier.Validate(); err == nil {
			t.Fatalf("accepted invalid verifier: %+v", verifier)
		}
	}
}

func TestAssertionExpectationBindingUsesSessionAndMonotonicDeadline(t *testing.T) {
	firstSession := AssertionSession{}
	firstSession.id[0] = 1
	secondSession := AssertionSession{}
	secondSession.id[0] = 2
	binding := AssertionExpectationBinding{session: firstSession, deadline: 100}

	if err := binding.validate(firstSession, 99); err != nil {
		t.Fatalf("last valid instant = %v", err)
	}
	if err := binding.validate(firstSession, 100); !errors.Is(err, ErrAssertionExpired) {
		t.Fatalf("exact deadline = %v", err)
	}
	if err := binding.validate(secondSession, 0); !errors.Is(err, ErrAssertionSession) {
		t.Fatalf("another manager session = %v", err)
	}
}
