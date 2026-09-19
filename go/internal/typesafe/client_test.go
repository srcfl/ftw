package typesafe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEvaluatePostsStateAndQuestions(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.13.0",
			"answers": map[string]any{
				"is_urgent": map[string]any{"type": "noul", "noul": 0.91},
				"topic": map[string]any{
					"type":          "choice",
					"choice":        "charging",
					"confidence":    0.84,
					"probabilities": map[string]any{"charging": 0.84, "plan": 0.16},
				},
			},
			"usage": map[string]any{"input_tokens": 120, "output_tokens": 8},
		})
	}))
	defer srv.Close()

	cli := &Client{APIKey: "ts-test", BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := cli.Evaluate(context.Background(), map[string]any{
		"utterance": "charge the car to 80%",
	}, map[string]Question{
		"is_urgent": Noul("Does this convey urgency?", "Time-sensitive for the household today", "No deadline or outage"),
		"topic": Choice("What is this about?", map[string]string{
			"charging": "Car charging",
			"plan":     "The energy plan",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/systemone" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotAuth != "Bearer ts-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"jev-latest"`) {
		t.Fatalf("default model missing: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"charge the car to 80%"`) || !strings.Contains(gotBody, `"is_urgent"`) {
		t.Fatalf("state or questions missing: %s", gotBody)
	}
	noul, ok := got.NoulOf("is_urgent")
	if !ok || noul != 0.91 {
		t.Fatalf("noul = %v ok=%v", noul, ok)
	}
	ch, ok := got.ChoiceOf("topic")
	if !ok || ch.Choice != "charging" || ch.Confidence != 0.84 {
		t.Fatalf("choice = %+v ok=%v", ch, ok)
	}
	if got.Model != "jev-1.13.0" {
		t.Fatalf("resolved model = %q", got.Model)
	}
}

func TestEvaluateRetriesOverloadedThenSucceeds(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(529)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "jev-1.13.0",
			"answers": map[string]any{"ok": map[string]any{"type": "noul", "noul": 1}},
		})
	}))
	defer srv.Close()

	cli := &Client{APIKey: "ts-test", BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := cli.Evaluate(context.Background(), "ping", map[string]Question{
		"ok": Noul("Is this a ping?", "", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts = %d", n.Load())
	}
	if v, ok := got.NoulOf("ok"); !ok || v != 1 {
		t.Fatalf("noul = %v ok=%v", v, ok)
	}
}

func TestEvaluateRejectsMissingKey(t *testing.T) {
	_, err := (*Client)(nil).Evaluate(context.Background(), "x", map[string]Question{"a": Noul("y", "", "")})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusConflict {
		t.Fatalf("err = %v", err)
	}
}

func TestEvaluateMapsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	cli := &Client{APIKey: "bad", BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := cli.Evaluate(context.Background(), "x", map[string]Question{"a": Noul("y", "", "")})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingListsAbsentIds(t *testing.T) {
	r := &Result{Answers: map[string]Answer{"a": {Type: "noul", Noul: 0.2}}}
	got := r.Missing("a", "b", "c")
	if strings.Join(got, ",") != "b,c" {
		t.Fatalf("missing = %v", got)
	}
}
