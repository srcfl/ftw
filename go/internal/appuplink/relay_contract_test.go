package appuplink

import "testing"

// The relay's close codes and control words, written out from srcfl/ftw-webapp
// relay/README.md.
//
// They are duplicated here rather than imported, because the relay is
// TypeScript and the box must build without it. Duplication without a check is
// how two implementations drift, so the check is this: the numbers below are
// copied from the document and compared against the constants the code uses.
// Changing one without the other fails here rather than in a house.
func TestRelayCloseCodesMatchTheDocument(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"bad join, text on a binary channel, or an oversize message", CloseBadJoin, 4400},
		{"wrong epoch; the reason carries the right one", CloseEpoch, 4409},
		{"the epoch turned over; the reason carries the new one", CloseRotated, 4410},
		{"rate limited, room occupied, or the process is at its ceiling", CloseBusy, 4429},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d, want %d", c.name, c.got, c.want)
		}
	}

	if CtrlReady != "ready" {
		t.Errorf("ready word = %q", CtrlReady)
	}
	if CtrlGone != "gone" {
		t.Errorf("gone word = %q", CtrlGone)
	}
}

// An hour, matching src/lib/carrier/rendezvous.ts. The two ends disagreeing
// costs a round trip rather than correctness, because the relay corrects a
// wrong guess — but only while the guesses are in the same units.
func TestEpochLengthMatchesTheApp(t *testing.T) {
	if EpochMs != 3_600_000 {
		t.Fatalf("epoch is %d ms, want one hour", EpochMs)
	}
	if HandleBytes != 16 {
		t.Fatalf("handle is %d bytes, want 16", HandleBytes)
	}
}
