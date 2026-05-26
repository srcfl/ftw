// Package wormhole — Magic-wormhole TCP forwarding via the fowld subprocess
//
// # Magic-wormhole TCP forwarding via the fowld subprocess
//
// Wraps `fowld` (the daemon variant of the `fowl` tool) as a subprocess.
// fowld communicates exclusively over stdin/stdout using newline-delimited JSON.
// Each line in is a command; each line out is an event.
//
// Why subprocess: the Go magic-wormhole port (wormhole-william v1.0.8) implements
// only file/text/directory transfer; it has no Dilation extension required for
// bidirectional TCP streaming. The Python `fowl` tool does. Subprocess-wrapping
// keeps us on the canonical magic-wormhole stack without porting Dilation to Go.
//
// Install fowl: uv tool install fowl  (sourceful's preferred path)
// See docs/ftw-pair.md for the full setup guide.
//
// # Protocol overview (fowld JSON)
//
// Commands sent to fowld via stdin (one JSON object per line):
//
//	{"kind": "allocate-code"}                                    → host: allocate a fresh PAKE code
//	{"kind": "set-code", "code": "<code>"}                      → client: join an existing session
//	{"kind": "danger-disable-permission-check"}                  → allow any forward target
//	{"kind": "local", "listen": "tcp:P:interface=localhost",
//	                  "connect": "tcp:localhost:Q"}              → listen on local :P, forward to remote :Q
//
// Events emitted by fowld on stdout (one JSON object per line):
//
//	{"kind": "welcome", ...}                                     → connected to rendezvous server
//	{"kind": "code-allocated", "code": "<code>"}                 → PAKE code ready to share
//	{"kind": "peer-connected", ...}                              → remote peer joined the session
//	{"kind": "listening", "listen": "tcp:P:interface=localhost", → local port P is ready
//	                       "connect": ..., "listener_id": ...}
//	{"kind": "error", "message": "..."}                          → unrecoverable error
//
// # Error reporting notes
//
// fowld (the Twisted/Python daemon) can emit error information in two ways:
//
//  1. As a JSON event: {"kind":"error","message":"<reason>"}
//     Captured in fowldEvent.Message.
//
//  2. As a raw non-JSON stdout line, e.g.:
//     b'{"kind":"bad-command"}': failed: 'bad-command'
//     This is emitted by the LocalCommandDispatch.lineReceived handler when
//     parse_fowld_command raises. The line is not JSON — our scanner catches
//     it as a "non-JSON diagnostic" and includes it verbatim in the error.
//
//  3. Via stderr: Python deprecation warnings, policy log lines
//     ("DANGER. All connect / listen endpoints allowed."), and tracebacks.
//     Captured in a ring buffer and appended to any error message.
//
// The combination of (2) and (3) is why a bare "fowld error: " with no body
// was previously seen: we decoded ev.Message from a JSON event that had an
// empty or missing "message" field, discarded the non-JSON diagnostics, and
// discarded stderr entirely.
//
// # Shared code format
//
// The code shared between host and client is:
//
//	"<fowl-wormhole-code>:<mcp-port>"
//
// e.g. "7-spinach-atlas:9876". The fowl wormhole code alone is the standard
// magic-wormhole PAKE handshake code. The appended ":<port>" tells the client
// which TCP port to forward to on the host side.
//
// # Data-flow once connected
//
//	Client machine                    Host machine (Pi)
//	──────────────────────────        ────────────────────────────
//	  Claude / MCP client             ftw-pair sidecar
//	      │                               │
//	      ▼                               ▼
//	  localhost:NNNN  ←── fowl Dilation ──→  localhost:MCP_PORT
//	  (pre-allocated                        (the real MCP server)
//	   by ftw-connect)
//
// NNNN is chosen before Connect starts; it is passed by the client to its own
// fowld via the `local` command after the wormhole handshake completes.
package wormhole

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
)

// fowldBinary is the name (or absolute path) of the fowld executable.
// If it is not on PATH the functions return ErrFowlNotFound.
const fowldBinary = "fowld"

// stderrRingSize is the number of recent stderr lines retained per subprocess.
// These are appended to any error message to provide context when fowld fails.
const stderrRingSize = 50

// ErrFowlNotFound is returned when fowld is not on PATH.
type ErrFowlNotFound struct {
	// Underlying is the exec.LookPath error, for diagnostics.
	Underlying error
}

