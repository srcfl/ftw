//go:build !windows

package ftwdbshadow

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestRustSidecarInteropShutdownDrainsPendingAndQueued(t *testing.T) {
	binary, reconcile := os.Getenv("FTWDB_SHADOW_BIN"), os.Getenv("FTWDB_RECONCILE_BIN")
	if binary == "" || reconcile == "" {
		t.Skip("set FTWDB_SHADOW_BIN and FTWDB_RECONCILE_BIN for the pinned Rust gate")
	}
	for _, tc := range []struct {
		name    string
		count   int
		pending int
	}{
		{"13-before-first-interval", 13, 0},
		{"256-before-first-interval", 256, 0},
		{"pending-and-full-queue", 257, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "ftw-drain-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			store, socket := filepath.Join(root, "shadow"), filepath.Join(root, "run", "shadow.sock")
			stop := startRustShadow(t, binary, store, socket)
			firstDurable, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			proxy, frames := shadowProxy(t, socket, func() {
				if tc.pending != 0 {
					close(firstDurable)
					<-release
				}
			})
			st, err := state.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			var b *Beta
			if tc.pending == 0 {
				b = Start(context.Background(), st, proxy, "test-site", "test")
			} else {
				b = runTestBeta(t, st, proxy)
			}
			t.Cleanup(b.Close)
			for i := 1; i <= tc.count; i++ {
				if err := st.RecordTick(state.HistoryPoint{TsMs: int64(i), GridW: float64(i) + 0.125, PVW: -1000, BatSoC: 0.75}, nil); err != nil {
					t.Fatal(err)
				}
				if i == 1 && tc.pending != 0 {
					select {
					case <-firstDurable:
					case <-time.After(3 * time.Second):
						t.Fatal("first commit did not reach Rust")
					}
				}
			}
			if s := b.Status(); s.Acknowledged != 0 || s.Pending != tc.pending || s.Queued != tc.count-tc.pending {
				t.Fatalf("test did not retain the expected pending and queued ticks: %+v", s)
			}
			started := time.Now()
			if tc.pending != 0 {
				// Cancel while a durable commit is still awaiting its receipt. The
				// same prepared bytes must survive the switch to the drain context.
				b.cancel()
				unblock()
			}
			b.Close()
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("shutdown took %v", elapsed)
			}
			s := b.Status()
			if s.Acknowledged != uint64(tc.count) || s.DurableThrough != uint64(tc.count) || s.Pending != 0 || s.Queued != 0 || s.Dropped != 0 || (tc.pending == 0 && s.Errors == 0) {
				t.Fatalf("shutdown failed to recover the lost ACK and drain the queue: %+v", s)
			}
			wire := frames()
			if len(wire) < 2 || !bytes.Equal(wire[0], wire[1]) {
				t.Fatalf("shutdown retry changed the commit: %d frames", len(wire))
			}
			args := []string{store}
			seen := make(map[uint64]bool)
			for _, frame := range wire {
				prepared, err := PreparedCommitFromFrame(frame)
				if err != nil {
					t.Fatal(err)
				}
				if !seen[prepared.Sequence()] {
					seen[prepared.Sequence()] = true
					framePath := filepath.Join(root, fmt.Sprintf("expected-%d.hex", prepared.Sequence()))
					if err := os.WriteFile(framePath, []byte(hex.EncodeToString(frame)), 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, framePath)
				}
			}
			stop(syscall.SIGKILL)
			output, err := exec.Command(reconcile, args...).CombinedOutput()
			if err != nil || !strings.Contains(string(output), `"content_matches":true`) {
				t.Fatalf("shutdown readback failed: %v\n%s", err, output)
			}
		})
	}
}

func TestBetaShutdownBoundsUnresponsiveSidecar(t *testing.T) {
	for _, stage := range []string{"hello", "durable-ack"} {
		t.Run(stage, func(t *testing.T) {
			source := mustID(t, "00112233445566778899aabbccddeeff")
			listener := listenUnix(t)
			blocked := make(chan struct{})
			server := runServer(listener, func(conn net.Conn) error {
				if stage == "hello" {
					if _, err := ReadMessage(conn); err != nil {
						return err
					}
				} else {
					if err := serverHello(conn, source); err != nil {
						return err
					}
					message, err := ReadMessage(conn)
					if err != nil {
						return err
					}
					health := message.(HealthRequest)
					if err := WriteMessage(conn, HealthResponse{SourceID: source, Nonce: health.Nonce, Status: HealthHealthy, Ops: &HealthOps{SyncPolicy: 1}}); err != nil {
						return err
					}
					message, err = ReadMessage(conn)
					if err != nil {
						return err
					}
					batch := message.(CommitBatchRequest)
					// Acceptance alone must never count as a durable acknowledgement.
					if err := WriteMessage(conn, Ack{Kind: AckCommitBatch, SourceID: source, Sequence: batch.Sequence, CommitID: batch.CommitID, AcceptedThroughSequence: &batch.Sequence, Points: uint32(len(batch.Points))}); err != nil {
						return err
					}
					if message, err = ReadMessage(conn); err != nil {
						return err
					} else if _, ok := message.(FlushRequest); !ok {
						return fmt.Errorf("expected flush, got %T", message)
					}
				}
				close(blocked)
				if _, err := ReadMessage(conn); err == nil {
					return fmt.Errorf("expected cancellation to close the connection")
				}
				return nil
			})
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			b := &Beta{feed: st.ObserveLiveHistory(), status: BetaStatus{Enabled: true, State: "waiting"}, cancel: cancel, done: make(chan struct{})}
			go func() {
				defer close(b.done)
				b.run(ctx, ClientConfig{SocketPath: listener.Addr().String(), SourceID: source, NodeID: "test", ClientVersion: "test", IOTimeout: 10 * time.Second}, "test-site", time.Millisecond)
			}()
			t.Cleanup(b.Close)
			if err := st.RecordTick(state.HistoryPoint{TsMs: 1}, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-blocked:
			case <-time.After(3 * time.Second):
				t.Fatal("sidecar did not reach the blocked operation")
			}
			for i := 2; i <= 257; i++ {
				if err := st.RecordTick(state.HistoryPoint{TsMs: int64(i)}, nil); err != nil {
					t.Fatal(err)
				}
			}
			closed := make(chan struct{})
			go func() {
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(b.Close)
				}
				wg.Wait()
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown exceeded its total budget")
			}
			waitServer(t, server)
			s := b.Status()
			if s.Acknowledged != 0 || s.DurableThrough != 0 || s.Pending != 1 || s.Queued != 256 || s.Dropped != 0 || s.LastError == "" || s.State != "degraded" {
				t.Fatalf("shutdown hid unconfirmed work: %+v", s)
			}
			if err := st.RecordTick(state.HistoryPoint{TsMs: 258}, nil); err != nil {
				t.Fatal(err)
			}
			rows, err := st.LoadHistory(0, 500, 0)
			if err != nil || len(rows) != 258 || b.Status().Offered != 257 {
				t.Fatalf("stopped session changed SQLite or accepted more work: rows=%d err=%v status=%+v", len(rows), err, b.Status())
			}
		})
	}
}
