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

func runTestBeta(t *testing.T, st *state.Store, socket string) *Beta {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b := &Beta{feed: st.ObserveLiveHistory(), status: BetaStatus{Enabled: true, State: "waiting"}, cancel: cancel, done: make(chan struct{})}
	id := mustID(t, "00112233445566778899aabbccddeeff")
	go func() {
		defer close(b.done)
		// Keep Start's I/O budget for real Rust fsync and CI scheduling delays.
		// Only the polling interval is shortened; failures must still surface.
		b.run(ctx, ClientConfig{SocketPath: socket, SourceID: id, NodeID: "test", ClientVersion: "test", IOTimeout: 2 * time.Second}, "test-site", 20*time.Millisecond)
	}()
	t.Cleanup(b.Close)
	return b
}

func waitBeta(t *testing.T, b *Beta, ready func(BetaStatus) bool) BetaStatus {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		status := b.Status()
		if ready(status) {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("shadow did not reach expected state: %+v", b.Status())
	return BetaStatus{}
}

func TestBetaRefusesUnsafeSidecarWithoutBlockingSQLite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ops    *HealthOps
		status HealthStatus
	}{
		{"unknown-ops", nil, HealthHealthy},
		{"not-durable", &HealthOps{SyncPolicy: 2}, HealthHealthy},
		{"store-limit", &HealthOps{SyncPolicy: 1, DatabaseBytes: betaMaxStoreBytes}, HealthHealthy},
		{"poisoned", &HealthOps{SyncPolicy: 1}, HealthUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := listenUnix(t)
			source := mustID(t, "00112233445566778899aabbccddeeff")
			server := runServer(listener, func(conn net.Conn) error {
				if err := serverHello(conn, source); err != nil {
					return err
				}
				message, err := ReadMessage(conn)
				if err != nil {
					return err
				}
				health := message.(HealthRequest)
				if err := WriteMessage(conn, HealthResponse{SourceID: source, Nonce: health.Nonce, Status: tc.status, Ops: tc.ops}); err != nil {
					return err
				}
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				if message, err := ReadMessage(conn); err == nil {
					return fmt.Errorf("unsafe sidecar received %T", message)
				}
				return nil
			})
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			b := runTestBeta(t, st, listener.Addr().String())
			if err := st.RecordTick(state.HistoryPoint{TsMs: 1}, nil); err != nil {
				t.Fatal(err)
			}
			waitBeta(t, b, func(s BetaStatus) bool { return s.Errors > 0 })
			for i := 2; i <= 400; i++ {
				if err := st.RecordTick(state.HistoryPoint{TsMs: int64(i)}, nil); err != nil {
					t.Fatal(err)
				}
			}
			s := b.Status()
			rows, err := st.LoadHistory(0, 500, 0)
			if err != nil || len(rows) != 400 || s.Dropped == 0 || s.Acknowledged != 0 {
				t.Fatalf("shadow changed source or hid loss: rows=%d status=%+v err=%v", len(rows), s, err)
			}
			waitServer(t, server)
		})
	}
}

func TestBetaMissingSidecarAndShutdown(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := runTestBeta(t, st, "/tmp/ftwdb-does-not-exist-beta.sock")
	if err := st.RecordTick(state.HistoryPoint{TsMs: 1}, nil); err != nil {
		t.Fatal(err)
	}
	waitBeta(t, b, func(s BetaStatus) bool { return s.Errors > 0 })
	started := time.Now()
	b.Close()
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("missing sidecar delayed shutdown by %v", elapsed)
	}
	if s := b.Status(); s.Acknowledged != 0 || s.Pending != 1 || s.Queued != 0 || s.LastError == "" {
		t.Fatalf("missing sidecar hid unconfirmed work: %+v", s)
	}
}

