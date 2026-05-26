# `ftw-pair` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `ftw-pair` MCP sidecar + `ftw-connect` friend CLI that let an owner grant time-bound MCP access to a live forty-two-watts instance via a one-time magic-wormhole code.

**Architecture:** Host-side sidecar (`go/cmd/ftw-pair/`) exposes an MCP server on `localhost:9999`, port-forwarded to the friend's laptop through a magic-wormhole tunnel. Friend-side CLI (`go/cmd/ftw-connect/`) accepts the code, opens the tunnel, registers a Claude Code MCP entry. Sidecar talks to the running 42W service over `http://localhost:8080`; no privileged internal state access.

**Tech Stack:**
- Go 1.26 (module `github.com/frahlg/forty-two-watts/go`)
- MCP server: `github.com/modelcontextprotocol/go-sdk/mcp` (HTTP/SSE transport)
- Wormhole: `github.com/psanford/wormhole-william` (pure-Go, has `forward` sub-pkg for TCP port forwarding)
- Existing 42W deps: `modbus`, `paho.mqtt.golang`, `modernc.org/sqlite`, `gopher-lua`, `yaml.v3`
- New systemd helpers: `golang.org/x/sys/unix` (for `pcap_capture`'s `AF_PACKET` socket — Linux only)
- Web: vanilla Lit-style web components (matches existing `web/components/ftw-*.js`)

**Spec:** [`docs/superpowers/specs/2026-05-25-ftw-pair-mcp-remote-design.md`](../specs/2026-05-25-ftw-pair-mcp-remote-design.md)

---

## Task 1: Add dependencies + scaffold both binaries

**Files:**
- Modify: `go/go.mod`
- Create: `go/cmd/ftw-pair/main.go`
- Create: `go/cmd/ftw-pair/main_test.go`
- Create: `go/cmd/ftw-connect/main.go`
- Create: `go/cmd/ftw-connect/main_test.go`

- [ ] **Step 1: Add deps**

```bash
cd go
go get github.com/modelcontextprotocol/go-sdk/mcp@latest
go get github.com/psanford/wormhole-william@latest
go mod tidy
```

- [ ] **Step 2: Write failing test for ftw-pair scaffold**

`go/cmd/ftw-pair/main_test.go`:

```go
package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	out, err := exec.Command("go", "run", ".", "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ftw-pair") {
		t.Fatalf("expected ftw-pair in output, got: %s", out)
	}
}
```

- [ ] **Step 3: Run test (should fail — file doesn't exist)**

```bash
cd go/cmd/ftw-pair && go test .
```

Expected: FAIL — `main.go` doesn't exist yet.

- [ ] **Step 4: Implement minimal ftw-pair main.go**

`go/cmd/ftw-pair/main.go`:

```go
// ftw-pair is the host-side sidecar that exposes a forty-two-watts
// instance as an MCP server over a magic-wormhole tunnel.
//
// Spawned by `forty-two-watts pair`. Talks to the running main
// service via http://localhost:8080. Exposes MCP on :9999, forwarded
// through wormhole to the friend's laptop.
//
// Lifecycle: TTL-bound (default 4h). Hard kill at expiry. One active
// session per host.
package main

import (
	"flag"
	"fmt"
	"os"
)

var Version = "dev"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Printf("ftw-pair %s\n", Version)
		os.Exit(0)
	}
	// real entry point implemented in later tasks
	fmt.Fprintln(os.Stderr, "ftw-pair: not yet implemented")
	os.Exit(1)
}
```

- [ ] **Step 5: Run test (should pass)**

```bash
cd go/cmd/ftw-pair && go test .
```

Expected: PASS.

- [ ] **Step 6: Mirror for ftw-connect**

`go/cmd/ftw-connect/main_test.go`:

```go
package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	out, err := exec.Command("go", "run", ".", "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ftw-connect") {
		t.Fatalf("expected ftw-connect, got: %s", out)
	}
}
```

`go/cmd/ftw-connect/main.go`:

```go
// ftw-connect is the friend-side CLI that accepts a magic-wormhole
// code, opens the tunnel to a paired forty-two-watts instance, and
// registers the resulting MCP endpoint with Claude Code.
package main

import (
	"flag"
	"fmt"
	"os"
)

var Version = "dev"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Printf("ftw-connect %s\n", Version)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "ftw-connect: not yet implemented")
	os.Exit(1)
}
```

- [ ] **Step 7: Both compile and test**

```bash
cd go && go build ./cmd/ftw-pair ./cmd/ftw-connect && go test ./cmd/ftw-pair ./cmd/ftw-connect
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add go/go.mod go/go.sum go/cmd/ftw-pair/ go/cmd/ftw-connect/
git commit -m "feat(pair): scaffold ftw-pair sidecar + ftw-connect CLI binaries

Empty main packages with -version flag, plus the MCP + wormhole
library dependencies needed for subsequent tasks."
```

---

## Task 2: Session state + TTL timer

**Files:**
- Create: `go/cmd/ftw-pair/session.go`
- Create: `go/cmd/ftw-pair/session_test.go`

- [ ] **Step 1: Write failing test for session lifecycle**

`go/cmd/ftw-pair/session_test.go`:

```go
package main

import (
	"context"
	"testing"
	"time"
)

func TestSessionExpiresAtTTL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := NewSession(ctx, SessionConfig{
		TTL:    50 * time.Millisecond,
		Intent: "test driver",
	})
	if s.Remaining() <= 0 {
		t.Fatal("expected positive remaining at start")
	}
	select {
	case <-s.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("session did not expire")
	}
	if s.Remaining() > 0 {
		t.Fatalf("expected 0 remaining after expiry, got %s", s.Remaining())
	}
	if s.ExitReason() != "ttl_expired" {
		t.Fatalf("expected ttl_expired, got %q", s.ExitReason())
	}
}

func TestSessionEarlyAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := NewSession(ctx, SessionConfig{TTL: time.Hour, Intent: "x"})
	s.End("aborted_by_owner")

	select {
	case <-s.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("session did not end")
	}
	if s.ExitReason() != "aborted_by_owner" {
		t.Fatalf("expected aborted_by_owner, got %q", s.ExitReason())
	}
}
```

- [ ] **Step 2: Run test (FAIL — missing types)**

```bash
cd go/cmd/ftw-pair && go test -run TestSession
```

Expected: FAIL.

- [ ] **Step 3: Implement session**

`go/cmd/ftw-pair/session.go`:

```go
package main

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SessionConfig captures everything decided at pair-start.
type SessionConfig struct {
	TTL    time.Duration
	Intent string // free-form owner-supplied description threaded into the friend-side prompt
	As     string // optional friend identity ("@erikarenhill")
}

// Session is the lifecycle owner. It enforces the TTL and surfaces a
// Done() channel everything else listens on for shutdown. ExitReason
// is set exactly once and is visible after Done() closes.
type Session struct {
	ID        string
	StartedAt time.Time
	cfg       SessionConfig

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	reason string
	ended  bool
}

func NewSession(parent context.Context, cfg SessionConfig) *Session {
	if cfg.TTL <= 0 {
		cfg.TTL = 4 * time.Hour
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Session{
		ID:        uuid.NewString(),
		StartedAt: time.Now(),
		cfg:       cfg,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	go s.timerLoop(ctx)
	return s
}

func (s *Session) timerLoop(ctx context.Context) {
	timer := time.NewTimer(s.cfg.TTL)
	defer timer.Stop()
	select {
	case <-timer.C:
		s.End("ttl_expired")
	case <-ctx.Done():
		s.End("context_cancelled")
	}
}

func (s *Session) End(reason string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.reason = reason
	s.mu.Unlock()
	s.cancel()
	close(s.done)
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Remaining() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return 0
	}
	r := s.cfg.TTL - time.Since(s.StartedAt)
	if r < 0 {
		return 0
	}
	return r
}

func (s *Session) ExitReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func (s *Session) Intent() string { return s.cfg.Intent }
func (s *Session) As() string     { return s.cfg.As }
```

- [ ] **Step 4: Add uuid dep**

```bash
cd go && go get github.com/google/uuid && go mod tidy
```

- [ ] **Step 5: Run tests (PASS)**

```bash
cd go/cmd/ftw-pair && go test -run TestSession -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go/cmd/ftw-pair/session.go go/cmd/ftw-pair/session_test.go go/go.mod go/go.sum
git commit -m "feat(pair): session lifecycle with TTL + early-abort"
```

---

## Task 3: Audit log

**Files:**
- Create: `go/cmd/ftw-pair/audit.go`
- Create: `go/cmd/ftw-pair/audit_test.go`

- [ ] **Step 1: Failing test**

`go/cmd/ftw-pair/audit_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestAuditAppendAndRender(t *testing.T) {
	a := NewAudit()
	a.Append(AuditEvent{Tool: "ftw_api", Args: map[string]any{"path": "/api/status"}, OutcomeOK: true, OutcomeMsg: "200 OK, 432 bytes"})
	a.Append(AuditEvent{Tool: "deploy_driver", Args: map[string]any{"name": "goodwe_xs"}, OutcomeOK: false, OutcomeMsg: "config parse error"})

	md := a.RenderMarkdown()
	if !strings.Contains(md, "ftw_api") || !strings.Contains(md, "deploy_driver") {
		t.Fatalf("expected both tools in markdown, got:\n%s", md)
	}
	if !strings.Contains(md, "200 OK") {
		t.Fatalf("expected outcome string in markdown:\n%s", md)
	}
}

func TestAuditFileDiffTracking(t *testing.T) {
	a := NewAudit()
	a.RecordFileWrite("drivers/goodwe_xs.lua", "OLD", "NEW")
	md := a.RenderMarkdown()
	if !strings.Contains(md, "goodwe_xs.lua") {
		t.Fatalf("expected file path in diff section:\n%s", md)
	}
	if !strings.Contains(md, "-OLD") || !strings.Contains(md, "+NEW") {
		t.Fatalf("expected unified diff in markdown:\n%s", md)
	}
}
```

- [ ] **Step 2: Run test — FAIL**

- [ ] **Step 3: Implement audit**

`go/cmd/ftw-pair/audit.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

type AuditEvent struct {
	At         time.Time      `json:"at"`
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args"`
	OutcomeOK  bool           `json:"ok"`
	OutcomeMsg string         `json:"outcome"`
}

type fileEdit struct {
	Path   string
	Before string
	After  string
}

type Audit struct {
	mu     sync.Mutex
	events []AuditEvent
	edits  []fileEdit
}

func NewAudit() *Audit { return &Audit{} }

func (a *Audit) Append(e AuditEvent) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	a.mu.Lock()
	a.events = append(a.events, e)
	a.mu.Unlock()
}

func (a *Audit) RecordFileWrite(path, before, after string) {
	a.mu.Lock()
	a.edits = append(a.edits, fileEdit{Path: path, Before: before, After: after})
	a.mu.Unlock()
}

func (a *Audit) Events() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuditEvent, len(a.events))
	copy(out, a.events)
	return out
}

func (a *Audit) ToolCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.events)
}

func (a *Audit) LastTools(n int) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n > len(a.events) {
		n = len(a.events)
	}
	out := make([]string, 0, n)
	for _, e := range a.events[len(a.events)-n:] {
		out = append(out, e.Tool)
	}
	return out
}

func (a *Audit) RenderMarkdown() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	b.WriteString("## Tool-call log\n\n")
	for _, e := range a.events {
		argJSON, _ := json.Marshal(e.Args)
		status := "ok"
		if !e.OutcomeOK {
			status = "error"
		}
		fmt.Fprintf(&b, "- `%s` %s — %s — %s — %s\n",
			e.At.UTC().Format(time.RFC3339), e.Tool, string(argJSON), status, e.OutcomeMsg)
	}
	if len(a.edits) > 0 {
		b.WriteString("\n## File edits\n\n")
		for _, e := range a.edits {
			fmt.Fprintf(&b, "### %s\n\n```diff\n", e.Path)
			b.WriteString(unifiedDiff(e.Before, e.After))
			b.WriteString("\n```\n\n")
		}
	}
	return b.String()
}

