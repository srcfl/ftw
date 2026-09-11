package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/srcfl/ftw/go/internal/assistant"
	"github.com/srcfl/ftw/go/internal/config"
)

// githubNewIssueURL is a markdown template with one body field. The YAML
// bug form is not used: it asks the operator to fill steps the diagnosis
// already wrote.
const githubNewIssueURL = "https://github.com/srcfl/ftw/issues/new?template=ask_why.md"
const githubIssueURLBudget = 7000

type assistantAskRequest struct {
	Question string            `json:"question"`
	Trigger  *assistantTrigger `json:"trigger,omitempty"`
	History  []assistant.Turn  `json:"history,omitempty"`
	// ThreadID continues a stored conversation. Empty starts a new one.
	ThreadID string `json:"thread_id,omitempty"`
}

type assistantTrigger struct {
	Kind   string `json:"kind"`
	Driver string `json:"driver,omitempty"`
}

type assistantAskResponse struct {
	Answer        string `json:"answer"`
	IssueTitle    string `json:"issue_title,omitempty"`
	IssueBody     string `json:"issue_body,omitempty"`
	IssueURL      string `json:"issue_url,omitempty"`
	Model         string `json:"model"`
	ResolvedModel string `json:"resolved_model,omitempty"`
	ThreadID      string `json:"thread_id,omitempty"`
}

