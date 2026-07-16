package mpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const OptimizerProtocolVersion = 1

// OptimizerRuntimeInfo is returned by the sidecar handshake and exposed to
// component diagnostics. Protocol compatibility, not matching app versions,
// decides whether core may use a separately released optimizer.
type OptimizerRuntimeInfo struct {
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	ProtocolVersion int      `json:"protocol_version"`
	Features        []string `json:"features"`
	BuildSHA        string   `json:"build_sha,omitempty"`
	Transport       string   `json:"transport"`
}

// OptimizerTransport owns request framing and lifecycle only. The request
// schema and independent Go-side candidate validation remain in
// ExternalOptimizer.
type OptimizerTransport interface {
	RoundTrip(context.Context, []byte) ([]byte, error)
	Health(context.Context) (OptimizerRuntimeInfo, error)
	Close() error
}

type ProcessTransportConfig struct {
	Command     []string
	ModuleDir   string
	IdleTimeout time.Duration
}

// ProcessTransport preserves the all-in-one/native fallback. One warm worker
// is shared and calls are serialized because CVXPY warm-start state is local
// to the process.
type ProcessTransport struct {
	cfg ProcessTransportConfig

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	waitCh    chan error
	idleTimer *time.Timer
}

func NewProcessTransport(cfg ProcessTransportConfig) (*ProcessTransport, error) {
	if len(cfg.Command) == 0 || strings.TrimSpace(cfg.Command[0]) == "" {
		return nil, errors.New("optimizer command is empty")
	}
	return &ProcessTransport{cfg: cfg}, nil
}

func (t *ProcessTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancelIdleStopLocked()
	if err := t.ensureStartedLocked(); err != nil {
		return nil, err
	}
	if _, err := t.stdin.Write(append(append([]byte(nil), payload...), '\n')); err != nil {
		t.stopLocked()
		return nil, fmt.Errorf("write optimizer request: %w", err)
	}
	line, err := scanLine(ctx, t.scanner)
	if err != nil {
		t.stopLocked()
		return nil, err
	}
	t.scheduleIdleStopLocked()
	return line, nil
}

func (t *ProcessTransport) Health(context.Context) (OptimizerRuntimeInfo, error) {
	return OptimizerRuntimeInfo{
		Name: "ftw-optimizer", Version: "bundled",
		ProtocolVersion: OptimizerProtocolVersion,
		Features:        []string{"champion", "recourse", "multistage"},
		Transport:       "process",
	}, nil
}