func unifiedDiff(before, after string) string {
	// Bytes-level — keep it simple. A real diff lib is overkill given
	// the inputs are typically small driver files / config snippets.
	bls := strings.Split(before, "\n")
	als := strings.Split(after, "\n")
	var b strings.Builder
	for _, l := range bls {
		fmt.Fprintf(&b, "-%s\n", l)
	}
	for _, l := range als {
		fmt.Fprintf(&b, "+%s\n", l)
	}
	return strings.TrimRight(b.String(), "\n")
}
```

- [ ] **Step 4: Run tests — PASS**

```bash
cd go/cmd/ftw-pair && go test -run TestAudit -v
```

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/audit.go go/cmd/ftw-pair/audit_test.go
git commit -m "feat(pair): audit log + markdown render for PR session report"
```

---

## Task 4: Path-scope enforcement

**Files:**
- Create: `go/cmd/ftw-pair/scope.go`
- Create: `go/cmd/ftw-pair/scope_test.go`

- [ ] **Step 1: Failing test (cover `..` traversal, symlink escape, in-scope)**

`go/cmd/ftw-pair/scope_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScopeAllowsRepoStateAndTmp(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	sc := NewScope(repo, state)

	// Valid: inside repo
	if _, err := sc.Resolve(filepath.Join(repo, "drivers", "foo.lua")); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	// Valid: inside state
	if _, err := sc.Resolve(filepath.Join(state, "state.db")); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	// Valid: /tmp
	if _, err := sc.Resolve(filepath.Join(os.TempDir(), "x")); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestScopeRejectsTraversal(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	sc := NewScope(repo, state)

	cases := []string{
		"/etc/passwd",
		filepath.Join(repo, "..", "..", "etc", "passwd"),
		"~/.ssh/id_rsa",
		"",
	}
	for _, p := range cases {
		if _, err := sc.Resolve(p); err == nil {
			t.Fatalf("expected scope-reject for %q", p)
		}
	}
}

func TestScopeRejectsSymlinkEscape(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	sc := NewScope(repo, state)

	link := filepath.Join(repo, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := sc.Resolve(filepath.Join(link, "passwd")); err == nil {
		t.Fatal("expected reject on symlink-escape")
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement scope**

`go/cmd/ftw-pair/scope.go`:

```go
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type Scope struct {
	roots []string // canonicalized
}

func NewScope(repoDir, stateDir string) *Scope {
	roots := []string{repoDir, stateDir, os.TempDir()}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if abs, err := filepath.Abs(r); err == nil {
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				out = append(out, resolved)
			} else {
				out = append(out, abs)
			}
		}
	}
	return &Scope{roots: out}
}

var ErrOutOfScope = errors.New("path is outside the allowed scope (repo, state-dir, /tmp)")

// Resolve canonicalizes p (resolving symlinks of all *existing* prefix
// components) and verifies the result is under one of the configured
// roots. Returns the canonical absolute path. Use this output for any
// subsequent fs op so callers can't trick us by passing a different
// path representation.
func (s *Scope) Resolve(p string) (string, error) {
	if p == "" {
		return "", ErrOutOfScope
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	// Walk down existing components and EvalSymlinks the deepest
	// existing one — this catches "symlink to /etc + /passwd" without
	// requiring the file to exist before the write.
	dir := abs
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolvedDir = dir
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return "", err
	}
	canonical := filepath.Join(resolvedDir, rel)

	for _, root := range s.roots {
		if canonical == root || strings.HasPrefix(canonical, root+string(filepath.Separator)) {
			return canonical, nil
		}
	}
	return "", ErrOutOfScope
}
```

- [ ] **Step 4: Run — PASS**

```bash
cd go/cmd/ftw-pair && go test -run TestScope -v
```

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/scope.go go/cmd/ftw-pair/scope_test.go
git commit -m "feat(pair): path-scope enforcement (repo + state-dir + /tmp, blocks symlink escape)"
```

---

## Task 5: MCP server scaffold

**Files:**
- Create: `go/cmd/ftw-pair/mcp.go`
- Create: `go/cmd/ftw-pair/mcp_test.go`

Wire up the MCP server skeleton. Tools register themselves into a registry; this task ships an empty registry and verifies the server boots, responds to `tools/list`, and shuts down cleanly when the session ends.

- [ ] **Step 1: Failing test**

`go/cmd/ftw-pair/mcp_test.go`:

```go
package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMCPServerBootAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := NewSession(ctx, SessionConfig{TTL: time.Hour})
	defer sess.End("test_cleanup")

	srv, err := StartMCP(ctx, MCPConfig{
		Addr:    "127.0.0.1:0",
		Session: sess,
		Audit:   NewAudit(),
		Tools:   nil, // empty registry
	})
	if err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	defer srv.Shutdown(context.Background())

	// GET /healthz should always answer.
	resp, err := http.Get("http://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "ok") {
		t.Fatalf("expected ok, got %d %s", resp.StatusCode, body)
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement MCP server**

`go/cmd/ftw-pair/mcp.go`:

```go
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool is the local interface every tool handler implements. We don't
// use the raw mcpsdk types in our own code so the rest of the codebase
// stays vendor-independent.
type Tool interface {
	Name() string
	Schema() mcpsdk.Tool
	Handle(ctx context.Context, args map[string]any) (any, error)
}

type MCPConfig struct {
	Addr    string
	Session *Session
	Audit   *Audit
	Tools   []Tool
}

type MCPServer struct {
	httpSrv *http.Server
	ln      net.Listener
}

func (s *MCPServer) Addr() string { return s.ln.Addr().String() }

func (s *MCPServer) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

func StartMCP(ctx context.Context, cfg MCPConfig) (*MCPServer, error) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "ftw-pair",
		Version: Version,
	}, nil)

	for _, t := range cfg.Tools {
		tool := t // capture
		mcpsdk.AddTool(server, tool.Schema(), func(ctx context.Context, req *mcpsdk.CallToolRequest, args map[string]any) (*mcpsdk.CallToolResult, any, error) {
			out, err := tool.Handle(ctx, args)
			ok := err == nil
			msg := "ok"
			if err != nil {
				msg = err.Error()
			}
			cfg.Audit.Append(AuditEvent{Tool: tool.Name(), Args: args, OutcomeOK: ok, OutcomeMsg: msg})
			if err != nil {
				return nil, nil, err
			}
			return nil, out, nil
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.Handle("/mcp", mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	httpSrv := &http.Server{Handler: mux}
	go httpSrv.Serve(ln)

	// Cooperative shutdown when the session ends.
	go func() {
		select {
		case <-cfg.Session.Done():
		case <-ctx.Done():
		}
		_ = httpSrv.Shutdown(context.Background())
	}()

	return &MCPServer{httpSrv: httpSrv, ln: ln}, nil
}
```

- [ ] **Step 4: Run — PASS**

```bash
cd go/cmd/ftw-pair && go test -run TestMCP -v
```

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/mcp.go go/cmd/ftw-pair/mcp_test.go go/go.mod go/go.sum
git commit -m "feat(pair): MCP server scaffold + tool registry interface"
```

---

## Task 6: Tool — `ftw_api`

**Files:**
- Create: `go/cmd/ftw-pair/tool_api.go`
- Create: `go/cmd/ftw-pair/tool_api_test.go`

The single biggest tool — proxies HTTP requests to the running main service. Use it for everything 42W already exposes.

- [ ] **Step 1: Failing test**

`go/cmd/ftw-pair/tool_api_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToolFtwAPI_GETStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/status" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"mode": "test"})
	}))
	defer upstream.Close()

	tool := NewFtwAPITool(upstream.URL)
	out, err := tool.Handle(context.Background(), map[string]any{
		"method": "GET",
		"path":   "/api/status",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"mode":"test"`) {
		t.Fatalf("expected proxied body, got %s", b)
	}
}

func TestToolFtwAPI_RejectsAbsoluteURL(t *testing.T) {
	tool := NewFtwAPITool("http://localhost:8080")
	_, err := tool.Handle(context.Background(), map[string]any{
		"method": "GET",
		"path":   "http://attacker.example/api/x",
	})
	if err == nil {
		t.Fatal("expected reject of absolute URL")
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_api.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type FtwAPITool struct {
	baseURL string
	client  *http.Client
}

func NewFtwAPITool(baseURL string) *FtwAPITool {
	return &FtwAPITool{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{},
	}
}

func (t *FtwAPITool) Name() string { return "ftw_api" }

func (t *FtwAPITool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "ftw_api",
		Description: "Proxy an HTTP request to the running forty-two-watts service on localhost:8080. Path MUST start with /api/. Read docs/api.md for the available endpoints.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"method": {Type: "string", Description: "HTTP method (GET/POST/PUT/DELETE)"},
				"path":   {Type: "string", Description: "Path starting with /api/"},
				"body":   {Type: "object", Description: "Optional JSON body for write methods"},
			},
			Required: []string{"method", "path"},
		},
	}
}

func (t *FtwAPITool) Handle(ctx context.Context, args map[string]any) (any, error) {
	method, _ := args["method"].(string)
	path, _ := args["path"].(string)
	if method == "" || path == "" {
		return nil, fmt.Errorf("method and path are required")
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path must start with / (got %q)", path)
	}

	var body io.Reader
	if b, ok := args["body"]; ok && b != nil {
		buf, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), t.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// Try to surface JSON cleanly; fall back to raw string.
	var parsed any
	if json.Unmarshal(respBody, &parsed) == nil {
		return map[string]any{
			"status": resp.StatusCode,
			"body":   parsed,
		}, nil
	}
	return map[string]any{
		"status": resp.StatusCode,
		"body":   string(respBody),
	}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_api.go go/cmd/ftw-pair/tool_api_test.go
git commit -m "feat(pair): ftw_api tool — generic proxy to localhost:8080"
```

---

## Task 7: Filesystem tools

**Files:**
- Create: `go/cmd/ftw-pair/tool_fs.go`
- Create: `go/cmd/ftw-pair/tool_fs_test.go`

Three tools: `read_file`, `write_file`, `list_directory`. All scope-gated via `Scope.Resolve`. `write_file` calls `audit.RecordFileWrite` so the PR report carries the diff.

- [ ] **Step 1: Failing test**

`go/cmd/ftw-pair/tool_fs_test.go`:

```go
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupScope(t *testing.T) (string, *Scope, *Audit) {
	t.Helper()
	repo := t.TempDir()
	state := t.TempDir()
	return repo, NewScope(repo, state), NewAudit()
}

func TestToolReadWriteRoundTrip(t *testing.T) {
	repo, sc, a := setupScope(t)
	w := NewWriteFileTool(sc, a)
	r := NewReadFileTool(sc)

	p := filepath.Join(repo, "x.txt")
	if _, err := w.Handle(context.Background(), map[string]any{"path": p, "content": "hello"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := r.Handle(context.Background(), map[string]any{"path": p})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m := out.(map[string]any)
	if m["content"] != "hello" {
		t.Fatalf("expected hello, got %v", m["content"])
	}
}

func TestToolFSOutOfScopeRejected(t *testing.T) {
	_, sc, a := setupScope(t)
	w := NewWriteFileTool(sc, a)
	if _, err := w.Handle(context.Background(), map[string]any{"path": "/etc/passwd", "content": ":"}); err == nil {
		t.Fatal("expected scope reject")
	}
}

func TestToolWriteRecordsDiff(t *testing.T) {
	repo, sc, a := setupScope(t)
	p := filepath.Join(repo, "y.txt")
	_ = os.WriteFile(p, []byte("OLD"), 0o644)
	w := NewWriteFileTool(sc, a)
	if _, err := w.Handle(context.Background(), map[string]any{"path": p, "content": "NEW"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	md := a.RenderMarkdown()
	if !strings.Contains(md, "-OLD") || !strings.Contains(md, "+NEW") {
		t.Fatalf("expected diff recorded, got:\n%s", md)
	}
}

func TestToolListDirectory(t *testing.T) {
	repo, sc, _ := setupScope(t)
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte{}, 0o644)
	_ = os.Mkdir(filepath.Join(repo, "sub"), 0o755)
	ls := NewListDirectoryTool(sc)
	out, err := ls.Handle(context.Background(), map[string]any{"path": repo})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	entries := out.(map[string]any)["entries"].([]map[string]any)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_fs.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ReadFileTool struct{ scope *Scope }

func NewReadFileTool(s *Scope) *ReadFileTool { return &ReadFileTool{scope: s} }
func (t *ReadFileTool) Name() string         { return "read_file" }
func (t *ReadFileTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "read_file",
		Description: "Read a file inside the repo, state dir, or /tmp. Returns content as UTF-8 string.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"path": {Type: "string"},
			},
			Required: []string{"path"},
		},
	}
}
func (t *ReadFileTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	p, _ := args["path"].(string)
	abs, err := t.scope.Resolve(p)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": abs, "content": string(b), "size": len(b)}, nil
}

type WriteFileTool struct {
	scope *Scope
	audit *Audit
}

func NewWriteFileTool(s *Scope, a *Audit) *WriteFileTool { return &WriteFileTool{scope: s, audit: a} }
func (t *WriteFileTool) Name() string                    { return "write_file" }
func (t *WriteFileTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "write_file",
		Description: "Write a file inside the repo, state dir, or /tmp. Creates parents as needed.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"path":    {Type: "string"},
				"content": {Type: "string"},
			},
			Required: []string{"path", "content"},
		},
	}
}
func (t *WriteFileTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	p, _ := args["path"].(string)
	c, _ := args["content"].(string)
	abs, err := t.scope.Resolve(p)
	if err != nil {
		return nil, err
	}
	before, _ := os.ReadFile(abs)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
		return nil, err
	}
	t.audit.RecordFileWrite(abs, string(before), c)
	return map[string]any{"path": abs, "bytes": len(c)}, nil
}

