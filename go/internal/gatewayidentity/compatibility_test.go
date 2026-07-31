package gatewayidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"testing"
)

func sha256Sum(message []byte) [sha256.Size]byte { return sha256.Sum256(message) }

func TestGatewayIDFromMACCompatibilityVectors(t *testing.T) {
	// Frozen Sourceful Energy Gateway compatibility vectors.
	vectors := map[string]string{
		"dc:a6:32:f8:38:f7": "0123dca63201f838f7",
		"aa:bb:cc:dd:ee:ff": "0123aabbcc01ddeeff",
		"00:00:00:00:00:00": "012300000001000000",
	}
	for raw, want := range vectors {
		mac, err := net.ParseMAC(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := GatewayIDFromMAC(mac)
		if err != nil || got != want {
			t.Errorf("GatewayIDFromMAC(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestNameWordListCompatibilityHashes(t *testing.T) {
	// Frozen Sourceful Energy Gateway hashes. Each hash covers every word
	// in order, joined by a newline and ending in a newline.
	vectors := []struct {
		name  string
		words []string
		want  string
	}{
		{"adjectives", gatewayAdjectives[:], "fa1cbef437e20bf4626a5c3c3a18effb22ead483d514438c14f1b03d1dc53ef1"},
		{"colors", gatewayColors[:], "4956d4b537fa655e8be4662a7b8ba1ac28f3f6523fb502711857857da97348b2"},
		{"animals", gatewayAnimals[:], "105257b32ac128a4223c09260c287d034dcb456f8f68d97721b9947ceb603b1c"},
	}
	for _, vector := range vectors {
		digest := sha256.Sum256([]byte(strings.Join(vector.words, "\n") + "\n"))
		if got := hex.EncodeToString(digest[:]); got != vector.want {
			t.Errorf("%s word-list hash = %s, want %s", vector.name, got, vector.want)
		}
	}
}

func TestThreeWordNameCompatibilityVectors(t *testing.T) {
	// Frozen Sourceful Energy Gateway vectors. The FTW map has no
	// hand-written exceptions.
	vectors := map[string]string{
		"012300bcdec52af201": "flat-aegean-aphid",
		"0123dca63201f838f7": "interesting-iron-pike",
		"000000000000000000": "short-maroon-cow",
		"ffffffffffffffffff": "brilliant-pewter-zebra",
		"0123aabbcc01ddeeff": "shallow-stone-badger",
	}
	for id, want := range vectors {
		got, err := ThreeWordName(id)
		if err != nil || got != want {
			t.Errorf("ThreeWordName(%q) = %q, %v; want %q", id, got, err, want)
		}
	}
}
