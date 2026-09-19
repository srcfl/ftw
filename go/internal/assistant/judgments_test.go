package assistant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/typesafe"
)

func TestAskQuestionsCoverFanOut(t *testing.T) {
	qs := AskQuestions(SiteContext{Drivers: []string{"sungrow", "sdm630"}})
	for _, id := range []string{askQTopic, askQPlan, askQHealth, askQLogs, askQReport, askQControl, askQBug, askQUrgency, askQDriver} {
		if _, ok := qs[id]; !ok {
			t.Fatalf("missing question %s", id)
		}
	}
	drv, ok := qs[askQDriver].Criteria.(map[string]string)
	if !ok {
		t.Fatalf("driver criteria type %T", qs[askQDriver].Criteria)
	}
	if _, ok := drv["sungrow"]; !ok {
		t.Fatal("configured driver must be a Choice option")
	}
	if _, ok := drv["none"]; !ok {
		t.Fatal("Choice needs a no-match option")
	}
}

func TestAskQuestionsJSONForAPI(t *testing.T) {
	qs := AskQuestions(SiteContext{Drivers: []string{"sungrow"}})
	raw, err := json.Marshal(qs)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"type":"choice"`) || !strings.Contains(s, `"type":"noul"`) || !strings.Contains(s, `"type":"score"`) {
		t.Fatalf("questions JSON missing primitive types: %s", s)
	}
}

func TestAskStateKeepsNamedFields(t *testing.T) {
	raw, err := json.Marshal(AskState(SiteContext{
		Utterance: "why is the battery idle?",
		Trigger:   "the operator is asking why the current plan looks like this",
		Drivers:   []string{"sungrow"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"utterance"`, `"drivers"`, `"policy"`, `"ask_why"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("state missing %s: %s", want, s)
		}
	}
}

func TestComposeAskRouteSelectsToolsAndSkipsIssueOnPlan(t *testing.T) {
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		askQTopic:   choice("explain_plan", 0.92),
		askQPlan:    noul(0.88),
		askQHealth:  noul(0.1),
		askQLogs:    noul(0.2),
		askQReport:  noul(0.12),
		askQControl: noul(0.05),
		askQBug:     noul(0.91),
		askQUrgency: score(0.2),
		askQDriver:  choice("none", 0.8),
	}}
	got := ComposeAskRoute(res)
	if got.Handler != AskToolsLLM {
		t.Fatalf("handler = %s", got.Handler)
	}
	if got.FileIssue {
		t.Fatal("expected-plan questions must not file an issue from a high bug noul")
	}
	if strings.Join(got.Tools, ",") != ToolPlanNow {
		t.Fatalf("tools = %v", got.Tools)
	}
}

func TestComposeAskRouteRefusesControlWithoutDispatch(t *testing.T) {
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		askQTopic:   choice("change_control", 0.9),
		askQControl: noul(0.95),
		askQPlan:    noul(0.8),
		askQHealth:  noul(0.1),
		askQLogs:    noul(0.1),
		askQReport:  noul(0.1),
		askQBug:     noul(0.1),
		askQUrgency: score(1.1),
		askQDriver:  choice("none", 0.7),
	}}
	got := ComposeAskRoute(res)
	if got.Handler != AskRefuseControl || !got.ControlRequest {
		t.Fatalf("got %+v", got)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("refuse path should not fetch tools: %v", got.Tools)
	}
}

func TestComposeAskRouteFallsBackWhenUncertain(t *testing.T) {
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		askQTopic:   choice("other", 0.31),
		askQControl: noul(0.2),
		askQPlan:    noul(0.9),
		askQHealth:  noul(0.9),
		askQLogs:    noul(0.9),
		askQReport:  noul(0.9),
		askQBug:     noul(0.2),
		askQUrgency: score(0.4),
		askQDriver:  choice("none", 0.4),
	}}
	got := ComposeAskRoute(res)
	if got.Handler != AskFullLLM || !got.Uncertain {
		t.Fatalf("got %+v", got)
	}
	if len(got.Tools) != 5 {
		t.Fatalf("uncertain path keeps every tool, got %v", got.Tools)
	}
}

func TestComposeAskRouteSkipsLLMForChat(t *testing.T) {
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		askQTopic:   choice("conversation", 0.88),
		askQControl: noul(0.02),
		askQPlan:    noul(0.01),
		askQHealth:  noul(0.01),
		askQLogs:    noul(0.01),
		askQReport:  noul(0.01),
		askQBug:     noul(0.01),
		askQUrgency: score(0),
		askQDriver:  choice("none", 0.9),
	}}
	got := ComposeAskRoute(res)
	if got.Handler != AskSkipLLM {
		t.Fatalf("handler = %s", got.Handler)
	}
}

func noul(v float64) typesafe.Answer {
	return typesafe.Answer{Type: "noul", Noul: v}
}

func choice(c string, conf float64) typesafe.Answer {
	return typesafe.Answer{Type: "choice", Choice: c, Confidence: conf}
}

func score(v float64) typesafe.Answer {
	return typesafe.Answer{Type: "score", Score: v, Confidence: 0.7}
}
