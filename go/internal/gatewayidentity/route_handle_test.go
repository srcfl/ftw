package gatewayidentity

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestRouteHandleCompatibilityVectors(t *testing.T) {
	vectors := []struct {
		publicKey string
		want      string
	}{
		{
			"6b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296" +
				"4fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5",
			"6KQ3tpEjr--3KU_Ad9-IrJKE7NjtsSZmXsE7eNqUELw",
		},
		{
			"7cf27b188d034f7e8a52380304b51ac3c08969e277f21b35a60b48fc47669978" +
				"07775510db8ed040293d9ac69f7430dbba7dade63ce982299e04b79d227873d1",
			"5ZzA-WRRRjg7PKEcXw7Kqx_cw6iAeQzEsZ3s5k0E9x8",
		},
	}
	for _, vector := range vectors {
		publicKey, err := hex.DecodeString(vector.publicKey)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RouteHandle(publicKey)
		if err != nil {
			t.Fatal(err)
		}
		if got != vector.want {
			t.Fatalf("RouteHandle() = %q, want %q", got, vector.want)
		}
		if len(got) != RouteHandleSize || strings.Contains(got, "=") {
			t.Fatalf("route handle wire form = %q", got)
		}
	}
}

func TestRouteHandleRejectsInvalidKeys(t *testing.T) {
	for _, key := range [][]byte{
		nil,
		make([]byte, PublicKeyBytes-1),
		make([]byte, PublicKeyBytes),
		make([]byte, PublicKeyBytes+1),
	} {
		if _, err := RouteHandle(key); err == nil {
			t.Fatalf("accepted invalid public key of length %d", len(key))
		}
	}
}

func TestRouteHandleDomainSeparationIsFrozen(t *testing.T) {
	if RouteHandleDomain != "ftw-home-link-route-v1" {
		t.Fatalf("route handle domain = %q", RouteHandleDomain)
	}
}