type assistantStatusResponse struct {
	Enabled     bool   `json:"enabled"`
	Configured  bool   `json:"configured"`
	Ready       bool   `json:"ready"`
	Model       string `json:"model"`
	BaseURLHost string `json:"base_url_host,omitempty"`
	SetupURL    string `json:"setup_url,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
}

func (s *Server) assistantSnapshot() config.Assistant {
	if s.deps.Cfg == nil {
		return config.Assistant{}
	}
	if s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		defer s.deps.CfgMu.RUnlock()
	}
	if s.deps.Cfg.Assistant == nil {
		return config.Assistant{}
	}
	return *s.deps.Cfg.Assistant
}

func (s *Server) handleAssistantStatus(w http.ResponseWriter, r *http.Request) {
	asst := s.assistantSnapshot()
	host := ""
	if u, err := url.Parse(asst.ResolvedBaseURL()); err == nil {
		host = u.Host
	}
	out := assistantStatusResponse{
		Enabled:     asst.Enabled,
		Configured:  strings.TrimSpace(asst.APIKey) != "",
		Ready:       asst.Ready(),
		Model:       asst.ResolvedModel(),
		BaseURLHost: host,
		SetupURL:    "https://openrouter.ai/keys",
	}
	if !asst.Enabled && strings.TrimSpace(asst.APIKey) == "" {
		out.Unavailable = "Paste an OpenRouter key in Settings → System. That turns Ask why on."
	} else if !asst.Enabled {
		out.Unavailable = "Turn on Ask why in Settings → System."
	} else if strings.TrimSpace(asst.APIKey) == "" {
		out.Unavailable = "Paste an OpenRouter API key in Settings → System. A free key is enough."
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAssistantAsk(w http.ResponseWriter, r *http.Request) {
	if !s.assistantAskMu.TryLock() {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Ask why is already running"})
		return
	}
	defer s.assistantAskMu.Unlock()

	var body assistantAskRequest
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if utf8.RuneCountInString(body.Question) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "question is too long"})
		return
	}

	asst := s.assistantSnapshot()
	if !asst.Ready() {
		msg := "Ask why is off. Turn it on in Settings → System and paste an OpenRouter key."
		if asst.Enabled && strings.TrimSpace(asst.APIKey) == "" {
			msg = "Ask why needs an OpenRouter API key in Settings → System."
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
		return
	}

	if s.deps.Ctrl == nil || s.deps.Tel == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "site state is not available"})
		return
	}

	stream := strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	var flush http.Flusher
	if stream {
		var ok bool
		flush, ok = w.(http.Flusher)
		if !ok {
			stream = false
		}
	}
	progress := func(kind, text string) {
		if !stream {
			return
		}
		writeAssistantSSE(w, flush, map[string]string{"type": kind, "text": text})
	}

	start := time.Now()
	progress("status", "Reading the site")
	facts := s.assistantFacts()
	// Complete emits "Asking the model" once per round. Saying it here
	// too would double every line in the activity log.
	cli := &assistant.Client{HTTP: s.deps.AssistantHTTP}
	reply, err := cli.Complete(r.Context(), assistant.Request{
		APIKey:   asst.APIKey,
		Model:    asst.ResolvedModel(),
		BaseURL:  asst.ResolvedBaseURL(),
		Question: body.Question,
		Trigger:  formatAssistantTrigger(body.Trigger),
		History:  body.History,
		Snapshot: facts,
		Run:      s.runAssistantTool,
		Progress: progress,
	})
	if err != nil {
		var apiErr *assistant.APIError
		if errors.As(err, &apiErr) {
			slog.Warn("assistant ask failed", "status", apiErr.Status, "err", apiErr.Msg)
			if stream {
				writeAssistantSSE(w, flush, map[string]string{"type": "error", "error": apiErr.Msg})
				return
			}
			writeJSON(w, apiErr.Status, map[string]string{"error": apiErr.Msg})
			return
		}
		slog.Warn("assistant ask failed", "err", err)
		if stream {
			writeAssistantSSE(w, flush, map[string]string{"type": "error", "error": "could not reach the model API"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not reach the model API"})
		return
	}

	out := assistantAskResponse{
		Answer:        reply.Answer,
		IssueTitle:    reply.IssueTitle,
		IssueBody:     reply.IssueBody,
		Model:         reply.Model,
		ResolvedModel: reply.ResolvedModel,
	}
	if out.IssueTitle != "" && out.IssueBody == "" {
		out.IssueBody = out.Answer
	}
	if out.IssueTitle != "" {
		out.IssueURL = filledIssueURL(out.IssueTitle, out.IssueBody)
	}
	out.ThreadID = s.recordAssistantTurn(body, out.Answer, out.ResolvedModel)
	slog.Info("assistant ask",
		"model", out.ResolvedModel,
		"ms", time.Since(start).Milliseconds(),
		"tools", reply.ToolRounds,
		"issue", out.IssueTitle != "")
	if stream {
		writeAssistantSSE(w, flush, map[string]any{
			"type":           "done",
			"answer":         out.Answer,
			"issue_title":    out.IssueTitle,
			"issue_body":     out.IssueBody,
			"issue_url":      out.IssueURL,
			"model":          out.Model,
			"resolved_model": out.ResolvedModel,
			"thread_id":      out.ThreadID,
		})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func filledIssueURL(title, body string) string {
	if utf8.RuneCountInString(body) > 4000 {
		body = string([]rune(body)[:4000]) + "\n\n[truncated]"
	}
	u := githubNewIssueURL + "&title=" + url.QueryEscape(title) + "&body=" + url.QueryEscape(body)
	if len(u) <= githubIssueURLBudget {
		return u
	}
	return githubNewIssueURL + "&title=" + url.QueryEscape(title)
}

func writeAssistantSSE(w http.ResponseWriter, flush http.Flusher, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(raw)
	_, _ = w.Write([]byte("\n\n"))
	if flush != nil {
		flush.Flush()
	}
}

func (s *Server) assistantFacts() string {
	var b strings.Builder
	v := strings.TrimSpace(s.deps.Version)
	if v == "" {
		v = "dev"
	}
	b.WriteString("FTW version ")
	b.WriteString(v)
	b.WriteString("\n\n")
	b.WriteString(s.toolDriverHealth(nil))
	b.WriteString("\n\n")
	b.WriteString(s.toolPlanNow())
	b.WriteString("\n\n")
	b.WriteString(s.toolRecentLogs([]byte(`{"limit":20}`)))
	return assistant.Redact(b.String())
}

func formatAssistantTrigger(t *assistantTrigger) string {
	if t == nil {
		return ""
	}
	kind := strings.TrimSpace(t.Kind)
	driver := sanitizeDriverName(t.Driver)
	if kind == "plan" {
		return "the operator is asking why the current plan looks like this"
	}
	if kind == "driver_offline" && driver != "" {
		return "driver " + driver + " is offline"
	}
	if kind == "driver_offline" {
		return "a driver is offline"
	}
	return ""
}

func sanitizeDriverName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 40 {
		return ""
	}
	for _, r := range name {
		if r == '_' || r == '-' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return ""
	}
	return name
}
