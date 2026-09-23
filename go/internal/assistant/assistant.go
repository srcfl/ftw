// Package assistant calls an OpenAI-compatible chat API (OpenRouter by
// default) to explain a local FTW site. It never issues commands.
package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultModel   = "openrouter/free"
	DefaultBaseURL = "https://openrouter.ai/api/v1"
	// Timeout covers a slow free-tier completion, including a few tool rounds.
	Timeout = 90 * time.Second
	// maxReportRunes keeps a stuffed one-shot inside a free-model context.
	maxReportRunes  = 80_000
	maxQuestion     = 2000
	maxTokens       = 2500
	maxIssueTitle   = 80
	maxHistoryTurns = 6
	maxHistoryRunes = 1500
)

// Request is one Ask why turn.
type Request struct {
	APIKey   string
	Model    string
	BaseURL  string
	Question string
	// Trigger is a short fact from the UI, e.g. "driver sungrow is offline".
	Trigger string
	// Report is only used when Run is nil (tests, or a model without tools).
	Report string
	// Snapshot is a local facts pack gathered before the model runs.
	Snapshot string
	// History is earlier turns in this dialog, oldest first. Follow-ups
	// need it; the current question is not included.
	History []Turn
	// Run executes read-only tools. Nil means no tool loop: the report is
	// stuffed into the first user message, matching the original one-shot.
	Run Runner
	// Progress is optional live status for the UI. kind is "status",
	// "tool", "delta" (a token of the answer), or "round" (a new model
	// round starts, so deltas so far were not the answer).
	Progress func(kind, text string)
}

// Reply is what the UI shows. Issue fields are empty when the model does
// not think this is a bug.
type Reply struct {
	Answer        string
	IssueTitle    string
	IssueBody     string
	Model         string
	ResolvedModel string
	ToolRounds    int
}

// APIError is an outbound failure the HTTP layer can map to a status code.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