type ListDirectoryTool struct{ scope *Scope }

func NewListDirectoryTool(s *Scope) *ListDirectoryTool { return &ListDirectoryTool{scope: s} }
func (t *ListDirectoryTool) Name() string              { return "list_directory" }
func (t *ListDirectoryTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "list_directory",
		Description: "List entries in a directory (non-recursive).",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"path": {Type: "string"},
			},
			Required: []string{"path"},
		},
	}
}
func (t *ListDirectoryTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	p, _ := args["path"].(string)
	abs, err := t.scope.Resolve(p)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		entry := map[string]any{
			"name": e.Name(),
			"dir":  e.IsDir(),
		}
		if info != nil {
			entry["size"] = info.Size()
		}
		out = append(out, entry)
	}
	if out == nil {
		return nil, fmt.Errorf("nil read") // unreachable but keeps lint happy
	}
	return map[string]any{"entries": out}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_fs.go go/cmd/ftw-pair/tool_fs_test.go
git commit -m "feat(pair): read_file / write_file / list_directory tools (scope-gated)"
```

---

## Task 8: Shell tool — `run_command`

**Files:**
- Create: `go/cmd/ftw-pair/tool_shell.go`
- Create: `go/cmd/ftw-pair/tool_shell_test.go`

- [ ] **Step 1: Failing test**

`go/cmd/ftw-pair/tool_shell_test.go`:

```go
package main

import (
	"context"
	"strings"
	"testing"
)

func TestToolRunCommandEcho(t *testing.T) {
	repo, sc, _ := setupScope(t)
	rc := NewRunCommandTool(sc)
	out, err := rc.Handle(context.Background(), map[string]any{
		"cmd":     "echo hello-from-pair",
		"workdir": repo,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := out.(map[string]any)
	if !strings.Contains(m["stdout"].(string), "hello-from-pair") {
		t.Fatalf("expected stdout, got %v", m)
	}
	if m["exit_code"].(int) != 0 {
		t.Fatalf("expected exit 0, got %v", m["exit_code"])
	}
}

func TestToolRunCommandWorkdirOutOfScope(t *testing.T) {
	_, sc, _ := setupScope(t)
	rc := NewRunCommandTool(sc)
	if _, err := rc.Handle(context.Background(), map[string]any{
		"cmd":     "ls",
		"workdir": "/etc",
	}); err == nil {
		t.Fatal("expected scope reject for /etc")
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_shell.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type RunCommandTool struct{ scope *Scope }

func NewRunCommandTool(s *Scope) *RunCommandTool { return &RunCommandTool{scope: s} }
func (t *RunCommandTool) Name() string           { return "run_command" }
func (t *RunCommandTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "run_command",
		Description: "Run a shell command. workdir must be inside the allowed scope (repo, state-dir, /tmp). Default timeout 30s.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"cmd":        {Type: "string"},
				"workdir":    {Type: "string"},
				"timeout_s":  {Type: "integer", Description: "Optional, default 30, max 600"},
			},
			Required: []string{"cmd", "workdir"},
		},
	}
}
func (t *RunCommandTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	cmd, _ := args["cmd"].(string)
	wd, _ := args["workdir"].(string)
	if cmd == "" || wd == "" {
		return nil, fmt.Errorf("cmd and workdir are required")
	}
	abs, err := t.scope.Resolve(wd)
	if err != nil {
		return nil, err
	}

	timeoutS := 30
	if v, ok := args["timeout_s"].(float64); ok && v > 0 {
		if v > 600 {
			v = 600
		}
		timeoutS = int(v)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS)*time.Second)
	defer cancel()

	c := exec.CommandContext(cctx, "/bin/sh", "-c", cmd)
	c.Dir = abs

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err = c.Run()
	exitCode := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
		err = nil
	} else if err != nil {
		return nil, err
	}
	return map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": exitCode,
	}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_shell.go go/cmd/ftw-pair/tool_shell_test.go
git commit -m "feat(pair): run_command tool with workdir scope + timeout"
```

---

## Task 9: Service tools

**Files:**
- Create: `go/cmd/ftw-pair/tool_service.go`
- Create: `go/cmd/ftw-pair/tool_service_test.go`

`restart_main_service` shells to `systemctl restart forty-two-watts`. `tail_service_logs` reads from journalctl. Inject the actual command via a function var so tests don't need systemd.

- [ ] **Step 1: Failing test**

`go/cmd/ftw-pair/tool_service_test.go`:

```go
package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRestartMainServiceCallsSystemctl(t *testing.T) {
	var got []string
	r := &RestartMainServiceTool{run: func(ctx context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("ok"), nil
	}}
	if _, err := r.Handle(context.Background(), nil); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if strings.Join(got, " ") != "systemctl restart forty-two-watts" {
		t.Fatalf("unexpected args: %v", got)
	}
}

func TestRestartMainServiceSurfacesError(t *testing.T) {
	r := &RestartMainServiceTool{run: func(context.Context, ...string) ([]byte, error) {
		return []byte("permission denied"), errors.New("exit 1")
	}}
	if _, err := r.Handle(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestTailServiceLogsReadsJournalctl(t *testing.T) {
	tl := &TailServiceLogsTool{run: func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("Jan 01 00:00:00 host ftw[1]: started\n"), nil
	}}
	out, err := tl.Handle(context.Background(), map[string]any{"since": "10m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.(map[string]any)["log"].(string), "started") {
		t.Fatalf("expected log content")
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_service.go`:

```go
package main

import (
	"context"
	"fmt"
	"os/exec"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func defaultExec(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("no args")
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	return c.CombinedOutput()
}

type RestartMainServiceTool struct {
	run func(ctx context.Context, args ...string) ([]byte, error)
}

func NewRestartMainServiceTool() *RestartMainServiceTool {
	return &RestartMainServiceTool{run: defaultExec}
}
func (t *RestartMainServiceTool) Name() string { return "restart_main_service" }
func (t *RestartMainServiceTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "restart_main_service",
		Description: "Restart the forty-two-watts systemd unit. Drops dispatch for ~5s — drivers re-init from config.",
		InputSchema: &mcpsdk.Schema{Type: "object"},
	}
}
func (t *RestartMainServiceTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	out, err := t.run(ctx, "systemctl", "restart", "forty-two-watts")
	if err != nil {
		return nil, fmt.Errorf("systemctl: %w (%s)", err, out)
	}
	return map[string]any{"ok": true, "output": string(out)}, nil
}

type TailServiceLogsTool struct {
	run func(ctx context.Context, args ...string) ([]byte, error)
}

func NewTailServiceLogsTool() *TailServiceLogsTool {
	return &TailServiceLogsTool{run: defaultExec}
}
func (t *TailServiceLogsTool) Name() string { return "tail_service_logs" }
func (t *TailServiceLogsTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "tail_service_logs",
		Description: "Read recent journalctl entries for forty-two-watts.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"since": {Type: "string", Description: "journalctl --since value, e.g. '10m', '1h', '2026-05-25 14:00'"},
				"lines": {Type: "integer", Description: "Optional max lines (default 500)"},
			},
		},
	}
}
func (t *TailServiceLogsTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	since, _ := args["since"].(string)
	if since == "" {
		since = "30m"
	}
	lines := "500"
	if v, ok := args["lines"].(float64); ok && v > 0 {
		lines = fmt.Sprintf("%d", int(v))
	}
	out, err := t.run(ctx, "journalctl", "-u", "forty-two-watts", "--since", since, "-n", lines, "--no-pager")
	if err != nil {
		return nil, err
	}
	return map[string]any{"log": string(out)}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_service.go go/cmd/ftw-pair/tool_service_test.go
git commit -m "feat(pair): restart_main_service + tail_service_logs tools"
```

---

## Task 10: Network discovery — `network_scan` + `http_probe`

**Files:**
- Create: `go/cmd/ftw-pair/tool_net.go`
- Create: `go/cmd/ftw-pair/tool_net_test.go`

`network_scan` reads `/proc/net/arp` on Linux (the only target — host is a Pi). `http_probe` does a simple GET with a 5s timeout. Both small and safe.

- [ ] **Step 1: Failing test**

`go/cmd/ftw-pair/tool_net_test.go`:

```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "ProbeTest/1.0")
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	}))
	defer srv.Close()

	tool := NewHTTPProbeTool()
	out, err := tool.Handle(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["status"].(int) != 200 {
		t.Fatalf("status: %v", m["status"])
	}
	if !strings.Contains(m["body"].(string), "OK") {
		t.Fatalf("body: %v", m["body"])
	}
}

func TestNetworkScanReadsArpFile(t *testing.T) {
	tool := &NetworkScanTool{
		arpReader: func() (string, error) {
			return `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.10     0x1         0x2         aa:bb:cc:dd:ee:ff     *        eth0
192.168.1.20     0x1         0x2         11:22:33:44:55:66     *        eth0`, nil
		},
	}
	out, err := tool.Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := out.(map[string]any)["entries"].([]map[string]string)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0]["ip"] != "192.168.1.10" {
		t.Fatalf("first ip: %v", entries[0])
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_net.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type NetworkScanTool struct {
	// arpReader is injected for tests; default reads /proc/net/arp.
	arpReader func() (string, error)
}

func NewNetworkScanTool() *NetworkScanTool {
	return &NetworkScanTool{arpReader: defaultArpReader}
}

func defaultArpReader() (string, error) {
	b, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (t *NetworkScanTool) Name() string { return "network_scan" }
func (t *NetworkScanTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "network_scan",
		Description: "Read /proc/net/arp on the host. Returns IP+MAC tuples for every neighbor the host has seen. Linux only.",
		InputSchema: &mcpsdk.Schema{Type: "object"},
	}
}
func (t *NetworkScanTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	raw, err := t.arpReader()
	if err != nil {
		return nil, err
	}
	var out []map[string]string
	for i, line := range strings.Split(raw, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		out = append(out, map[string]string{
			"ip":     fields[0],
			"mac":    fields[3],
			"device": fields[5],
		})
	}
	return map[string]any{"entries": out}, nil
}

type HTTPProbeTool struct{ client *http.Client }

func NewHTTPProbeTool() *HTTPProbeTool {
	return &HTTPProbeTool{client: &http.Client{Timeout: 5 * time.Second}}
}
func (t *HTTPProbeTool) Name() string { return "http_probe" }
func (t *HTTPProbeTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "http_probe",
		Description: "GET a URL. Returns status, headers, and up to 4KB of body.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"url": {Type: "string"},
			},
			Required: []string{"url"},
		},
	}
}
func (t *HTTPProbeTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url required")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return map[string]any{
		"status":  resp.StatusCode,
		"headers": resp.Header,
		"body":    string(body),
	}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_net.go go/cmd/ftw-pair/tool_net_test.go
