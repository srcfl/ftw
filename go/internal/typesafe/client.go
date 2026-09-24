// Package typesafe calls TypeSafe's System One HTTP API.
//
// Jev returns typed judgments and probabilities. Callers own workflow,
// safety and dispatch. This package does not talk to hardware.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultModel   = "jev-latest"
	// Timeout covers a slow or retried System One call. Typical answers
	// are far faster; this is a ceiling, not a target.
	Timeout     = 15 * time.Second
	maxBody     = 1 << 20
	maxAttempts = 3
)

// Client posts state and questions to POST /v1/systemone.
type Client struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// Request is one System One evaluation.
type Request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// Question is one Choice, Noul or Score. Instructions and criteria may
// be strings or JSON structure; the live TypeSafe docs are the contract.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Result is the typed answers for one request, keyed by the same ids.
type Result struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Answer is one Noul, Choice or Score. Noul has no separate confidence;
// a value near 0.5 means similar probability for yes and no.
type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Legend        map[string]string  `json:"legend"`
}

// Usage is token accounting from the API. Output tokens are free on Jev.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// APIError is an outbound failure the caller can map to a status code.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

// Noul builds a yes/no question. Empty true/false descriptions omit criteria.
func Noul(instructions string, yes, no string) Question {
	q := Question{Type: "noul", Instructions: instructions}
	if strings.TrimSpace(yes) != "" || strings.TrimSpace(no) != "" {
		q.Criteria = map[string]string{"true": yes, "false": no}
	}
	return q
}

// Choice builds a one-of-a-set question. Option keys are the values code
// will receive; descriptions are the rubric, not display copy.
func Choice(instructions string, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: options}
}

// Score builds an ordered rubric. Levels must stand on their own; the
// weighted score can land between them.
func Score(instructions string, levels []string) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// Evaluate asks every question over the same state in one call.
func (c *Client) Evaluate(ctx context.Context, state any, questions map[string]Question) (*Result, error) {
	if c == nil || strings.TrimSpace(c.APIKey) == "" {
		return nil, &APIError{Status: http.StatusConflict, Msg: "TypeSafe API key is missing"}
	}
	if len(questions) == 0 {
		return nil, &APIError{Status: http.StatusBadRequest, Msg: "TypeSafe request has no questions"}
	}
	model := strings.TrimSpace(c.Model)
	if model == "" {
		model = DefaultModel
	}
	endpoint, err := c.endpoint()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(Request{State: state, Model: model, Questions: questions})
	if err != nil {
		return nil, &APIError{Status: http.StatusBadRequest, Msg: "TypeSafe request is not JSON"}
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: Timeout}
	}

	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, retry, err := c.post(ctx, httpClient, endpoint, body)
		if err == nil {
			return result, nil
		}
		last = err
		if !retry || attempt == maxAttempts {
			return nil, err
		}
		delay := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, last
}

func (c *Client) endpoint() (string, error) {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	endpoint, err := url.JoinPath(base, "v1", "systemone")
	if err != nil {
		return "", &APIError{Status: http.StatusBadRequest, Msg: "TypeSafe base URL is not valid"}
	}
	return endpoint, nil
}

func (c *Client) post(ctx context.Context, httpClient *http.Client, endpoint string, body []byte) (*Result, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, &APIError{Status: http.StatusBadRequest, Msg: "TypeSafe request could not be built"}
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.APIKey))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, true, &APIError{Status: http.StatusBadGateway, Msg: "could not reach TypeSafe"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, false, &APIError{Status: http.StatusUnauthorized, Msg: "TypeSafe rejected the API key"}
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, false, &APIError{Status: http.StatusBadRequest, Msg: "TypeSafe rejected the questions or state"}
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == 529:
		return nil, true, &APIError{Status: http.StatusTooManyRequests, Msg: "TypeSafe is rate limited or overloaded"}
	case resp.StatusCode >= 500:
		return nil, true, &APIError{Status: http.StatusBadGateway, Msg: "TypeSafe is unavailable"}
	case resp.StatusCode >= 400:
		return nil, false, &APIError{Status: http.StatusBadGateway, Msg: "TypeSafe request failed"}
	}

	var out Result
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, &APIError{Status: http.StatusBadGateway, Msg: "TypeSafe returned unreadable JSON"}
	}
	if out.Answers == nil {
		out.Answers = map[string]Answer{}
	}
	return &out, false, nil
}

// NoulOf returns the yes-probability for id.
func (r *Result) NoulOf(id string) (float64, bool) {
	if r == nil {
		return 0, false
	}
	a, ok := r.Answers[id]
	if !ok || a.Type != "noul" {
		return 0, false
	}
	return a.Noul, true
}

// ChoiceOf returns the winning option for id.
func (r *Result) ChoiceOf(id string) (Answer, bool) {
	if r == nil {
		return Answer{}, false
	}
	a, ok := r.Answers[id]
	if !ok || a.Type != "choice" {
		return Answer{}, false
	}
	return a, true
}

// ScoreOf returns the weighted score for id.
func (r *Result) ScoreOf(id string) (Answer, bool) {
	if r == nil {
		return Answer{}, false
	}
	a, ok := r.Answers[id]
	if !ok || a.Type != "score" {
		return Answer{}, false
	}
	return a, true
}

// Missing reports question ids that have no matching answer.
func (r *Result) Missing(ids ...string) []string {
	var out []string
	if r == nil {
		return append([]string{}, ids...)
	}
	for _, id := range ids {
		if _, ok := r.Answers[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}