// Turn is one earlier Ask why line in a dialog.
type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// SanitizeHistory keeps user/assistant lines, caps count and length.
func SanitizeHistory(in []Turn) []Turn {
	out := make([]Turn, 0, maxHistoryTurns)
	for _, t := range in {
		if len(out) >= maxHistoryTurns {
			break
		}
		role := strings.ToLower(strings.TrimSpace(t.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		if utf8.RuneCountInString(text) > maxHistoryRunes {
			text = string([]rune(text)[:maxHistoryRunes]) + "…"
		}
		out = append(out, Turn{Role: role, Text: text})
	}
	return out
}

// Client posts chat completions. HTTP may be nil (a 90s client is used).
type Client struct {
	HTTP *http.Client
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream,omitempty"`
	Tools       []ToolDef     `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete asks the model to explain the site. With Run set it loops on
// read-only tools. Without Run it stuffs Report into the first message.
func (c *Client) Complete(ctx context.Context, req Request) (Reply, error) {
	var zero Reply
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		return zero, &APIError{Status: http.StatusConflict, Msg: "Ask why needs an API key in Settings → System"}
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = DefaultModel
	}
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		question = "What is going on right now?"
	}
	if utf8.RuneCountInString(question) > maxQuestion {
		return zero, &APIError{Status: http.StatusBadRequest, Msg: "question is too long"}
	}

	user := "Question:\n" + question
	if t := strings.TrimSpace(req.Trigger); t != "" {
		user += "\n\nTrigger:\n" + t
	}
	if snap := strings.TrimSpace(req.Snapshot); snap != "" {
		user += "\n\nSite snapshot:\n\n" + snap
	}
	var tools []ToolDef
	if req.Run != nil {
		tools = ToolDefs()
		user += "\n\nUse tools only if the snapshot is not enough. Finish with ## Answer, ## Issue title, and ## Issue body."
	} else {
		report := strings.TrimSpace(req.Report)
		if report == "" {
			return zero, &APIError{Status: http.StatusServiceUnavailable, Msg: "help report is empty"}
		}
		if utf8.RuneCountInString(report) > maxReportRunes {
			runes := []rune(report)
			report = string(runes[:maxReportRunes]) + "\n\n[truncated]\n"
		}
		user += "\n\nSite report:\n\n" + report
	}

	messages := []chatMessage{
		{Role: "system", Content: Skill},
	}
	for _, t := range SanitizeHistory(req.History) {
		messages = append(messages, chatMessage{Role: t.Role, Content: t.Text})
	}
	messages = append(messages, chatMessage{Role: "user", Content: user})

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: Timeout}
	}
	endpoint, err := url.JoinPath(base, "chat", "completions")
	if err != nil {
		return zero, &APIError{Status: http.StatusBadRequest, Msg: "assistant.base_url is not a valid URL"}
	}

	toolRounds := 0
	for round := 0; round < maxRounds; round++ {
		useTools := len(tools) > 0 && round < maxRounds-1
		wire := chatRequest{
			Model:       model,
			Messages:    messages,
			MaxTokens:   maxTokens,
			Temperature: 0.2,
			Stream:      true,
		}
		if useTools {
			wire.Tools = tools
			wire.ToolChoice = "auto"
		}
		if req.Progress != nil {
			// A round starts its own answer. Text streamed before a tool
			// call belongs to that round, not to the reply the operator
			// ends up reading, so the UI drops what it has so far.
			req.Progress("round", "")
			req.Progress("status", "Asking the model")
		}
		msg, resolved, err := c.post(ctx, httpClient, endpoint, key, wire, req.Progress)
		if err != nil {
			return zero, err
		}
		if len(msg.ToolCalls) == 0 || req.Run == nil || !useTools {
			reply := Parse(msg.Content)
			reply.Model = model
			reply.ResolvedModel = resolved
			if reply.ResolvedModel == "" {
				reply.ResolvedModel = model
			}
			reply.ToolRounds = toolRounds
			return reply, nil
		}
		messages = append(messages, msg)
		for _, call := range msg.ToolCalls {
			toolRounds++
			if req.Progress != nil {
				req.Progress("tool", strings.TrimSpace(call.Function.Name))
			}
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    runTool(req.Run, call),
			})
		}
	}
	return zero, &APIError{Status: http.StatusBadGateway, Msg: "model kept calling tools without answering"}
}

func runTool(run Runner, call toolCall) string {
	name := strings.TrimSpace(call.Function.Name)
	if !AllowedTool(name) {
		return "unknown tool; Ask why is read-only"
	}
	args := json.RawMessage(strings.TrimSpace(call.Function.Arguments))
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	out, err := run(name, args)
	if err != nil {
		return "tool error: " + err.Error()
	}
	out = Redact(strings.TrimSpace(out))
	if utf8.RuneCountInString(out) > maxToolResult {
		out = string([]rune(out)[:maxToolResult]) + "\n[truncated]"
	}
	if out == "" {
		return "(empty)"
	}
	return out
}

func (c *Client) post(ctx context.Context, httpClient *http.Client, endpoint, key string, wire chatRequest, progress func(kind, text string)) (chatMessage, string, error) {
	var zero chatMessage
	body, err := json.Marshal(wire)
	if err != nil {
		return zero, "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return zero, "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("HTTP-Referer", "https://github.com/srcfl/ftw")
	httpReq.Header.Set("X-Title", "FTW")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "could not reach the model API"}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return zero, "", &APIError{Status: http.StatusUnauthorized, Msg: "OpenRouter rejected the API key"}
	case resp.StatusCode == http.StatusPaymentRequired:
		return zero, "", &APIError{Status: http.StatusPaymentRequired, Msg: "this model needs credits; use openrouter/free or add credit"}
	case resp.StatusCode == http.StatusTooManyRequests:
		return zero, "", &APIError{Status: http.StatusTooManyRequests, Msg: "model rate limit; try again in a minute"}
	case resp.StatusCode >= 500:
		return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "model API is unavailable"}
	case resp.StatusCode >= 400:
		return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "model API error"}
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readStream(resp.Body, progress)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	msg, model, err := parseChatJSON(raw)
	if err != nil {
		return zero, "", err
	}
	if progress != nil && msg.Content != "" && len(msg.ToolCalls) == 0 {
		progress("delta", msg.Content)
	}
	return msg, model, nil
}

type streamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string           `json:"content"`
			ToolCalls []streamToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type streamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolAcc struct {
	id, typ, name, args string
}

func readStream(body io.Reader, progress func(kind, text string)) (chatMessage, string, error) {
	var zero chatMessage
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var content strings.Builder
	resolved := ""
	accs := map[int]*toolAcc{}
	var order []int
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "model API error"}
		}
		if chunk.Model != "" {
			resolved = strings.TrimSpace(chunk.Model)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			content.WriteString(d.Content)
			if progress != nil {
				progress("delta", d.Content)
			}
		}
		for _, tc := range d.ToolCalls {
			a, ok := accs[tc.Index]
			if !ok {
				a = &toolAcc{typ: "function"}
				accs[tc.Index] = a
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Type != "" {
				a.typ = tc.Type
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "model API stream failed"}
	}
	msg := chatMessage{Role: "assistant", Content: content.String()}
	for _, idx := range order {
		a := accs[idx]
		tc := toolCall{ID: a.id, Type: a.typ}
		tc.Function.Name = a.name
		tc.Function.Arguments = a.args
		msg.ToolCalls = append(msg.ToolCalls, tc)
	}
	if msg.Content == "" && len(msg.ToolCalls) == 0 {
		return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "model returned no answer"}
	}
	return msg, resolved, nil
}

func parseChatJSON(raw []byte) (chatMessage, string, error) {
	var zero chatMessage
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "model API returned unreadable JSON"}
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "model API error"}
	}
	if len(parsed.Choices) == 0 {
		return zero, "", &APIError{Status: http.StatusBadGateway, Msg: "model returned no answer"}
	}
	return parsed.Choices[0].Message, strings.TrimSpace(parsed.Model), nil
}

// Parse splits the model's markdown into answer and optional issue fields.
func Parse(content string) Reply {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimSpace(content)
	sections := splitSections(content)
	answer := strings.TrimSpace(sections["answer"])
	if answer == "" {
		answer = content
	}
	title := cleanIssueTitle(sections["issue title"])
	body := Redact(strings.TrimSpace(sections["issue body"]))
	if title == "" {
		body = ""
	}
	return Reply{Answer: answer, IssueTitle: title, IssueBody: body}
}

func splitSections(content string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(content, "\n")
	var current string
	var buf []string
	flush := func() {
		if current == "" {
			return
		}
		out[current] = strings.TrimSpace(strings.Join(buf, "\n"))
	}
	for _, line := range lines {
		if name, ok := headingName(line); ok {
			flush()
			current = name
			buf = buf[:0]
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return out
}

func headingName(line string) (string, bool) {
	s := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(s, "#"):
		s = strings.TrimSpace(strings.TrimLeft(s, "#"))
	case strings.HasPrefix(s, "**") && strings.HasSuffix(s, "**"):
		s = strings.TrimSpace(strings.Trim(s, "*"))
	default:
		return "", false
	}
	if s == "" {
		return "", false
	}
	switch strings.ToLower(s) {
	case "answer", "issue title", "issue body":
		return strings.ToLower(s), true
	default:
		return "", false
	}
}

func cleanIssueTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`\"'")
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "none", "n/a", "na", "-", "empty", "not a bug":
		return ""
	}
	if utf8.RuneCountInString(s) > maxIssueTitle {
		s = string([]rune(s)[:maxIssueTitle-1]) + "…"
	}
	return s
}

var (
	ipv4Re   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	openKey  = regexp.MustCompile(`sk-or-v1-\S+`)
	bearerRe = regexp.MustCompile(`(?i)bearer\s+\S+`)
)

// Redact strips the obvious secret and address patterns from issue text.
func Redact(s string) string {
	s = ipv4Re.ReplaceAllString(s, "[ip omitted]")
	s = openKey.ReplaceAllString(s, "[key omitted]")
	s = bearerRe.ReplaceAllString(s, "Bearer [omitted]")
	return s
}