git commit -m "feat(pair): network_scan + http_probe tools"
```

---

## Task 11: Modbus tools — `modbus_probe` + `modbus_write`

**Files:**
- Create: `go/cmd/ftw-pair/tool_modbus.go`
- Create: `go/cmd/ftw-pair/tool_modbus_test.go`

Wrap `github.com/simonvetter/modbus` (already a dep). Each call opens a fresh client — driver-dev usage is sporadic and connection pooling adds bugs.

- [ ] **Step 1: Failing test**

Test against a goroutine running an in-process modbus server. Pattern: use `github.com/simonvetter/modbus` `Server` type in test setup. See `go/test/e2e/` for examples of how the codebase already stubs modbus.

`go/cmd/ftw-pair/tool_modbus_test.go`:

```go
package main

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/simonvetter/modbus"
)

type stubHandler struct {
	regs []uint16
}

func (h *stubHandler) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	out := make([]uint16, req.Quantity)
	for i := range out {
		idx := int(req.Addr) + i
		if idx < len(h.regs) {
			out[i] = h.regs[idx]
		}
		if req.IsWrite && i < len(req.Args) {
			h.regs[idx] = req.Args[i]
		}
	}
	return out, nil
}
func (h *stubHandler) HandleCoils(*modbus.CoilsRequest) ([]bool, error) {
	return nil, nil
}
func (h *stubHandler) HandleDiscreteInputs(*modbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, nil
}
func (h *stubHandler) HandleInputRegisters(*modbus.InputRegistersRequest) ([]uint16, error) {
	return nil, nil
}

func startTestModbus(t *testing.T) string {
	t.Helper()
	h := &stubHandler{regs: []uint16{100, 200, 300, 400}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL: "tcp://127.0.0.1:" + strconv.Itoa(port),
	}, h)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(func() { srv.Stop() })
	return "127.0.0.1:" + strconv.Itoa(port)
}

func TestModbusProbeRead(t *testing.T) {
	addr := startTestModbus(t)
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	tool := NewModbusProbeTool()
	out, err := tool.Handle(context.Background(), map[string]any{
		"host":     host,
		"port":     float64(p),
		"unit_id":  float64(1),
		"register": float64(0),
		"count":    float64(4),
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	vals := out.(map[string]any)["values"].([]uint16)
	if len(vals) != 4 || vals[0] != 100 {
		t.Fatalf("unexpected vals: %v", vals)
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_modbus.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/simonvetter/modbus"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ModbusProbeTool struct{}

func NewModbusProbeTool() *ModbusProbeTool { return &ModbusProbeTool{} }
func (t *ModbusProbeTool) Name() string    { return "modbus_probe" }
func (t *ModbusProbeTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "modbus_probe",
		Description: "Read holding registers from a Modbus TCP device.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"host":     {Type: "string"},
				"port":     {Type: "integer"},
				"unit_id":  {Type: "integer"},
				"register": {Type: "integer"},
				"count":    {Type: "integer"},
			},
			Required: []string{"host", "port", "unit_id", "register", "count"},
		},
	}
}
func (t *ModbusProbeTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	host, _ := args["host"].(string)
	port := int(args["port"].(float64))
	unit := uint8(args["unit_id"].(float64))
	reg := uint16(args["register"].(float64))
	n := uint16(args["count"].(float64))

	cfg := &modbus.ClientConfiguration{
		URL:     fmt.Sprintf("tcp://%s:%d", host, port),
		Timeout: 3 * time.Second,
	}
	c, err := modbus.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Open(); err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetUnitId(unit)
	vals, err := c.ReadRegisters(reg, n, modbus.HOLDING_REGISTER)
	if err != nil {
		return nil, err
	}
	return map[string]any{"values": vals}, nil
}

type ModbusWriteTool struct{}

func NewModbusWriteTool() *ModbusWriteTool { return &ModbusWriteTool{} }
func (t *ModbusWriteTool) Name() string    { return "modbus_write" }
func (t *ModbusWriteTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "modbus_write",
		Description: "Write a single holding register on a Modbus TCP device. Use with care — this can move physical hardware.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"host":     {Type: "string"},
				"port":     {Type: "integer"},
				"unit_id":  {Type: "integer"},
				"register": {Type: "integer"},
				"value":    {Type: "integer"},
			},
			Required: []string{"host", "port", "unit_id", "register", "value"},
		},
	}
}
func (t *ModbusWriteTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	host, _ := args["host"].(string)
	port := int(args["port"].(float64))
	unit := uint8(args["unit_id"].(float64))
	reg := uint16(args["register"].(float64))
	val := uint16(args["value"].(float64))

	cfg := &modbus.ClientConfiguration{
		URL:     fmt.Sprintf("tcp://%s:%d", host, port),
		Timeout: 3 * time.Second,
	}
	c, err := modbus.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Open(); err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetUnitId(unit)
	if err := c.WriteRegister(reg, val); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_modbus.go go/cmd/ftw-pair/tool_modbus_test.go
git commit -m "feat(pair): modbus_probe + modbus_write tools"
```

---

## Task 12: MQTT observation — `mqtt_observe`

**Files:**
- Create: `go/cmd/ftw-pair/tool_mqtt.go`
- Create: `go/cmd/ftw-pair/tool_mqtt_test.go`

Subscribe to a topic glob on a broker, collect messages for `duration_s` (default 10, max 60), return them as a list.

- [ ] **Step 1: Failing test**

Use the `mochi-mqtt` server (already a dep) as an in-process broker for the test.

`go/cmd/ftw-pair/tool_mqtt_test.go`:

```go
package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	paho "github.com/eclipse/paho.mqtt.golang"
)

func startTestBroker(t *testing.T) string {
	t.Helper()
	srv := mqttserver.New(nil)
	_ = srv.AddHook(new(auth.AllowHook), nil)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "t", Address: "127.0.0.1:" + strconv.Itoa(port)})); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	time.Sleep(50 * time.Millisecond)
	return "tcp://127.0.0.1:" + strconv.Itoa(port)
}