func (e *ErrFowlNotFound) Error() string {
	return "fowld not found on PATH — install with `uv tool install fowl`"
}

func (e *ErrFowlNotFound) Unwrap() error { return e.Underlying }

// ── fowld JSON event types ────────────────────────────────────────────────────

// fowldEvent represents a single JSON event emitted by fowld on stdout.
//
// Normal structured fields (Message, Summary, Detail) cover the documented
// {"kind":"error","message":"..."} shape.  The raw field captures the original
// line so callers always see what fowld actually sent, even when the message
// field is empty or when fowld uses an undocumented key name.
type fowldEvent struct {
	Kind       string `json:"kind"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	Summary    string `json:"summary,omitempty"` // alternate error field used by some fowld versions
	Detail     string `json:"detail,omitempty"`  // alternate error field used by some fowld versions
	ListenEP   string `json:"listen,omitempty"`
	ConnectEP  string `json:"connect,omitempty"`
	ListenerID string `json:"listener_id,omitempty"`

	// raw is the original JSON line, set before the struct is used.
	// Not a JSON field — populated by the reader loop.
	raw string
}

// errorText returns the best human-readable error description from the event.
// It concatenates non-empty structured fields and falls back to the raw line
// when all named fields are empty.
func (ev *fowldEvent) errorText() string {
	parts := make([]string, 0, 3)
	for _, s := range []string{ev.Message, ev.Summary, ev.Detail} {
		if t := strings.TrimSpace(s); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	// Fall back to the verbatim JSON — at minimum the caller sees the raw event.
	return "(no message field — raw event: " + ev.raw + ")"
}

// ── stderr ring buffer ────────────────────────────────────────────────────────

// stderrRing is a fixed-capacity ring buffer for recent stderr lines.
type stderrRing struct {
	mu   sync.Mutex
	buf  []string
	size int
}

func newStderrRing(n int) *stderrRing { return &stderrRing{buf: make([]string, 0, n), size: n} }

func (r *stderrRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) >= r.size {
		copy(r.buf, r.buf[1:])
		r.buf = r.buf[:r.size-1]
	}
	r.buf = append(r.buf, line)
}

// tail returns the buffered lines joined by newlines.
// Returns an empty string when the buffer is empty.
func (r *stderrRing) tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return ""
	}
	return strings.Join(r.buf, "\n")
}

// ── Host ──────────────────────────────────────────────────────────────────────

// Host is the host-side tunnel handle.  The host runs on the Pi; it
// advertises a magic-wormhole code that the remote peer uses to connect.
// Call Close when done.
type Host struct {
	// Code is the human-shareable code to hand to the remote peer.
	// Format: "<fowl-wormhole-code>:<mcp-port>"
	// e.g. "7-spinach-atlas:9876"
	Code string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Close terminates the fowld subprocess and waits for it to exit.
func (h *Host) Close() {
	h.cancel()
	if h.stdin != nil {
		h.stdin.Close() //nolint:errcheck
	}
	h.wg.Wait()
}

// StartHost starts a fowld subprocess that allocates a fresh wormhole
// code and waits for a peer to connect.  remoteAddr is the TCP address of the
// local MCP server (e.g. "127.0.0.1:9876") that the peer will be forwarded to.
//
// The function blocks until the wormhole code has been allocated (i.e. the
// first PAKE handshake message has been sent to the rendezvous server) and then
// returns; the caller can immediately read host.Code and share it with the peer.
//
// A background goroutine keeps the fowld process alive and drains its output.
// Call host.Close() to tear everything down.
func StartHost(ctx context.Context, remoteAddr string) (*Host, error) {
	// Resolve the MCP port from remoteAddr so we can embed it in the code.
	_, portStr, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return nil, fmt.Errorf("wormhole host: invalid remoteAddr %q: %w", remoteAddr, err)
	}

	// Verify fowld is available before starting anything.
	if _, err := exec.LookPath(fowldBinary); err != nil {
		return nil, &ErrFowlNotFound{Underlying: err}
	}

	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, fowldBinary)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole host: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole host: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole host: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole host: start fowld: %w", err)
	}

	// Drain stderr into a ring buffer for diagnostics.
	stderrBuf := newStderrRing(stderrRingSize)
	go drainStderr(stderrPipe, stderrBuf)

	h := &Host{
		cmd:    cmd,
		stdin:  stdinPipe,
		cancel: cancel,
	}

	// Disable the connect-policy check so the peer can reach us freely.
	if err := h.writeJSON(map[string]any{"kind": "danger-disable-permission-check"}); err != nil {
		cancel()
		stdinPipe.Close()
		return nil, fmt.Errorf("wormhole host: write danger-disable: %w", err)
	}

	// Ask fowld to allocate a new wormhole code.
	if err := h.writeJSON(map[string]any{"kind": "allocate-code", "length": 2}); err != nil {
		cancel()
		stdinPipe.Close()
		return nil, fmt.Errorf("wormhole host: write allocate-code: %w", err)
	}

	// Scan stdout for the code-allocated event.  We also watch for error events.
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	scanner := bufio.NewScanner(stdoutPipe)

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer cmd.Wait() //nolint:errcheck
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var ev fowldEvent
			if jsonErr := json.Unmarshal([]byte(line), &ev); jsonErr != nil {
				// Non-JSON line from fowld stdout — this is how fowld reports
				// command-parse failures (e.g. "b'...': failed: 'bad-command'").
				// Treat any non-JSON stdout line as a fatal error so it surfaces
				// to the caller rather than being silently discarded.
				select {
				case errCh <- buildFowldError("non-JSON stdout", line, stderrBuf):
				default:
				}
				continue
			}
			ev.raw = line
			switch ev.Kind {
			case "code-allocated":
				select {
				case codeCh <- ev.Code:
				default:
				}
			case "error":
				select {
				case errCh <- buildFowldError(ev.errorText(), "", stderrBuf):
				default:
				}
			}
			// All other events (welcome, peer-connected, incoming-connection,
			// bytes-in/out, etc.) are silently consumed; we keep the subprocess
			// alive by continuously draining stdout.
		}
	}()

	// Wait for code-allocated (or an error / context cancellation).
	select {
	case fowlCode := <-codeCh:
		// Embed the MCP port in the shared code.
		h.Code = fowlCode + ":" + portStr
		return h, nil
	case ferr := <-errCh:
		h.Close()
		return nil, fmt.Errorf("wormhole host: %w", ferr)
	case <-cctx.Done():
		h.Close()
		return nil, fmt.Errorf("wormhole host: context cancelled before code allocated")
	}
}

func (h *Host) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = h.stdin.Write(b)
	return err
}

// ── Client ────────────────────────────────────────────────────────────────────

// Client is the client-side tunnel handle.  Call Close when done.
type Client struct {
	// LocalAddr is the 127.0.0.1:NNNN address the MCP client should dial;
	// bytes are forwarded through the wormhole to the host's MCP server.
	LocalAddr string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Close terminates the fowld subprocess and waits for it to exit.
func (w *Client) Close() {
	w.cancel()
	if w.stdin != nil {
		w.stdin.Close() //nolint:errcheck
	}
	w.wg.Wait()
}

// Connect joins an existing wormhole session identified by code
// and sets up a local TCP listener that forwards to the host's MCP server.
//
// code must be in the format produced by StartHost:
//
//	"<fowl-wormhole-code>:<mcp-port>"
//	e.g. "7-spinach-atlas:9876"
//
// The function blocks until the local forwarding listener is ready and then
// returns a Client whose LocalAddr can be dialled by the MCP client.
// Call client.Close() to tear everything down.
func Connect(ctx context.Context, code string) (*Client, error) {
	// Split the composite code into the fowl wormhole code and the host MCP port.
	fowlCode, mcpPort, err := splitCompositeCode(code)
	if err != nil {
		return nil, fmt.Errorf("wormhole client: %w", err)
	}

	// Pre-allocate a free local port.  We open a listener to get the OS to
	// assign a port, note the port, then close the listener.  There is a small
	// TOCTOU race but it is acceptable in practice.
	localPort, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("wormhole client: pick free port: %w", err)
	}

	// Verify fowld is available.
	if _, err := exec.LookPath(fowldBinary); err != nil {
		return nil, &ErrFowlNotFound{Underlying: err}
	}

	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, fowldBinary)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole client: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole client: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole client: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole client: start fowld: %w", err)
	}

	// Drain stderr into a ring buffer for diagnostics.
	stderrBuf := newStderrRing(stderrRingSize)
	go drainStderr(stderrPipe, stderrBuf)

	w := &Client{
		LocalAddr: fmt.Sprintf("127.0.0.1:%d", localPort),
		cmd:       cmd,
		stdin:     stdinPipe,
		cancel:    cancel,
	}

	// Disable the listen-policy check so we can open listeners freely.
	if err := w.writeJSON(map[string]any{"kind": "danger-disable-permission-check"}); err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole client: write danger-disable: %w", err)
	}

	// Join the existing wormhole session.
	if err := w.writeJSON(map[string]any{"kind": "set-code", "code": fowlCode}); err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole client: write set-code: %w", err)
	}

	// We need to:
	//   1. Wait for "peer-connected" (Dilation handshake complete).
	//   2. Send the "local" command to open the forwarding listener.
	//   3. Wait for "listening" to confirm the listener is ready.
	//
	// All of this is driven by scanning fowld's stdout.
	listenEP := fmt.Sprintf("tcp:%d:interface=localhost", localPort)
	connectEP := fmt.Sprintf("tcp:localhost:%s", mcpPort)

	readyCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	scanner := bufio.NewScanner(stdoutPipe)

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer cmd.Wait() //nolint:errcheck

		peerConnected := false
		localSent := false
		ready := false

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var ev fowldEvent
			if jsonErr := json.Unmarshal([]byte(line), &ev); jsonErr != nil {
				// Non-JSON diagnostic line (e.g. command-parse failure from fowld).
				select {
				case errCh <- buildFowldError("non-JSON stdout", line, stderrBuf):
				default:
				}
				continue
			}
			ev.raw = line
			switch ev.Kind {
			case "peer-connected":
				if !peerConnected {
					peerConnected = true
					// Now that Dilation is up, request the local forwarding listener.
					if !localSent {
						localSent = true
						if sendErr := w.writeJSON(map[string]any{
							"kind":    "local",
							"listen":  listenEP,
							"connect": connectEP,
						}); sendErr != nil {
							select {
							case errCh <- fmt.Errorf("send local command: %w", sendErr):
							default:
							}
						}
					}
				}
			case "listening":
				// fowld emits this when the local listener is bound and ready.
				// Verify it's our listener (by matching the listen endpoint).
				if !ready && ev.ListenEP == listenEP {
					ready = true
					select {
					case readyCh <- struct{}{}:
					default:
					}
				}
			case "error":
				select {
				case errCh <- buildFowldError(ev.errorText(), "", stderrBuf):
				default:
				}
			}
			// All other events are drained to keep the subprocess alive.
		}
	}()

	select {
	case <-readyCh:
		return w, nil
	case ferr := <-errCh:
		w.Close()
		return nil, fmt.Errorf("wormhole client: %w", ferr)
	case <-cctx.Done():
		w.Close()
		return nil, fmt.Errorf("wormhole client: context cancelled before tunnel ready")
	}
}

func (w *Client) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.stdin.Write(b)
	return err
}

// ── helpers ───────────────────────────────────────────────────────────────────

// buildFowldError constructs a diagnostic error that includes both the primary
// message and any stderr lines captured in ring.  If there are no stderr lines
// the error is just "fowld error: <msg>".
func buildFowldError(msg, nonJSONLine string, ring *stderrRing) error {
	var sb strings.Builder
	sb.WriteString("fowld error: ")
	sb.WriteString(msg)
	if nonJSONLine != "" {
		sb.WriteString(" (raw: ")
		sb.WriteString(nonJSONLine)
		sb.WriteString(")")
	}
	if tail := ring.tail(); tail != "" {
		sb.WriteString("\nfowld stderr:\n")
		sb.WriteString(tail)
	}
	return fmt.Errorf("%s", sb.String()) //nolint:err113
}

// drainStderr reads all lines from r into ring until EOF.
func drainStderr(r io.Reader, ring *stderrRing) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		ring.add(sc.Text())
	}
}

// splitCompositeCode splits a code of the form "<fowl-code>:<port>" into its
// two parts.  The port is everything after the LAST colon, since the fowl code
// itself contains hyphens but never colons.
func splitCompositeCode(code string) (fowlCode, port string, err error) {
	idx := strings.LastIndex(code, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid code %q: expected \"<wormhole-code>:<port>\"", code)
	}
	fowlCode = code[:idx]
	port = code[idx+1:]
	if fowlCode == "" || port == "" {
		return "", "", fmt.Errorf("invalid code %q: both wormhole-code and port must be non-empty", code)
	}
	return fowlCode, port, nil
}

// pickFreePort asks the OS for a free TCP port on localhost and returns it.
// The port is not held open; callers should use it immediately.
func pickFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}
