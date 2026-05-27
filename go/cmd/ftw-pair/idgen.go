package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
)

// Tiny memorable wordlist for the routing-key token. 64 words → 6
// words ≈ 36 bits. The token is NOT the access secret (host approval
// with 4-digit voice-channel cross-check is); 36 bits is plenty for a
// routing key against rate-limited attackers.
var wordlist = []string{
	"alpha", "amber", "arrow", "atom", "axis", "bay", "berry", "beam",
	"belt", "boat", "bolt", "brave", "brook", "buzz", "calm", "candle",
	"cap", "cliff", "cloud", "code", "comet", "core", "cosy", "cube",
	"daisy", "dawn", "delta", "drift", "echo", "ember", "fern", "field",
	"flame", "flax", "fjord", "forge", "frost", "garnet", "gem", "glade",
	"glow", "gray", "grove", "harbor", "haven", "hill", "honey", "iris",
	"ivory", "jade", "jet", "key", "knot", "lake", "lark", "leaf",
	"linen", "loft", "lyric", "marble", "meadow", "mint", "moss", "north",
}

func pickWord() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(wordlist))))
	return wordlist[n.Int64()]
}

func genWordToken() string {
	parts := make([]string, 6)
	for i := range parts {
		parts[i] = pickWord()
	}
	return parts[0] + "-" + parts[1] + "-" + parts[2] + "-" + parts[3] + "-" + parts[4] + "-" + parts[5]
}

func genApprovalCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(10000))
	return fmt.Sprintf("%04d", n.Int64())
}

func randomHostID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "host-" + hex.EncodeToString(b)
}
