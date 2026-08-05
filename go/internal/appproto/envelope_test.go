package appproto

import (
	"encoding/hex"
	"testing"
)

func TestEnvelopeRoundTrips(t *testing.T) {
	in, err := newEnvelope(MsgTick, ptrU32(17), Tick{Seq: 5, UptimeMs: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeEnvelope(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.T != MsgTick || out.ID == nil || *out.ID != 17 {
		t.Fatalf("envelope = %+v", out)
	}
	var tick Tick
	if err := Unmarshal(out.B, &tick); err != nil {
		t.Fatal(err)
	}
	if tick.Seq != 5 || tick.UptimeMs != 60_000 {
		t.Fatalf("tick = %+v", tick)
	}
}

// Deterministic encoding: the same message is the same bytes every time, so a
// frame's length never depends on which map order a runtime happened to pick.
func TestEncodingIsDeterministic(t *testing.T) {
	build := func() []byte {
		env, err := newEnvelope(MsgDelta, nil, Delta{
			Seq:      3,
			UptimeMs: 61_000,
			Fields: map[string]int64{
				fidKey(FidLoadW): 3700, fidKey(FidGridW): 1200,
				fidKey(FidPvW): -3400, fidKey(FidBatteryW): 900,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := EncodeEnvelope(env)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	first := hex.EncodeToString(build())
	for i := 0; i < 20; i++ {
		if got := hex.EncodeToString(build()); got != first {
			t.Fatalf("encoding %d differed:\n%s\n%s", i, first, got)
		}
	}
}

// An envelope with no type cannot be routed, and guessing would be worse than
// dropping it.
func TestEnvelopeWithoutATypeIsRefused(t *testing.T) {
	raw, err := encMode.Marshal(map[string]any{"id": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeEnvelope(raw); err == nil {
		t.Fatal("a typeless envelope decoded")
	}
}

// A duplicate key is two different claims about the same field and there is no
// safe way to pick one. An unknown key is just a newer peer, and is dropped.
func TestDuplicateKeysAreRefusedButUnknownOnesAreNot(t *testing.T) {
	// {"t": "tick", "t": "delta"} written by hand, because no encoder will
	// produce it.
	dup, err := hex.DecodeString("a261746474696367616474" + "6564656c7461")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeEnvelope(dup); err == nil {
		t.Fatal("an envelope with a duplicate key decoded")
	}

	extra, err := encMode.Marshal(map[string]any{"t": MsgTick, "b": map[string]any{
		"seq": 1, "uptimeMs": 2, "telepathy": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := DecodeEnvelope(extra)
	if err != nil {
		t.Fatalf("an unknown key was treated as fatal: %v", err)
	}
	var tick Tick
	if err := Unmarshal(env.B, &tick); err != nil {
		t.Fatal(err)
	}
	if tick.Seq != 1 {
		t.Fatalf("tick = %+v", tick)
	}
}

func TestBulkBucketStepsUpRatherThanGrowing(t *testing.T) {
	cases := []struct {
		payload int
		want    int
	}{
		{0, 1024},
		{1018, 1024},
		{1019, 4096},
		{4090, 4096},
		{4091, 16384},
		{16379, 0},
	}
	for _, c := range cases {
		if got := bulkBucketFor(c.payload); got != c.want {
			t.Fatalf("bulkBucketFor(%d) = %d, want %d", c.payload, got, c.want)
		}
	}
}