// The proxy loses one acknowledgement after Rust has made it durable. It keeps
// the exact Go commit frames so Rust can reconcile the copied points offline.
func shadowProxy(t *testing.T, target string, beforeLostACK ...func()) (string, func() [][]byte) {
	t.Helper()
	listener := listenUnix(t)
	var mu sync.Mutex
	var frames [][]byte
	go func() {
		for {
			front, err := listener.Accept()
			if err != nil {
				return
			}
			back, err := net.Dial("unix", target)
			if err != nil {
				_ = front.Close()
				continue
			}
			func() {
				defer front.Close()
				defer back.Close()
				for {
					_ = front.SetDeadline(time.Now().Add(3 * time.Second))
					_ = back.SetDeadline(time.Now().Add(3 * time.Second))
					frame, message, err := readRawFrame(front)
					if err != nil {
						return
					}
					commit := false
					if _, ok := message.(CommitBatchRequest); ok {
						commit = true
						mu.Lock()
						frames = append(frames, frame)
						mu.Unlock()
					}
					if _, err := back.Write(frame); err != nil {
						return
					}
					reply, err := ReadMessage(back)
					if err != nil {
						return
					}
					mu.Lock()
					lose := commit && len(frames) == 1
					atLimit := len(frames) == 1
					mu.Unlock()
					if lose {
						for _, hook := range beforeLostACK {
							hook()
						}
						return
					}
					if health, ok := reply.(HealthResponse); ok && atLimit {
						// New writes stop at the cap, but the existing durable receipt
						// must still be retrievable after its first ACK went missing.
						health.Status = HealthDegraded
						health.Ops.DatabaseBytes = betaMaxStoreBytes
						reply = health
					}
					if err := WriteMessage(front, reply); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String(), func() [][]byte { mu.Lock(); defer mu.Unlock(); return append([][]byte(nil), frames...) }
}

func startRustShadow(t *testing.T, binary, store, socket string) func(os.Signal) {
	t.Helper()
	command := exec.Command(binary, store, socket)
	log, err := os.CreateTemp(t.TempDir(), "sidecar.log")
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var once sync.Once
	stop := func(signal os.Signal) {
		once.Do(func() {
			_ = command.Process.Signal(signal)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = command.Process.Kill()
				t.Error("sidecar did not stop")
			}
			_ = log.Close()
		})
	}
	t.Cleanup(func() { stop(syscall.SIGTERM) })
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if _, err := os.Stat(socket); err == nil {
			return stop
		}
		select {
		case err := <-done:
			t.Fatalf("sidecar exited: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sidecar did not create socket")
	return stop
}

func TestRustSidecarInteropIdleBetweenLiveBatches(t *testing.T) {
	binary := os.Getenv("FTWDB_SHADOW_BIN")
	if binary == "" {
		t.Skip("set FTWDB_SHADOW_BIN for the pinned Rust gate")
	}
	root, err := os.MkdirTemp("/tmp", "ftw-beta-idle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "run", "shadow.sock")
	startRustShadow(t, binary, filepath.Join(root, "shadow"), socket)
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := runTestBeta(t, st, socket)
	if err := st.RecordTick(state.HistoryPoint{TsMs: 1000, GridW: 42}, nil); err != nil {
		t.Fatal(err)
	}
	first := waitBeta(t, b, func(s BetaStatus) bool { return s.Acknowledged == 1 })
	if first.Errors != 0 {
		t.Fatalf("first batch failed: %+v", first)
	}
	// Rust closes idle connections after two seconds. Production batches arrive
	// every thirty seconds; leave the real server idle beyond its deadline.
	time.Sleep(3 * time.Second)
	if err := st.RecordTick(state.HistoryPoint{TsMs: 2000, GridW: 43}, nil); err != nil {
		t.Fatal(err)
	}
	status := waitBeta(t, b, func(s BetaStatus) bool { return s.Acknowledged == 2 })
	if status.Errors != 0 || status.State != "ok" || status.DurableThrough != 2 || status.Pending != 0 || status.Dropped != 0 {
		t.Fatalf("idle time caused a failed batch: %+v", status)
	}
	b.Close()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	client, _, err := Connect(context.Background(), ClientConfig{SocketPath: socket, SourceID: source, NodeID: "check", ClientVersion: "test", IOTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	health, err := client.Health(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if health.Ops == nil || health.Ops.DatabasePoints != 10 || health.Ops.ProtocolErrorCount != 0 {
		t.Fatalf("unexpected sidecar health after idle batches: %+v", health.Ops)
	}
}

func TestRustSidecarInteropLiveHistory(t *testing.T) {
	binary := os.Getenv("FTWDB_SHADOW_BIN")
	reconcile := os.Getenv("FTWDB_RECONCILE_BIN")
	if binary == "" || reconcile == "" {
		t.Skip("set FTWDB_SHADOW_BIN and FTWDB_RECONCILE_BIN for the pinned Rust gate")
	}
	root, err := os.MkdirTemp("/tmp", "ftw-beta-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	store := filepath.Join(root, "shadow")
	socket := filepath.Join(root, "run", "shadow.sock")
	stop := startRustShadow(t, binary, store, socket)
	proxy, frames := shadowProxy(t, socket)
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := runTestBeta(t, st, proxy)
	for i, ts := range []int64{3000, 1000, 1000} {
		if err := st.RecordTick(state.HistoryPoint{TsMs: ts, GridW: float64(42 + i), PVW: -1000, BatW: 200, LoadW: 842, BatSoC: 0.75}, nil); err != nil {
			t.Fatal(err)
		}
	}
	status := waitBeta(t, b, func(s BetaStatus) bool { return s.Acknowledged == 3 })
	if status.Errors == 0 || status.DurableThrough != 3 {
		t.Fatalf("lost acknowledgement was not recovered: %+v", status)
	}
	b.Close()
	wire := frames()
	if len(wire) != 2 || !bytes.Equal(wire[0], wire[1]) {
		t.Fatalf("retry changed exact frame: %d", len(wire))
	}
	// SIGKILL after the durable acknowledgement tests the persisted receipt,
	// independently of a clean process shutdown.
	stop(syscall.SIGKILL)
	_ = os.Remove(socket) // Killed processes cannot unlink their socket.
	stop = startRustShadow(t, binary, store, socket)
	source := mustID(t, "00112233445566778899aabbccddeeff")
	client, _, err := Connect(context.Background(), ClientConfig{SocketPath: socket, SourceID: source, NodeID: "reopen", ClientVersion: "test", IOTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PreparedCommitFromFrame(wire[0])
	if err != nil {
		t.Fatal(err)
	}
	ack, err := client.CommitDurable(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Commit.Deduplicated || ack.DurableThrough != 3 {
		t.Fatal("restart lost durable receipt")
	}
	health, err := client.Health(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if health.Ops == nil || health.Ops.DatabasePoints != 15 || health.Ops.DatabaseCommits != 1 {
		t.Fatalf("copy has gaps or duplicates: %+v", health)
	}
	_ = client.Close()
	stop(syscall.SIGTERM)
	framePath := filepath.Join(root, "expected.hex")
	if err := os.WriteFile(framePath, []byte(hex.EncodeToString(wire[0])+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(reconcile, store, framePath).CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"content_matches":true`) {
		t.Fatalf("reconcile failed: %v\n%s", err, output)
	}
}
