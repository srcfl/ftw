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