func (t *ProcessTransport) ensureStartedLocked() error {
	if t.cmd != nil {
		return nil
	}
	cmd := exec.Command(t.cfg.Command[0], t.cfg.Command[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if t.cfg.ModuleDir != "" {
		cmd.Env = append(cmd.Env, "PYTHONPATH="+t.cfg.ModuleDir)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("optimizer stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("optimizer stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start optimizer %q: %w", t.cfg.Command[0], err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.cmd, t.stdin, t.scanner, t.waitCh = cmd, stdin, bufio.NewScanner(stdout), waitCh
	t.scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return nil
}

func (t *ProcessTransport) cancelIdleStopLocked() {
	if t.idleTimer != nil {
		t.idleTimer.Stop()
		t.idleTimer = nil
	}
}

func (t *ProcessTransport) scheduleIdleStopLocked() {
	if t.cfg.IdleTimeout <= 0 || t.cmd == nil {
		return
	}
	t.cancelIdleStopLocked()
	var timer *time.Timer
	timer = time.AfterFunc(t.cfg.IdleTimeout, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.idleTimer != timer {
			return
		}
		t.idleTimer = nil
		t.stopLocked()
	})
	t.idleTimer = timer
}

func (t *ProcessTransport) stopLocked() {
	t.cancelIdleStopLocked()
	if t.cmd == nil {
		return
	}
	_ = t.stdin.Close()
	_ = t.cmd.Process.Kill()
	<-t.waitCh
	t.cmd, t.stdin, t.scanner, t.waitCh = nil, nil, nil, nil
}

func (t *ProcessTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancelIdleStopLocked()
	if t.cmd == nil {
		return nil
	}
	_ = t.stdin.Close()
	select {
	case err := <-t.waitCh:
		t.cmd, t.stdin, t.scanner, t.waitCh = nil, nil, nil, nil
		return err
	case <-time.After(time.Second):
		t.stopLocked()
		return nil
	}
}

type UnixTransport struct{ socketPath string }

func NewUnixTransport(socketPath string) *UnixTransport {
	return &UnixTransport{socketPath: socketPath}
}

func (t *UnixTransport) exchange(ctx context.Context, payload []byte) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", t.socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", t.socketPath, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(append(append([]byte(nil), payload...), '\n')); err != nil {
		return nil, fmt.Errorf("write unix optimizer: %w", err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return scanLine(ctx, scanner)
}

func (t *UnixTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	return t.exchange(ctx, payload)
}

func (t *UnixTransport) Health(ctx context.Context) (OptimizerRuntimeInfo, error) {
	payload, _ := json.Marshal(map[string]any{"type": "handshake", "protocol_version": OptimizerProtocolVersion})
	line, err := t.exchange(ctx, payload)
	if err != nil {
		return OptimizerRuntimeInfo{}, err
	}
	var info OptimizerRuntimeInfo
	if err := json.Unmarshal(line, &info); err != nil {
		return OptimizerRuntimeInfo{}, fmt.Errorf("decode optimizer handshake: %w", err)
	}
	if info.ProtocolVersion != OptimizerProtocolVersion {
		return OptimizerRuntimeInfo{}, fmt.Errorf("optimizer protocol version %d, want %d", info.ProtocolVersion, OptimizerProtocolVersion)
	}
	info.Transport = "unix"
	return info, nil
}

func (t *UnixTransport) Close() error { return nil }

// AutoTransport prefers an independently updated sidecar, but treats it as an
// optional capability. Socket absence, failed handshake, protocol mismatch, or
// a broken request all retry once through the bundled process transport.
type AutoTransport struct {
	primary  OptimizerTransport
	fallback OptimizerTransport
}

func NewAutoTransport(primary, fallback OptimizerTransport) *AutoTransport {
	return &AutoTransport{primary: primary, fallback: fallback}
}

func (t *AutoTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	if info, err := t.primary.Health(ctx); err == nil && optimizerHasFeature(info, requiredOptimizerFeature(payload)) {
		if response, err := t.primary.RoundTrip(ctx, payload); err == nil {
			return response, nil
		}
	}
	return t.fallback.RoundTrip(ctx, payload)
}

func requiredOptimizerFeature(payload []byte) string {
	var request struct {
		Settings struct {
			ScenarioPolicy string `json:"scenario_policy"`
		} `json:"settings"`
	}
	if json.Unmarshal(payload, &request) == nil {
		switch request.Settings.ScenarioPolicy {
		case "recourse", "multistage":
			return request.Settings.ScenarioPolicy
		}
	}
	return "champion"
}

func optimizerHasFeature(info OptimizerRuntimeInfo, feature string) bool {
	for _, available := range info.Features {
		if available == feature {
			return true
		}
	}
	return false
}

func (t *AutoTransport) Health(ctx context.Context) (OptimizerRuntimeInfo, error) {
	if info, err := t.primary.Health(ctx); err == nil {
		return info, nil
	}
	return t.fallback.Health(ctx)
}

func (t *AutoTransport) Close() error {
	errPrimary := t.primary.Close()
	errFallback := t.fallback.Close()
	if errPrimary != nil {
		return errPrimary
	}
	return errFallback
}

func scanLine(ctx context.Context, scanner *bufio.Scanner) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		if !scanner.Scan() {
			err := scanner.Err()
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			ch <- result{err: err}
			return
		}
		ch <- result{line: append([]byte(nil), scanner.Bytes()...)}
	}()
	select {
	case got := <-ch:
		return got.line, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
