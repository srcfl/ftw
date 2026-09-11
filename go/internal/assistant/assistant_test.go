package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseSplitsAnswerAndIssue(t *testing.T) {
	got := Parse("## Answer\nBattery is idle because the plan is in a cheap-hour wait.\n\n## Issue title\n\n## Issue body\n")
	if !strings.Contains(got.Answer, "Battery is idle") {
		t.Fatalf("answer = %q", got.Answer)
	}
	if got.IssueTitle != "" || got.IssueBody != "" {
		t.Fatalf("expected no issue, got %+v", got)
	}
}

func TestParseKeepsIssueWhenPresent(t *testing.T) {
	got := Parse("## Answer\nDriver is offline.\n## Issue title\n[bug] sungrow poll timeout\n## Issue body\nThe Sungrow driver has been offline for 20 minutes.\n")
	if got.IssueTitle != "[bug] sungrow poll timeout" {
		t.Fatalf("title = %q", got.IssueTitle)
	}
	if !strings.Contains(got.IssueBody, "Sungrow") {
		t.Fatalf("body = %q", got.IssueBody)
	}
}

func TestParseBareProseIsTheAnswer(t *testing.T) {
	got := Parse("The house is importing 400 W and that matches the plan.")
	if got.Answer != "The house is importing 400 W and that matches the plan." {
		t.Fatalf("answer = %q", got.Answer)
	}
	if got.IssueTitle != "" {
		t.Fatalf("unexpected title %q", got.IssueTitle)
	}
}

func TestRedactStripsAddressesAndKeys(t *testing.T) {
	in := "Talks to 192.168.1.10 with sk-or-v1-secret and Bearer abcdef"
	got := Redact(in)
	if strings.Contains(got, "192.168.1.10") || strings.Contains(got, "sk-or-v1") || strings.Contains(got, "abcdef") {
		t.Fatalf("not redacted: %q", got)
	}
}