func TestMQTTObserveCollects(t *testing.T) {
	url := startTestBroker(t)

	go func() {
		opts := paho.NewClientOptions().AddBroker(url).SetClientID("pub")
		c := paho.NewClient(opts)
		token := c.Connect()
		token.WaitTimeout(2 * time.Second)
		time.Sleep(100 * time.Millisecond) // let observer subscribe
		c.Publish("test/topic", 0, false, "hello").WaitTimeout(time.Second)
	}()

	tool := NewMQTTObserveTool()
	out, err := tool.Handle(context.Background(), map[string]any{
		"broker":     url,
		"topic":      "test/#",
		"duration_s": float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := out.(map[string]any)["messages"].([]map[string]any)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	if msgs[0]["topic"] != "test/topic" || msgs[0]["payload"] != "hello" {
		t.Fatalf("unexpected message: %v", msgs[0])
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_mqtt.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type MQTTObserveTool struct{}

func NewMQTTObserveTool() *MQTTObserveTool { return &MQTTObserveTool{} }
func (t *MQTTObserveTool) Name() string    { return "mqtt_observe" }
func (t *MQTTObserveTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "mqtt_observe",
		Description: "Subscribe to a topic glob on an MQTT broker, return everything received during the window. Capped at 60s and 500 messages.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"broker":     {Type: "string", Description: "tcp://host:port"},
				"topic":      {Type: "string", Description: "Topic glob, e.g. extapi/#"},
				"duration_s": {Type: "integer", Description: "Window length in seconds (default 10, max 60)"},
			},
			Required: []string{"broker", "topic"},
		},
	}
}
func (t *MQTTObserveTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	broker, _ := args["broker"].(string)
	topic, _ := args["topic"].(string)
	if broker == "" || topic == "" {
		return nil, fmt.Errorf("broker and topic required")
	}
	dur := 10 * time.Second
	if v, ok := args["duration_s"].(float64); ok && v > 0 {
		if v > 60 {
			v = 60
		}
		dur = time.Duration(v) * time.Second
	}

	opts := paho.NewClientOptions().AddBroker(broker).SetClientID(fmt.Sprintf("ftw-pair-%d", time.Now().UnixNano()))
	client := paho.NewClient(opts)
	if t := client.Connect(); !t.WaitTimeout(5 * time.Second) || t.Error() != nil {
		return nil, fmt.Errorf("connect: %v", t.Error())
	}
	defer client.Disconnect(100)

	var mu sync.Mutex
	out := make([]map[string]any, 0, 64)
	cap := 500

	if t := client.Subscribe(topic, 0, func(_ paho.Client, m paho.Message) {
		mu.Lock()
		defer mu.Unlock()
		if len(out) >= cap {
			return
		}
		out = append(out, map[string]any{
			"topic":   m.Topic(),
			"payload": string(m.Payload()),
			"qos":     m.Qos(),
			"at":      time.Now().UTC().Format(time.RFC3339Nano),
		})
	}); !t.WaitTimeout(5*time.Second) || t.Error() != nil {
		return nil, fmt.Errorf("subscribe: %v", t.Error())
	}

	timer := time.NewTimer(dur)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}

	mu.Lock()
	defer mu.Unlock()
	return map[string]any{"messages": out}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_mqtt.go go/cmd/ftw-pair/tool_mqtt_test.go
git commit -m "feat(pair): mqtt_observe tool"
```

---

## Task 13: `pcap_capture` tool

**Files:**
- Create: `go/cmd/ftw-pair/tool_pcap.go`
- Create: `go/cmd/ftw-pair/tool_pcap_test.go`

Shell to `tcpdump -i <iface> -w <tmp.pcap> -G <duration> -W 1 <filter>` and return the pcap file path. Requires `tcpdump` on the host; documented in operations.md. The friend reads the file back via `read_file` (it lands in `/tmp`).

- [ ] **Step 1: Failing test (with injected runner)**

`go/cmd/ftw-pair/tool_pcap_test.go`:

```go
package main

import (
	"context"
	"strings"
	"testing"
)

func TestPCapCallsTcpdump(t *testing.T) {
	var got []string
	tool := &PCapCaptureTool{run: func(ctx context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte(""), nil
	}}
	out, err := tool.Handle(context.Background(), map[string]any{
		"interface":  "eth0",
		"bpf_filter": "tcp port 502",
		"duration_s": float64(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(got, " ")
	if !strings.Contains(cmd, "tcpdump") || !strings.Contains(cmd, "tcp port 502") {
		t.Fatalf("expected tcpdump call, got: %s", cmd)
	}
	path := out.(map[string]any)["pcap_path"].(string)
	if !strings.HasPrefix(path, "/tmp/ftw-pair-") || !strings.HasSuffix(path, ".pcap") {
		t.Fatalf("unexpected path: %s", path)
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_pcap.go`:

```go
package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type PCapCaptureTool struct {
	run func(ctx context.Context, args ...string) ([]byte, error)
}

func NewPCapCaptureTool() *PCapCaptureTool {
	return &PCapCaptureTool{run: func(ctx context.Context, args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, args[0], args[1:]...)
		return c.CombinedOutput()
	}}
}
func (t *PCapCaptureTool) Name() string { return "pcap_capture" }
func (t *PCapCaptureTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "pcap_capture",
		Description: "Capture network traffic with tcpdump on the host. Returns the path of the resulting pcap (under /tmp, readable via read_file). Capped at 60s.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"interface":  {Type: "string"},
				"bpf_filter": {Type: "string", Description: "Berkeley packet-filter expression"},
				"duration_s": {Type: "integer", Description: "Capture window, max 60"},
			},
			Required: []string{"interface", "bpf_filter", "duration_s"},
		},
	}
}
func (t *PCapCaptureTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	iface, _ := args["interface"].(string)
	filter, _ := args["bpf_filter"].(string)
	dur := args["duration_s"].(float64)
	if dur <= 0 || dur > 60 {
		return nil, fmt.Errorf("duration_s must be 1..60")
	}
	path := filepath.Join("/tmp", fmt.Sprintf("ftw-pair-%d.pcap", time.Now().UnixNano()))
	out, err := t.run(ctx,
		"tcpdump", "-i", iface, "-w", path, "-G", strconv.Itoa(int(dur)), "-W", "1", filter,
	)
	if err != nil {
		return nil, fmt.Errorf("tcpdump: %w (%s)", err, out)
	}
	return map[string]any{"pcap_path": path}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_pcap.go go/cmd/ftw-pair/tool_pcap_test.go
git commit -m "feat(pair): pcap_capture tool (shells to tcpdump)"
```

---

## Task 14: `deploy_driver` — multi-step wrapper

**Files:**
- Create: `go/cmd/ftw-pair/tool_deploy.go`
- Create: `go/cmd/ftw-pair/tool_deploy_test.go`

Encodes 42W-specific knowledge: write `drivers/<name>.lua`, ensure a `drivers:` entry exists in `config.yaml`, wait up to 5s for the configreload watcher to pick it up, then poll `/api/drivers/<name>` until `tick_count > 0` or 10s elapsed. Returns success or the last error.

- [ ] **Step 1: Failing test**

`go/cmd/ftw-pair/tool_deploy_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeployDriverHappyPath(t *testing.T) {
	repo := t.TempDir()
	driverDir := filepath.Join(repo, "drivers")
	cfgPath := filepath.Join(repo, "config.yaml")
	_ = os.MkdirAll(driverDir, 0o755)
	_ = os.WriteFile(cfgPath, []byte("drivers: []\n"), 0o644)

	var ticks atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ticks.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"name":       "goodwe_xs",
			"status":     "ok",
			"tick_count": ticks.Load(),
		})
	}))
	defer upstream.Close()

	sc := NewScope(repo, t.TempDir())
	a := NewAudit()
	tool := NewDeployDriverTool(sc, a, upstream.URL, cfgPath)
	out, err := tool.Handle(context.Background(), map[string]any{
		"name":       "goodwe_xs",
		"lua_source": "-- minimal driver\nfunction driver_init() end\nfunction driver_poll() end\n",
		"config":     map[string]any{"capabilities": []string{"pv"}, "lua": "drivers/goodwe_xs.lua"},
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	res := out.(map[string]any)
	if res["status"] != "ok" {
		t.Fatalf("status: %v", res)
	}
	luaPath := filepath.Join(driverDir, "goodwe_xs.lua")
	if _, err := os.Stat(luaPath); err != nil {
		t.Fatalf("lua not written: %v", err)
	}
	cfgBytes, _ := os.ReadFile(cfgPath)
	var raw map[string]any
	_ = yaml.Unmarshal(cfgBytes, &raw)
	drs, _ := raw["drivers"].([]any)
	if len(drs) != 1 {
		t.Fatalf("expected drivers list entry, got %v", raw)
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_deploy.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

type DeployDriverTool struct {
	scope      *Scope
	audit      *Audit
	apiBase    string
	configPath string
	client     *http.Client
}

func NewDeployDriverTool(s *Scope, a *Audit, apiBase, configPath string) *DeployDriverTool {
	return &DeployDriverTool{
		scope:      s,
		audit:      a,
		apiBase:    strings.TrimRight(apiBase, "/"),
		configPath: configPath,
		client:     &http.Client{Timeout: 5 * time.Second},
	}
}

func (t *DeployDriverTool) Name() string { return "deploy_driver" }
func (t *DeployDriverTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "deploy_driver",
		Description: "Write a driver Lua file, add/update its config.yaml entry, wait for the config-reload watcher to pick it up, and verify the driver ticked. Returns its health after the deploy.",
		InputSchema: &mcpsdk.Schema{
			Type: "object",
			Properties: map[string]*mcpsdk.Schema{
				"name":       {Type: "string"},
				"lua_source": {Type: "string"},
				"config":     {Type: "object", Description: "YAML map for the config.yaml drivers[] entry (excluding 'name')"},
			},
			Required: []string{"name", "lua_source", "config"},
		},
	}
}

func (t *DeployDriverTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	lua, _ := args["lua_source"].(string)
	cfg, _ := args["config"].(map[string]any)
	if name == "" || lua == "" || cfg == nil {
		return nil, fmt.Errorf("name, lua_source, config all required")
	}

	// 1. Write the lua file under <repo>/drivers/<name>.lua
	driverDir := filepath.Join(filepath.Dir(t.configPath), "drivers")
	luaPath := filepath.Join(driverDir, name+".lua")
	if _, err := t.scope.Resolve(luaPath); err != nil {
		return nil, err
	}
	beforeLua, _ := os.ReadFile(luaPath)
	if err := os.MkdirAll(driverDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(luaPath, []byte(lua), 0o644); err != nil {
		return nil, err
	}
	t.audit.RecordFileWrite(luaPath, string(beforeLua), lua)

	// 2. Edit config.yaml — add or replace the entry whose name matches.
	rawCfg, err := os.ReadFile(t.configPath)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(rawCfg, &doc); err != nil {
		return nil, err
	}
	drivers, _ := doc["drivers"].([]any)
	cfg["name"] = name
	if _, ok := cfg["lua"]; !ok {
		cfg["lua"] = "drivers/" + name + ".lua"
	}
	replaced := false
	for i, raw := range drivers {
		m, _ := raw.(map[string]any)
		if m != nil && m["name"] == name {
			drivers[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		drivers = append(drivers, cfg)
	}
	doc["drivers"] = drivers

	newCfg, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(t.configPath, newCfg, 0o644); err != nil {
		return nil, err
	}
	t.audit.RecordFileWrite(t.configPath, string(rawCfg), string(newCfg))

	// 3. Wait up to 10s for the driver to tick.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		st, err := t.getDriverStatus(ctx, name)
		if err == nil && st["tick_count"] != nil {
			if tc, _ := st["tick_count"].(float64); tc > 0 {
				return map[string]any{
					"status":     "ok",
					"driver":     name,
					"tick_count": tc,
					"detail":     st,
				}, nil
			}
		}
	}
	st, _ := t.getDriverStatus(ctx, name)
	return map[string]any{
		"status": "timeout_waiting_for_tick",
		"driver": name,
		"detail": st,
	}, nil
}

func (t *DeployDriverTool) getDriverStatus(ctx context.Context, name string) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", t.apiBase+"/api/drivers/"+name, nil)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return out, nil
}
```

- [ ] **Step 4: Run — PASS**

```bash
cd go/cmd/ftw-pair && go test -run TestDeployDriver -v
```

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_deploy.go go/cmd/ftw-pair/tool_deploy_test.go
git commit -m "feat(pair): deploy_driver — multi-step write + reload + verify"
```

---

## Task 15: Session tools

**Files:**
- Create: `go/cmd/ftw-pair/tool_session.go`
- Create: `go/cmd/ftw-pair/tool_session_test.go`

- [ ] **Step 1: Failing test**

```go
// go/cmd/ftw-pair/tool_session_test.go
package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionToolsReportState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := NewSession(ctx, SessionConfig{TTL: time.Hour, Intent: "test"})
	defer sess.End("test_cleanup")
	a := NewAudit()
	a.Append(AuditEvent{Tool: "ftw_api", OutcomeOK: true, OutcomeMsg: "ok"})

	lg := NewSessionLogTool(sess, a)
	out, err := lg.Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	md := out.(map[string]any)["markdown"].(string)
	if !strings.Contains(md, "intent") || !strings.Contains(md, "ftw_api") {
		t.Fatalf("expected intent + tool in markdown:\n%s", md)
	}

	rm := NewSessionRemainingTool(sess)
	r, _ := rm.Handle(context.Background(), nil)
	if r.(map[string]any)["seconds"].(int) <= 0 {
		t.Fatal("expected positive remaining")
	}
}

func TestSessionEndTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := NewSession(ctx, SessionConfig{TTL: time.Hour})
	end := NewSessionEndTool(sess)
	if _, err := end.Handle(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("session did not end")
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement**

`go/cmd/ftw-pair/tool_session.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SessionLogTool struct {
	sess  *Session
	audit *Audit
}

func NewSessionLogTool(s *Session, a *Audit) *SessionLogTool { return &SessionLogTool{sess: s, audit: a} }
func (t *SessionLogTool) Name() string                       { return "session_log" }
func (t *SessionLogTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "session_log",
		Description: "Render the full pair-session log as markdown. Paste this into the PR body when you're done.",
		InputSchema: &mcpsdk.Schema{Type: "object"},
	}
}
func (t *SessionLogTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Pair session report\n\n")
	fmt.Fprintf(&b, "- session_id: `%s`\n", t.sess.ID)
	fmt.Fprintf(&b, "- intent: %s\n", t.sess.Intent())
	if t.sess.As() != "" {
		fmt.Fprintf(&b, "- friend: %s\n", t.sess.As())
	}
	fmt.Fprintf(&b, "- started_at: %s\n", t.sess.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- exit_reason: %s\n\n", firstNonEmpty(t.sess.ExitReason(), "in_progress"))
	b.WriteString(t.audit.RenderMarkdown())
	return map[string]any{"markdown": b.String()}, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type SessionRemainingTool struct{ sess *Session }

func NewSessionRemainingTool(s *Session) *SessionRemainingTool { return &SessionRemainingTool{sess: s} }
func (t *SessionRemainingTool) Name() string                   { return "session_remaining" }
func (t *SessionRemainingTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "session_remaining",
		Description: "Seconds remaining before the pair session expires.",
		InputSchema: &mcpsdk.Schema{Type: "object"},
	}
}
func (t *SessionRemainingTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	return map[string]any{"seconds": int(t.sess.Remaining().Seconds())}, nil
}

type SessionEndTool struct{ sess *Session }

func NewSessionEndTool(s *Session) *SessionEndTool { return &SessionEndTool{sess: s} }
func (t *SessionEndTool) Name() string             { return "session_end" }
func (t *SessionEndTool) Schema() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "session_end",
		Description: "End the pair session voluntarily. Sidecar exits, tunnel closes.",
		InputSchema: &mcpsdk.Schema{Type: "object"},
	}
}
func (t *SessionEndTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	t.sess.End("ended_by_friend")
	return map[string]any{"ok": true}, nil
}
```

- [ ] **Step 4: Run — PASS**

- [ ] **Step 5: Commit**

```bash
git add go/cmd/ftw-pair/tool_session.go go/cmd/ftw-pair/tool_session_test.go
git commit -m "feat(pair): session_log + session_remaining + session_end tools"
```

---

## Task 16: Wormhole transport

**Files:**
- Create: `go/cmd/ftw-pair/wormhole.go`
- Create: `go/cmd/ftw-pair/wormhole_test.go`

Wraps `psanford/wormhole-william`'s file-transfer rendezvous to forward a TCP port. Library exposes `wormhole.Client` + `forward` package for port forwarding.

Read the upstream README in `vendor/github.com/psanford/wormhole-william/` after `go mod download` to confirm the exact API. Below is the expected shape:

- [ ] **Step 1: Failing test (uses real rendezvous if WORMHOLE_TEST=1, otherwise skips)**

`go/cmd/ftw-pair/wormhole_test.go`:

```go
package main

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

func TestWormholeForwardEndToEnd(t *testing.T) {
	if os.Getenv("WORMHOLE_TEST") == "" {
		t.Skip("set WORMHOLE_TEST=1 to run against real rendezvous (needs internet)")
	}

	// Stand up a local TCP echo server, wormhole-forward port 0 to it.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			c.Write([]byte("PONG\n"))
			c.Close()
		}
	}()
	defer ln.Close()

	host, hostErr := StartWormholeHost(context.Background(), ln.Addr().String())
	if hostErr != nil {
		t.Fatalf("host: %v", hostErr)
	}
	defer host.Close()

	t.Logf("wormhole code: %s", host.Code)

	// Connect from the same process via the corresponding client.
	connClient, err := ConnectWormholeClient(context.Background(), host.Code)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer connClient.Close()

	c, _ := net.DialTimeout("tcp", connClient.LocalAddr, 5*time.Second)
	defer c.Close()
	buf := make([]byte, 8)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := c.Read(buf)
	if string(buf[:n]) != "PONG\n" {
		t.Fatalf("unexpected reply: %q", buf[:n])
	}
}
```

- [ ] **Step 2: Implement (use the `forward` subpackage; consult the wormhole-william README for exact types)**

`go/cmd/ftw-pair/wormhole.go`:

```go
package main

