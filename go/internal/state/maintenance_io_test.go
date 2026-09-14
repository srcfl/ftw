package state

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type maintenanceTestFile struct {
	bytes.Buffer
	dirty, peak, syncs int
	syncErr            error
	short              bool
}

func (f *maintenanceTestFile) Write(p []byte) (int, error) {
	if f.short {
		p = p[:len(p)/2]
	}
	n, err := f.Buffer.Write(p)
	f.dirty += n
	f.peak = max(f.peak, f.dirty)
	return n, err
}

func (f *maintenanceTestFile) Sync() error {
	f.syncs++
	f.dirty = 0
	return f.syncErr
}

func TestMaintenanceWriterBoundsDirtyOutput(t *testing.T) {
	f := &maintenanceTestFile{}
	pauses := 0
	w := &maintenanceWriter{ctx: context.Background(), file: f, limit: 4, pause: func(context.Context) error { pauses++; return nil }}
	for _, p := range []string{"abc", "defghijklmnopq"} {
		if n, err := w.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatal(n, err)
		}
	}
	if f.String() != "abcdefghijklmnopq" || f.peak != 4 || f.dirty != 1 || f.syncs != 4 || pauses != 4 {
		t.Fatalf("unexpected output/pacing: %+v pauses=%d", f, pauses)
	}
}

func TestMaintenanceWriterStopsOnIOFailureOrCancellation(t *testing.T) {
	failed := errors.New("sync failed")
	for _, kind := range []string{"sync", "short", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := &maintenanceTestFile{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want, wantN := failed, 4
			w := &maintenanceWriter{ctx: ctx, file: f, limit: 4, pause: func(context.Context) error { return nil }}
			switch kind {
			case "sync":
				f.syncErr = failed
			case "short":
				f.short = true
				want, wantN = io.ErrShortWrite, 2
			case "cancel":
				w.pause = func(context.Context) error { cancel(); return ctx.Err() }
				want = context.Canceled
			}
			if n, err := w.Write([]byte("abcdefgh")); n != wantN || !errors.Is(err, want) {
				t.Fatalf("write returned %d, %v", n, err)
			}
			if f.Len() != wantN {
				t.Fatal("wrote beyond failure", f.Len())
			}
		})
	}
}