func TestCompletePostsChatCompletions(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "openrouter/free",
			"choices": []map[string]any{
				{"message": map[string]string{
					"role":    "assistant",
					"content": "## Answer\nIdle as planned.\n\n## Issue title\n\n## Issue body\n",
				}},
			},
		})
	}))
	defer srv.Close()

	cli := &Client{HTTP: srv.Client()}
	reply, err := cli.Complete(context.Background(), Request{
		APIKey:   "sk-or-v1-test",
		Model:    DefaultModel,
		BaseURL:  srv.URL,
		Question: "why idle?",
		Report:   "# FTW help report\nIdle.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-or-v1-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotBody, "why idle?") || !strings.Contains(gotBody, "FTW help report") {
		t.Fatalf("prompt missing question or report: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"role":"system"`) {
		t.Fatalf("system skill missing from %s", gotBody)
	}
	if !strings.Contains(gotBody, `"stream":true`) {
		t.Fatalf("stream not requested: %s", gotBody)
	}
	if reply.Answer != "Idle as planned." {
		t.Fatalf("answer = %q", reply.Answer)
	}
	if strings.Contains(reply.Answer, "sk-or-v1-test") {
		t.Fatal("API key leaked into the reply")
	}
}

func TestCompleteStreamsTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		write := func(content string) {
			b, _ := json.Marshal(map[string]any{
				"model": "openrouter/free",
				"choices": []any{map[string]any{
					"delta": map[string]any{"content": content},
				}},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		write("## Answer\n")
		write("Idle as planned.")
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	var deltas []string
	cli := &Client{HTTP: srv.Client()}
	reply, err := cli.Complete(context.Background(), Request{
		APIKey:   "k",
		BaseURL:  srv.URL,
		Question: "why idle?",
		Report:   "idle",
		Progress: func(kind, text string) {
			if kind == "delta" {
				deltas = append(deltas, text)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "## Answer\nIdle as planned." {
		t.Fatalf("deltas = %#v", deltas)
	}
	if reply.Answer != "Idle as planned." {
		t.Fatalf("answer = %q", reply.Answer)
	}
}

// A model often narrates before it calls a tool. That text is not the
// answer, so each round is announced and the UI drops what it holds.
func TestCompleteMarksEachRoundSoStaleTokensAreDropped(t *testing.T) {
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		write := func(v map[string]any) {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": v}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		round++
		if round == 1 {
			write(map[string]any{"content": "Let me check the drivers."})
			write(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]string{"name": "get_driver_health", "arguments": "{}"},
			}}})
		} else {
			write(map[string]any{"content": "## Answer\nSungrow is offline."})
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	var kinds []string
	var deltas []string
	cli := &Client{HTTP: srv.Client()}
	reply, err := cli.Complete(context.Background(), Request{
		APIKey:   "k",
		BaseURL:  srv.URL,
		Question: "why is sungrow down?",
		Run: func(name string, args json.RawMessage) (string, error) {
			return "sungrow status=offline", nil
		},
		Progress: func(kind, text string) {
			kinds = append(kinds, kind)
			if kind == "delta" {
				deltas = append(deltas, text)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "Sungrow is offline." {
		t.Fatalf("answer = %q", reply.Answer)
	}
	// Two rounds ran, so the UI was told twice to start a fresh answer.
	rounds := 0
	for _, k := range kinds {
		if k == "round" {
			rounds++
		}
	}
	if rounds != 2 {
		t.Fatalf("round markers = %d, want 2; kinds = %v", rounds, kinds)
	}
	// The narration streamed, but a round marker separates it from the answer.
	if len(deltas) != 2 || deltas[0] != "Let me check the drivers." {
		t.Fatalf("deltas = %#v", deltas)
	}
	firstDelta := -1
	secondRound := -1
	seenRound := 0
	for i, k := range kinds {
		if k == "delta" && firstDelta < 0 {
			firstDelta = i
		}
		if k == "round" {
			seenRound++
			if seenRound == 2 {
				secondRound = i
			}
		}
	}
	if !(firstDelta < secondRound) {
		t.Fatalf("the second round must follow the narration; kinds = %v", kinds)
	}
}

func TestCompleteMapsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	cli := &Client{HTTP: srv.Client()}
	_, err := cli.Complete(context.Background(), Request{
		APIKey: "bad", Model: DefaultModel, BaseURL: srv.URL, Report: "x",
	})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func TestCompleteRejectsEmptyKey(t *testing.T) {
	cli := &Client{}
	_, err := cli.Complete(context.Background(), Request{Report: "x"})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusConflict {
		t.Fatalf("err = %v", err)
	}
}

func TestSanitizeHistoryKeepsUserAndAssistant(t *testing.T) {
	got := SanitizeHistory([]Turn{
		{Role: "system", Text: "ignore"},
		{Role: "user", Text: "why charge?"},
		{Role: "assistant", Text: "Cheap hour."},
		{Role: "tool", Text: "nope"},
		{Role: "user", Text: ""},
	})
	if len(got) != 2 || got[0].Role != "user" || got[1].Role != "assistant" {
		t.Fatalf("got %#v", got)
	}
}

func TestSanitizeHistoryCapsLength(t *testing.T) {
	long := strings.Repeat("x", maxHistoryRunes+20)
	got := SanitizeHistory([]Turn{{Role: "user", Text: long}})
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if utf8.RuneCountInString(got[0].Text) != maxHistoryRunes+1 { // plus ellipsis
		t.Fatalf("len = %d", utf8.RuneCountInString(got[0].Text))
	}
	if !strings.HasSuffix(got[0].Text, "…") {
		t.Fatalf("missing ellipsis: %q", got[0].Text)
	}
}

func TestCompleteIncludesHistory(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "## Answer\nStill cheap.\n"}},
			},
		})
	}))
	defer srv.Close()
	cli := &Client{HTTP: srv.Client()}
	_, err := cli.Complete(context.Background(), Request{
		APIKey:   "k",
		BaseURL:  srv.URL,
		Question: "what about tomorrow?",
		Report:   "idle",
		History: []Turn{
			{Role: "user", Text: "why charge?"},
			{Role: "assistant", Text: "Cheap hour."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, "why charge?") || !strings.Contains(gotBody, "Cheap hour.") {
		t.Fatalf("history missing from prompt: %s", gotBody)
	}
	if !strings.Contains(gotBody, "what about tomorrow?") {
		t.Fatalf("current question missing: %s", gotBody)
	}
}

func TestCompleteRunsReadOnlyTools(t *testing.T) {
	var saw []string
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		saw = append(saw, string(raw))
		if strings.Contains(string(raw), `"role":"tool"`) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model": "openrouter/free",
				"choices": []map[string]any{
					{"message": map[string]string{
						"role":    "assistant",
						"content": "## Answer\nSungrow is offline.\n\n## Issue title\n\n## Issue body\n",
					}},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{
							"id":   "call_1",
							"type": "function",
							"function": map[string]string{
								"name":      "get_driver_health",
								"arguments": `{"name":"sungrow"}`,
							},
						},
						{
							"id":   "call_bad",
							"type": "function",
							"function": map[string]string{
								"name":      "driver_command",
								"arguments": `{"action":"battery"}`,
							},
						},
					},
				}},
			},
		})
	}))
	defer srv.Close()

	cli := &Client{HTTP: srv.Client()}
	reply, err := cli.Complete(context.Background(), Request{
		APIKey:   "k",
		BaseURL:  srv.URL,
		Question: "why is sungrow down?",
		Trigger:  "driver sungrow is offline",
		Run: func(name string, args json.RawMessage) (string, error) {
			called = append(called, name)
			return "sungrow status=offline last_success=12 min ago", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "Sungrow is offline." {
		t.Fatalf("answer = %q", reply.Answer)
	}
	if reply.ToolRounds != 2 {
		t.Fatalf("tool rounds = %d, want 2 (health + rejected write)", reply.ToolRounds)
	}
	if len(called) != 1 || called[0] != ToolDriverHealth {
		t.Fatalf("runner called %v, want only get_driver_health", called)
	}
	if !strings.Contains(saw[0], `"name":"get_driver_health"`) {
		t.Fatalf("first request missing tools: %s", saw[0])
	}
	if !strings.Contains(saw[0], "driver sungrow is offline") {
		t.Fatalf("first request missing trigger: %s", saw[0])
	}
	if !strings.Contains(saw[1], "unknown tool; Ask why is read-only") {
		t.Fatalf("write tool was not refused: %s", saw[1])
	}
	if !strings.Contains(saw[1], "sungrow status=offline") {
		t.Fatalf("health result missing: %s", saw[1])
	}
}

func TestAllowedToolIsReadOnly(t *testing.T) {
	if AllowedTool("driver_command") || AllowedTool("set_mode") {
		t.Fatal("write names must not be allowed")
	}
	if !AllowedTool(ToolSupportReport) || !AllowedTool(ToolPlanNow) {
		t.Fatal("read tools must be allowed")
	}
}