// wormhole.go wraps the wormhole-william TCP-forward primitive.
//
// On the host side (StartWormholeHost) we expose a code that, when
// consumed by the matching ConnectWormholeClient, sets up a TCP
// tunnel where the client's local-listener forwards to the host's
// remoteAddr. The host's local MCP server bound on :9999 is that
// remoteAddr in production.
//
// We intentionally only expose what's used: code generation, forward,
// graceful close. If wormhole-william's API drifts, this is the only
// file that needs to change.

import (
	"context"
	"fmt"

	"github.com/psanford/wormhole-william/wormhole"
)

type WormholeHost struct {
	Code   string
	cancel context.CancelFunc
}

func (h *WormholeHost) Close() { h.cancel() }

func StartWormholeHost(ctx context.Context, remoteAddr string) (*WormholeHost, error) {
	cctx, cancel := context.WithCancel(ctx)
	c := wormhole.Client{}

	// wormhole-william exposes ForwardConnections; the API takes a
	// destination address and returns a generated code + an error
	// channel. Names below assume the upstream API; consult the
	// vendored README at first use.
	code, _, err := c.SendForward(cctx, remoteAddr, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("send forward: %w", err)
	}
	return &WormholeHost{Code: code, cancel: cancel}, nil
}

type WormholeClient struct {
	LocalAddr string
	cancel    context.CancelFunc
}

func (w *WormholeClient) Close() { w.cancel() }

func ConnectWormholeClient(ctx context.Context, code string) (*WormholeClient, error) {
	cctx, cancel := context.WithCancel(ctx)
	c := wormhole.Client{}
	localAddr, _, err := c.ReceiveForward(cctx, code, "127.0.0.1:0", nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("receive forward: %w", err)
	}
	return &WormholeClient{LocalAddr: localAddr, cancel: cancel}, nil
}
```

> **Note for implementer:** wormhole-william's exact API names (`SendForward`, `ReceiveForward`) are the expected shape based on the library at vendored time; if they differ, fix the wrapper to match — keep the function signatures of `StartWormholeHost` / `ConnectWormholeClient` stable so the rest of the codebase doesn't notice.

- [ ] **Step 3: Run with WORMHOLE_TEST=1 once locally to confirm**

```bash
WORMHOLE_TEST=1 cd go/cmd/ftw-pair && go test -run TestWormhole -v
```

Expected: PASS (or skipped without env var). If the API names differ, adjust wrapper and rerun.

- [ ] **Step 4: Commit**

```bash
git add go/cmd/ftw-pair/wormhole.go go/cmd/ftw-pair/wormhole_test.go
git commit -m "feat(pair): magic-wormhole transport wrapper (port forward)"
```

---

## Task 17: Wire the sidecar entry point

**Files:**
- Modify: `go/cmd/ftw-pair/main.go`

Replace the placeholder `main()` with the full lifecycle: parse flags, create session, build tool registry, start MCP server, start wormhole, print code, block until session done.

- [ ] **Step 1: Rewrite main.go**

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var Version = "dev"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	apiBase := flag.String("api", "http://localhost:8080", "URL of the running forty-two-watts service")
	repoDir := flag.String("repo", "/opt/forty-two-watts", "Path to the 42W repo / install dir")
	stateDir := flag.String("state", "/var/lib/forty-two-watts", "Path to the configured state dir")
	configPath := flag.String("config", "/etc/forty-two-watts/config.yaml", "Path to config.yaml")
	addr := flag.String("addr", "127.0.0.1:9999", "Local MCP server bind address")
	ttl := flag.Duration("ttl", 4*time.Hour, "Session TTL")
	intent := flag.String("intent", "", "Owner-stated purpose for this session")
	as := flag.String("as", "", "Optional friend identity (logged in audit)")
	flag.Parse()

	if *version {
		fmt.Printf("ftw-pair %s\n", Version)
		os.Exit(0)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sess := NewSession(ctx, SessionConfig{TTL: *ttl, Intent: *intent, As: *as})
	defer sess.End("sidecar_exit")
	audit := NewAudit()
	scope := NewScope(*repoDir, *stateDir)

	tools := []Tool{
		NewFtwAPITool(*apiBase),
		NewReadFileTool(scope),
		NewWriteFileTool(scope, audit),
		NewListDirectoryTool(scope),
		NewRunCommandTool(scope),
		NewRestartMainServiceTool(),
		NewTailServiceLogsTool(),
		NewNetworkScanTool(),
		NewHTTPProbeTool(),
		NewModbusProbeTool(),
		NewModbusWriteTool(),
		NewMQTTObserveTool(),
		NewPCapCaptureTool(),
		NewDeployDriverTool(scope, audit, *apiBase, *configPath),
		NewSessionLogTool(sess, audit),
		NewSessionRemainingTool(sess),
		NewSessionEndTool(sess),
	}

	mcpSrv, err := StartMCP(ctx, MCPConfig{
		Addr: *addr, Session: sess, Audit: audit, Tools: tools,
	})
	if err != nil {
		slog.Error("start mcp", "err", err)
		os.Exit(1)
	}
	defer mcpSrv.Shutdown(context.Background())

	host, err := StartWormholeHost(ctx, mcpSrv.Addr())
	if err != nil {
		slog.Error("wormhole host", "err", err)
		os.Exit(1)
	}
	defer host.Close()

	// Write the code to /var/run/forty-two-watts/pair.code so the main
	// service can pick it up + render in the UI session card. Also
	// print to stderr (owner sees it on the spawning terminal).
	fmt.Fprintf(os.Stderr, "PAIR CODE: %s\n", host.Code)
	fmt.Fprintf(os.Stderr, "TTL: %s — sidecar will exit at expiry\n", *ttl)

	// Register the code with the main service so the UI can show it.
	// Best effort — failure here doesn't break the session.
	if err := postPairStatus(*apiBase, host.Code, sess); err != nil {
		slog.Warn("post pair status", "err", err)
	}

	<-sess.Done()
	slog.Info("pair session ended", "reason", sess.ExitReason(), "tool_calls", audit.ToolCount())
}
```

- [ ] **Step 2: Add `postPairStatus` helper**

Extend the import block at the top of `main.go` to include:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)
```

Append to `main.go`:

```go
// postPairStatus tells the running 42W service about the current pair
// session so the dashboard's <ftw-pair-card> can render it.
// Best-effort — a failure here doesn't block the session.
func postPairStatus(apiBase, code string, sess *Session) error {
	body := map[string]any{
		"session_id": sess.ID,
		"code":       code,
		"intent":     sess.Intent(),
		"started_at": sess.StartedAt.UTC().Format(time.RFC3339),
		"ttl_s":      int(sess.Remaining().Seconds()),
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", apiBase+"/api/pair/status", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
```

- [ ] **Step 3: Build**

```bash
cd go && go build ./cmd/ftw-pair
```

Expected: clean build.

- [ ] **Step 4: Commit**

```bash
git add go/cmd/ftw-pair/main.go
git commit -m "feat(pair): wire ftw-pair entry point — session, MCP, wormhole, status post"
```

---

## Task 18: `forty-two-watts pair` subcommand + `/api/pair/*` endpoints

**Files:**
- Create: `go/cmd/forty-two-watts/pair.go` (subcommand handler)
- Modify: `go/cmd/forty-two-watts/main.go` (dispatch into `pair`)
- Create: `go/internal/api/api_pair.go`
- Create: `go/internal/api/api_pair_test.go`
- Modify: `go/internal/api/api.go` (register routes)

`forty-two-watts pair` spawns the `ftw-pair` binary as a child process. `forty-two-watts pair --abort` POSTs `/api/pair/abort`. `/api/pair/status` (GET + POST) shows / accepts the current session info so the UI can render it.

- [ ] **Step 1: Failing test for /api/pair/status round-trip**

`go/internal/api/api_pair_test.go`:

```go
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPairStatusPostThenGet(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store)

	body := `{"session_id":"abc","code":"7-x","intent":"goodwe","started_at":"2026-05-25T10:00:00Z","ttl_s":14400}`
	req := httptest.NewRequest("POST", "/api/pair/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("POST status: %d %s", w.Code, w.Body)
	}

	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, httptest.NewRequest("GET", "/api/pair/status", nil))
	if w2.Code != 200 {
		t.Fatalf("GET status: %d", w2.Code)
	}
	var got map[string]any
	json.Unmarshal(w2.Body.Bytes(), &got)
	if got["session_id"] != "abc" {
		t.Fatalf("expected echo: %v", got)
	}
}

func TestPairStatusGet404WhenNoSession(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/pair/status", nil))
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPairAbortClearsStatus(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store)
	store.Set(PairStatus{SessionID: "abc", Code: "7-x"})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/pair/abort", bytes.NewReader(nil)))
	if w.Code != 200 {
		t.Fatalf("abort: %d", w.Code)
	}
	if _, ok := store.Get(); ok {
		t.Fatal("status not cleared")
	}
}
```

- [ ] **Step 2: Implement**

`go/internal/api/api_pair.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"sync"
)

type PairStatus struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
	Intent    string `json:"intent"`
	StartedAt string `json:"started_at"`
	TTLS      int    `json:"ttl_s"`
	// Tool counts are pushed by the sidecar via subsequent POSTs.
	ToolCount int      `json:"tool_count,omitempty"`
	LastTools []string `json:"last_tools,omitempty"`
}

type PairStatusStore struct {
	mu sync.Mutex
	cur *PairStatus
}

func NewPairStatusStore() *PairStatusStore { return &PairStatusStore{} }

func (s *PairStatusStore) Set(p PairStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = &p
}

func (s *PairStatusStore) Get() (PairStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return PairStatus{}, false
	}
	return *s.cur, true
}

func (s *PairStatusStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = nil
}

func RegisterPairRoutes(mux *http.ServeMux, store *PairStatusStore) {
	mux.HandleFunc("GET /api/pair/status", func(w http.ResponseWriter, r *http.Request) {
		p, ok := store.Get()
		if !ok {
			http.Error(w, `{"error":"no active session"}`, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("POST /api/pair/status", func(w http.ResponseWriter, r *http.Request) {
		var p PairStatus
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		store.Set(p)
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /api/pair/abort", func(w http.ResponseWriter, r *http.Request) {
		// We don't actually kill the sidecar from here; the sidecar
		// must periodically GET /api/pair/abort-requested to find out.
		// Simpler than wiring a channel through several layers.
		store.Clear()
		w.WriteHeader(200)
	})
}
```

- [ ] **Step 3: Hook into main api setup**

In `go/internal/api/api.go`, wherever routes are registered (search for "ServeMux" or "RegisterRoutes"), add:

```go
pairStore := NewPairStatusStore()
RegisterPairRoutes(mux, pairStore)
```

If `api.go` is wired via a struct, expose the store on that struct so the rest of the codebase can read it (the UI SSE feed will need it in Task 21).

- [ ] **Step 4: Implement the subcommand**

`go/cmd/forty-two-watts/pair.go`:

```go
package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// runPair is the entry point for `forty-two-watts pair`.
//
//   forty-two-watts pair                       # 4h session
//   forty-two-watts pair --ttl 2h
//   forty-two-watts pair --intent "..." --as "@erikarenhill"
//   forty-two-watts pair --abort               # signal the running sidecar to exit
func runPair(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	ttl := fs.String("ttl", "4h", "Session TTL (Go duration)")
	intent := fs.String("intent", "", "Free-form description of what the friend should help with")
	as := fs.String("as", "", "Optional friend identity")
	abort := fs.Bool("abort", false, "Abort the active session and exit")
	bin := fs.String("bin", "", "Path to ftw-pair binary (default: sibling of forty-two-watts)")
	_ = fs.Parse(args)

	if *abort {
		resp, err := http.Post("http://127.0.0.1:8080/api/pair/abort", "application/json", bytes.NewReader(nil))
		if err != nil {
			fmt.Fprintf(os.Stderr, "abort: %v\n", err)
			os.Exit(1)
		}
		resp.Body.Close()
		fmt.Println("abort signaled — sidecar will exit on its next poll")
		return
	}

	pairBin := *bin
	if pairBin == "" {
		self, _ := os.Executable()
		pairBin = filepath.Join(filepath.Dir(self), "ftw-pair")
	}
	if _, err := os.Stat(pairBin); err != nil {
		fmt.Fprintf(os.Stderr, "ftw-pair binary not found at %s\n", pairBin)
		os.Exit(1)
	}

	cmdArgs := []string{
		"-ttl", *ttl,
		"-intent", *intent,
		"-as", *as,
	}
	cmd := exec.Command(pairBin, cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Dispatch from main.go**

In `go/cmd/forty-two-watts/main.go`, extend the subcommand switch at line ~63:

```go
switch os.Args[1] {
case "nova-claim":
    runNovaClaim(os.Args[2:])
    return
case "pair":
    runPair(os.Args[2:])
    return
}
```

- [ ] **Step 6: Add an abort-poller in the sidecar**

The sidecar needs to discover that `POST /api/pair/abort` was called. Append to `go/cmd/ftw-pair/main.go`, just before `<-sess.Done()`:

```go
go func() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Done():
			return
		case <-t.C:
			resp, err := http.Get(*apiBase + "/api/pair/status")
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == 404 {
				sess.End("aborted_by_owner")
				return
			}
		}
	}
}()
```

- [ ] **Step 7: Run all tests**

```bash
cd go && go test ./...
```

- [ ] **Step 8: Commit**

```bash
git add go/cmd/forty-two-watts/pair.go go/cmd/forty-two-watts/main.go \
        go/internal/api/api_pair.go go/internal/api/api_pair_test.go go/internal/api/api.go \
        go/cmd/ftw-pair/main.go
git commit -m "feat(pair): pair subcommand + /api/pair/* endpoints + sidecar abort polling"
```

---

## Task 19: `ftw-connect` friend CLI

**Files:**
- Modify: `go/cmd/ftw-connect/main.go`
- Create: `go/cmd/ftw-connect/clipboard.go`
- Create: `go/cmd/ftw-connect/main_test.go` (replacing the scaffold)

- [ ] **Step 1: Failing test for the prompt builder**

`go/cmd/ftw-connect/main_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	p := buildPrompt("write a goodwe driver", "3h45m")
	if !strings.Contains(p, "goodwe") {
		t.Fatal("intent missing")
	}
	if !strings.Contains(p, "3h45m") {
		t.Fatal("ttl missing")
	}
	if !strings.Contains(p, "ftw-remote") {
		t.Fatal("server name missing")
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement main + prompt builder**

Replace `go/cmd/ftw-connect/main.go`:

```go
// ftw-connect is the friend-side CLI that turns a magic-wormhole code
// into a live MCP endpoint Claude Code can talk to.
//
// Usage:
//   ftw-connect 7-crossover-clockwork
//
// On success it:
//   1. opens the wormhole tunnel,
//   2. forwards a local TCP port to the host's MCP server,
//   3. registers that port with Claude Code (`claude mcp add ...`),
//   4. copies a context prompt to the clipboard, and
//   5. blocks until the tunnel closes (TTL, owner abort, ^C).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/psanford/wormhole-william/wormhole"
)

var Version = "dev"

const mcpName = "ftw-remote"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Printf("ftw-connect %s\n", Version)
		os.Exit(0)
	}
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: ftw-connect <wormhole-code>")
		os.Exit(2)
	}
	code := flag.Arg(0)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("Connecting to %s...\n", code)
	client := wormhole.Client{}
	localAddr, _, err := client.ReceiveForward(ctx, code, "127.0.0.1:0", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wormhole connect: %v\n", err)
		os.Exit(1)
	}
	mcpURL := "http://" + localAddr + "/mcp"
	fmt.Printf("Tunnel ready: %s\n", mcpURL)

	// Best-effort registration with Claude Code.
	if err := exec.Command("claude", "mcp", "add", mcpName, mcpURL, "--transport", "http").Run(); err != nil {
		fmt.Fprintf(os.Stderr, "claude mcp add failed: %v — register manually with:\n  claude mcp add %s %s\n", err, mcpName, mcpURL)
	} else {
		fmt.Printf("Registered MCP server '%s' with Claude Code.\n", mcpName)
	}
	defer func() {
		_ = exec.Command("claude", "mcp", "remove", mcpName).Run()
	}()

	// Fetch the owner's intent + remaining TTL from the host.
	intent, ttl := fetchPairMeta(localAddr)
	prompt := buildPrompt(intent, ttl)
	if err := copyClipboard(prompt); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard copy failed: %v — paste manually:\n\n%s\n", err, prompt)
	} else {
		fmt.Printf("Context prompt copied to clipboard. Paste it into Claude Code now.\n")
	}

	fmt.Println("Tunnel open. Ctrl-C to disconnect.")
	<-ctx.Done()
}

func fetchPairMeta(addr string) (intent, ttl string) {
	// Read /api/pair/status (proxied through ftw_api on the host).
	// On error fall back to placeholders — the prompt is still useful.
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/mcp/ftw-api?path=/api/pair/status&method=GET")
	if err != nil {
		return "(unspecified)", "(unknown)"
	}
	defer resp.Body.Close()
	// MCP responses are JSON-RPC framed; for simplicity ftw-connect
	// asks Claude (via the prompt) to call ftw_api itself. So we just
	// return placeholders here.
	return "(see owner's message)", "(see session_remaining)"
}

func buildPrompt(intent, ttl string) string {
	var b strings.Builder
	b.WriteString("You are connected to a forty-two-watts instance over the MCP server `")
	b.WriteString(mcpName)
	b.WriteString("`. The owner wants you to help with:\n\n> ")
	b.WriteString(intent)
	b.WriteString("\n\nStart with `ftw_api` GET `/api/status` to see the current state. The full HTTP API is documented in the repo at `docs/api.md` — `read_file` it if you need.\n\nEverything you do is recorded and will accompany the PR you open. When done, call `session_end` and report back to the owner.\n\nSession timeout: ")
	b.WriteString(ttl)
	b.WriteString(". If you need more time, the owner can re-pair.\n")
	return b.String()
}
```

- [ ] **Step 4: Clipboard helper (darwin/linux)**

`go/cmd/ftw-connect/clipboard.go`:

```go
package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func copyClipboard(s string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		// xclip first, fall back to wl-copy
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else {
			return fmt.Errorf("install xclip or wl-clipboard to enable clipboard copy")
		}
	default:
		return fmt.Errorf("unsupported os: %s", runtime.GOOS)
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}
```

- [ ] **Step 5: Run**

```bash
cd go && go test ./cmd/ftw-connect/
```

- [ ] **Step 6: Commit**

```bash
git add go/cmd/ftw-connect/
git commit -m "feat(pair): ftw-connect friend CLI — wormhole + claude mcp add + clipboard"
```

---

## Task 20: Web UI session card

**Files:**
- Create: `web/components/ftw-pair-card.js`
- Modify: `web/components/index.html` or wherever cards mount (search for `<ftw-update-check>` to find the right spot)

A card that polls `/api/pair/status` every 5s. Hidden when 404. Shows code, intent, TTL countdown, tool count, last 5 tools, abort button.

- [ ] **Step 1: Implement component**

`web/components/ftw-pair-card.js`:

```js
// <ftw-pair-card> — surfaces an active pair session on the dashboard.
//
// Polls /api/pair/status every 5 s. Renders the wormhole code, the
// owner-supplied intent, a TTL countdown, the running tool counter,
// and an Abort button that POSTs /api/pair/abort.
//
// Tokens: surface, line, fg, mono, accent-e (see web/components/theme.css)

import { FtwElement } from "./ftw-element.js";

const POLL_MS = 5000;

class FtwPairCard extends FtwElement {
  constructor() {
    super();
    this.state = null;
    this.tick = null;
  }
  connectedCallback() {
    super.connectedCallback?.();
    this.refresh();
    this.tick = setInterval(() => this.refresh(), POLL_MS);
  }
  disconnectedCallback() {
    super.disconnectedCallback?.();
    if (this.tick) clearInterval(this.tick);
  }
  async refresh() {
    try {
      const r = await fetch("/api/pair/status");
      if (r.status === 404) {
        this.state = null;
      } else if (r.ok) {
        this.state = await r.json();
      }
    } catch (_) {}
    this.render();
  }
  async abort() {
    if (!confirm("End the pair session now?")) return;
    await fetch("/api/pair/abort", { method: "POST" });
    this.state = null;
    this.render();
  }
  render() {
    if (!this.state) {
      this.innerHTML = "";
      return;
    }
    const remaining = this.computeRemaining();
    this.innerHTML = `
      <section class="pair-card">
        <header>
          <span class="eyebrow">PAIR SESSION ACTIVE</span>
          <button class="abort" onclick="this.getRootNode().host.abort()">Abort</button>
        </header>
        <div class="code">${this.state.code}</div>
        <p class="intent">${this.escape(this.state.intent || "(no intent set)")}</p>
        <dl>
          <dt>TTL</dt><dd>${remaining}</dd>
          <dt>Tool calls</dt><dd>${this.state.tool_count ?? 0}</dd>
          <dt>Last tools</dt><dd>${(this.state.last_tools || []).join(", ") || "—"}</dd>
        </dl>
      </section>
      <style>
        .pair-card { border: 1px solid var(--line); padding: 16px; }
        .eyebrow { font-family: var(--mono); letter-spacing: 0.18em; font-size: 11px; color: var(--ink-raised); }
        .code { font-family: var(--mono); font-size: 18px; margin: 8px 0; }
        .intent { color: var(--fg); }
        dl { display: grid; grid-template-columns: max-content 1fr; gap: 4px 16px; font-family: var(--mono); font-size: 12px; }
        dt { color: var(--ink-raised); }
        button.abort { float: right; background: var(--accent-e); color: #0a0a0a; border: 0; padding: 4px 10px; font-family: var(--mono); cursor: pointer; }
      </style>
    `;
  }
  computeRemaining() {
    if (!this.state.started_at || !this.state.ttl_s) return "—";
    const startedMs = Date.parse(this.state.started_at);
    const expiry = startedMs + this.state.ttl_s * 1000;
    const left = Math.max(0, Math.floor((expiry - Date.now()) / 1000));
    const h = Math.floor(left / 3600);
    const m = Math.floor((left % 3600) / 60);
    return `${h}h ${m}m`;
  }
  escape(s) {
    return String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  }
}
customElements.define("ftw-pair-card", FtwPairCard);
```

- [ ] **Step 2: Mount the card**

Find the dashboard mount point — likely `web/index.html` and/or `web/next-app.js`. Search for existing component usage to learn the pattern:

```bash
grep -RnE "ftw-update-check|ftw-card" web/*.html web/*.js | head
```

Insert `<ftw-pair-card></ftw-pair-card>` near the top of the dashboard so it's prominent when a session is active (it self-hides otherwise). Match the surrounding HTML / JS pattern.

- [ ] **Step 3: Smoke test**

```bash
make dev
# In another terminal:
curl -X POST -H "Content-Type: application/json" -d '{"session_id":"t","code":"7-test","intent":"smoke","started_at":"2026-05-25T10:00:00Z","ttl_s":14400,"tool_count":2,"last_tools":["ftw_api","read_file"]}' http://localhost:8080/api/pair/status
# Open http://localhost:8080 — the card should appear.
curl -X POST http://localhost:8080/api/pair/abort
# Card should disappear within 5s.
```

- [ ] **Step 4: Commit**

```bash
git add web/components/ftw-pair-card.js web/index.html web/next-app.js
git commit -m "feat(pair): <ftw-pair-card> dashboard component + mount"
```

---

## Task 21: Sidecar pushes tool-count + last-tools updates

**Files:**
- Modify: `go/cmd/ftw-pair/mcp.go`
- Modify: `go/cmd/ftw-pair/main.go`

The card needs live `tool_count` + `last_tools`. Cheapest wiring: every N tool calls (or every 10s), the sidecar re-POSTs to `/api/pair/status` with the latest counters.

- [ ] **Step 1: Add a heartbeat goroutine to main.go**

In `go/cmd/ftw-pair/main.go`, after the abort poller, add a separate goroutine:

```go
go func() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Done():
			return
		case <-t.C:
			_ = postPairStatusFull(*apiBase, host.Code, sess, audit)
		}
	}
}()
```

- [ ] **Step 2: Add the helper**

```go
func postPairStatusFull(apiBase, code string, sess *Session, audit *Audit) error {
	body := map[string]any{
		"session_id": sess.ID,
		"code":       code,
		"intent":     sess.Intent(),
		"started_at": sess.StartedAt.UTC().Format(time.RFC3339),
		"ttl_s":      int(sess.Remaining().Seconds()),
		"tool_count": audit.ToolCount(),
		"last_tools": audit.LastTools(5),
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", apiBase+"/api/pair/status", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
```

- [ ] **Step 3: Build + test**

```bash
cd go && go build ./cmd/ftw-pair && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add go/cmd/ftw-pair/main.go
git commit -m "feat(pair): sidecar heartbeats tool_count + last_tools to /api/pair/status"
```

---

## Task 22: PR template

**Files:**
- Create: `.github/PULL_REQUEST_TEMPLATE/pair-session.md`
- Modify: `.github/PULL_REQUEST_TEMPLATE.md` (if exists) to link the new template

- [ ] **Step 1: Add template**

`.github/PULL_REQUEST_TEMPLATE/pair-session.md`:

```markdown
<!-- PR template for changes produced during a pair session.
     Use this template by appending ?template=pair-session.md to the
     compare URL when opening the PR. -->

## Summary

<!-- One paragraph: what changed, why. -->

## Pair-session report

<!-- Paste here the output of `session_log()` from the pair session.
     This is mandatory for PRs from pair sessions — reviewers use it
     to understand what the friend's Claude Code actually did. -->

```

## Test plan

- [ ] Driver / change runs against real hardware during the pair session (proof in the session log above).
- [ ] Unit tests for any new Go code.
- [ ] No new lint warnings.
```

- [ ] **Step 2: Commit**

```bash
git add .github/PULL_REQUEST_TEMPLATE/pair-session.md
git commit -m "docs(pair): PR template for pair-session-produced changes"
```

---

## Task 23: Docs

**Files:**
- Create: `docs/ftw-pair.md`
- Modify: `CLAUDE.md` (one-line pointer in the "Key packages" section)

`docs/ftw-pair.md` walks an operator through: pairing, sharing the code, what the friend sees, aborting, and the PR flow. Write it as a runbook, not as design — design lives in the spec.

- [ ] **Step 1: Write docs/ftw-pair.md**

```markdown
# `ftw-pair` — handing over a 42W instance temporarily

`ftw-pair` lets you grant time-bound MCP access to a running
forty-two-watts instance over a magic-wormhole tunnel. The recipient's
Claude Code gets a curated tool surface for driver development, model
tuning, or live debugging.

## When to use it

- A friend with deeper experience offers to help you write a driver.
- You're away from the instance but want to iterate against it from
  Claude Code on a laptop somewhere else.
- An incident needs eyes-on access from someone other than the operator.

The trust level is "ssh-equivalent for the session duration". Only
pair with people you'd already give shell to.

## On the host

```bash
forty-two-watts pair --intent "help me write a goodwe XS driver" --ttl 4h
```

You'll see something like:

```
PAIR CODE: 7-crossover-clockwork
TTL: 4h0m0s — sidecar will exit at expiry
```

Send the code to the friend over any channel — Signal, SMS, Slack DM.

To end the session early:

```bash
forty-two-watts pair --abort
```

…or click **Abort** on the pair-session card in the web UI.

## On the friend

One-time install:

```bash
go install github.com/frahlg/forty-two-watts/go/cmd/ftw-connect@latest
```

Per session:

```bash
ftw-connect 7-crossover-clockwork
```

That:

1. Opens the wormhole tunnel.
2. Registers an MCP server named `ftw-remote` with Claude Code.
3. Copies a context-priming prompt to the clipboard.

Open Claude Code, paste the prompt, work. When done, the friend calls
`session_end` and opens a PR using the `pair-session.md` template.

## What the friend gets

A 17-tool MCP surface:

- `ftw_api(method, path, body)` — full 42W HTTP API
- `read_file` / `write_file` / `list_directory` — repo, state dir, /tmp
- `run_command` — shell, same scope
- `restart_main_service` / `tail_service_logs` — systemd, journalctl
- `network_scan` / `http_probe` / `modbus_probe` / `modbus_write` / `mqtt_observe` / `pcap_capture` — LAN-level introspection
- `deploy_driver(name, lua, config)` — write Lua + edit config + reload + verify
- `session_log` / `session_remaining` / `session_end`

## What gets recorded

Every tool call lands in an audit log. `session_log()` renders the log
as markdown for the PR template. Friends should paste it into the PR
body; reviewers use it to confirm what changed on your instance.

## Limits

- One session at a time.
- 4 h default TTL, configurable, hard kill at expiry.
- Wormhole rendezvous is the upstream public relay
  (`relay.magic-wormhole.io`). Override with `--rendezvous` if you run
  your own.
- No per-call approval. Pairing = full trust for the session.
```

- [ ] **Step 2: Pointer in CLAUDE.md**

Insert in the "Key packages" table in `CLAUDE.md`:

```markdown
| `go/cmd/ftw-pair` | MCP sidecar — host side of the pair flow (`docs/ftw-pair.md`) |
| `go/cmd/ftw-connect` | Friend-side CLI for joining a pair session |
```

- [ ] **Step 3: Commit**

```bash
git add docs/ftw-pair.md CLAUDE.md
git commit -m "docs(pair): operator runbook for ftw-pair + CLAUDE.md pointers"
```

---

## Task 24: End-to-end smoke test

**Files:**
- Create: `go/test/e2e/pair_test.go`

Spin up the bundled sims + main service, then run `ftw-pair` in-process (skip wormhole — call MCP directly over `localhost:9999`). Drive it through a `ftw_api GET /api/status` → `deploy_driver` → `session_log` flow, assert the resulting markdown contains the deployed driver name.

- [ ] **Step 1: Write the test**

`go/test/e2e/pair_test.go`:

```go
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPairFlow verifies that ftw-pair, run as a child process alongside
// a live main service, exposes a working MCP endpoint and that the
// session report eventually contains the tool calls we made.
//
// The wormhole hop is skipped — we talk directly to the sidecar's
// localhost MCP listener. wormhole-william is exercised by its own
// test gated on WORMHOLE_TEST=1 (see go/cmd/ftw-pair/wormhole_test.go).
func TestPairFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}

	repo := repoRoot(t)
	pairBin := buildBinary(t, repo, "ftw-pair")
	mainBin := buildBinary(t, repo, "forty-two-watts")

	// Start sims + main service on a temp config / state dir.
	work := t.TempDir()
	stateDir := filepath.Join(work, "state")
	_ = os.MkdirAll(stateDir, 0o755)
	cfgPath := writeMinimalConfig(t, work, stateDir)

	mainCmd := exec.Command(mainBin,
		"-config", cfgPath,
		"-web", filepath.Join(repo, "web"),
	)
	mainCmd.Stdout = os.Stdout
	mainCmd.Stderr = os.Stderr
	if err := mainCmd.Start(); err != nil {
		t.Fatalf("start main: %v", err)
	}
	defer mainCmd.Process.Kill()
	waitForAPI(t, "http://127.0.0.1:8080/api/status")

	// Start the sidecar. -addr :0 not supported — pick a high port.
	pairCmd := exec.Command(pairBin,
		"-addr", "127.0.0.1:19999",
		"-api", "http://127.0.0.1:8080",
		"-repo", repo,
		"-state", stateDir,
		"-config", cfgPath,
		"-ttl", "1m",
		"-intent", "e2e smoke",
	)
	pairCmd.Stdout = os.Stdout
	pairCmd.Stderr = os.Stderr
	if err := pairCmd.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	defer pairCmd.Process.Kill()
	waitForAPI(t, "http://127.0.0.1:19999/healthz")

	// Drive an MCP tools/call for ftw_api → /api/status via JSON-RPC over /mcp.
	resp := callMCP(t, "http://127.0.0.1:19999/mcp", "ftw_api", map[string]any{
		"method": "GET", "path": "/api/status",
	})
	if !strings.Contains(resp, "mode") {
		t.Fatalf("expected /api/status body in ftw_api response, got: %s", resp)
	}

	// Render session_log and confirm it captured ftw_api.
	log := callMCP(t, "http://127.0.0.1:19999/mcp", "session_log", map[string]any{})
	if !strings.Contains(log, "ftw_api") {
		t.Fatalf("session_log missing ftw_api entry:\n%s", log)
	}
	if !strings.Contains(log, "e2e smoke") {
		t.Fatalf("session_log missing intent:\n%s", log)
	}
}

func buildBinary(t *testing.T, repo, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/"+name)
	cmd.Dir = filepath.Join(repo, "go")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return dst
}

func waitForAPI(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("waitForAPI %s: timed out", url)
}

func callMCP(t *testing.T, url, tool string, args map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": args,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

func writeMinimalConfig(t *testing.T, dir, stateDir string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	contents := fmt.Sprintf(`site:
  name: e2e
state:
  path: %s/state.db
  cold_dir: %s/cold
drivers: []
`, stateDir, stateDir)
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}
```

- [ ] **Step 2: Run**

```bash
make e2e
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add go/test/e2e/pair_test.go
git commit -m "test(pair): e2e smoke — sims + main + ftw-pair MCP flow (wormhole skipped)"
```

---

## Task 25: Release wiring

**Files:**
- Modify: `Makefile`
- Modify: `.github/workflows/*.yml` (whatever builds release tarballs)

The release tarball must ship `ftw-pair` alongside `forty-two-watts`. `ftw-connect` is published via `go install` so the friend installs it themselves — it isn't bundled.

- [ ] **Step 1: Inspect Makefile build/release targets**

```bash
grep -nE "build|release|tarball" Makefile | head -20
```

- [ ] **Step 2: Add ftw-pair to the build matrix**

Wherever `forty-two-watts` is `go build`'d (likely a `build-arm64` target and an `e2e`-supporting `build` target), add an analogous `go build ./cmd/ftw-pair` line and include the resulting binary in the release tarball.

- [ ] **Step 3: Verify the tarball**

```bash
make release
tar -tzf dist/*.tar.gz | grep -E "ftw-pair|forty-two-watts"
```

Expected: both binaries listed.

- [ ] **Step 4: Commit**

```bash
git add Makefile .github/workflows/
git commit -m "build(pair): ship ftw-pair binary in release tarball"
```

---

## Wrap-up

After all tasks land:

1. Open PR for `spec/ftw-pair-mcp-remote` → `master` using the `/pr` skill.
2. Smoke-test on a dev Pi: `forty-two-watts pair --intent "smoke test"`, share the code with a second laptop, run `ftw-connect`, drive a `ftw_api` call from the friend's Claude Code, verify it lands.
3. Tear down: `session_end` from friend side, confirm sidecar exits and `/api/pair/status` returns 404.

## Spec coverage check

| Spec section | Implemented in tasks |
|---|---|
| Sidecar architecture | 1, 17 |
| Friend CLI | 19 |
| 17-tool MCP surface | 5–15 |
| Path scoping | 4 |
| Session lifecycle (TTL, abort, single-active) | 2, 18 |
| In-UI session visibility | 18, 20, 21 |
| Test report (`session_log`) | 3, 15 |
| Friend-side onboarding (clipboard prompt) | 19 |
| `forty-two-watts pair` subcommand | 18 |
| Wormhole transport | 16 |
| `psanford/wormhole-william` dependency | 1 |
| MCP SDK dependency | 1 |
| PR template | 22 |
| Docs (runbook) | 23 |
| Release packaging | 25 |
| e2e smoke | 24 |
