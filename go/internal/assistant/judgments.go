package assistant

import (
	"strings"

	"github.com/srcfl/ftw/go/internal/typesafe"
)

// Thresholds are starting points to evaluate on real Ask why questions.
// They are not TypeSafe cookbook defaults and they are not permission
// to act on hardware.
const (
	askChoiceConfidenceMin = 0.5
	askToolNoulMin         = 0.55
	askControlNoulMin      = 0.7
	askBugNoulMin          = 0.8
)

// AskHandler is the code path Ask why should take after Jev answers.
// None of these dispatch; Ask why stays read-only.
type AskHandler string

const (
	// AskFullLLM is today's path: snapshot plus every tool, then the
	// OpenRouter writer.
	AskFullLLM AskHandler = "full_llm"
	// AskToolsLLM calls only the tools Jev judged relevant, then the writer.
	AskToolsLLM AskHandler = "tools_llm"
	// AskRefuseControl tells the operator Ask why cannot change the site.
	// A later household-intent mapper may propose an existing Core operation.
	AskRefuseControl AskHandler = "refuse_control"
	// AskSkipLLM is greeting or off-topic; no investigation.
	AskSkipLLM AskHandler = "skip_llm"
)

const (
	askTopicExplainPlan = "explain_plan"
	askTopicCharging    = "charging"
	askTopicDriver      = "driver"
	askTopicSavings     = "savings"
	askTopicHowTo       = "how_to"
	askTopicReportBug   = "report_bug"
	askTopicChat        = "conversation"
	askTopicControl     = "change_control"
	askTopicOther       = "other"

	askQTopic   = "topic"
	askQPlan    = "needs_plan_now"
	askQHealth  = "needs_driver_health"
	askQLogs    = "needs_recent_logs"
	askQReport  = "needs_support_report"
	askQControl = "is_control_request"
	askQBug     = "looks_like_ftw_bug"
	askQUrgency = "urgency"
	askQDriver  = "implicated_driver"
)

// SiteContext is named state for Jev: the utterance plus facts the box
// already knows. Drivers and loadpoints must be the live configured set;
// Jev cannot choose an omitted identity.
type SiteContext struct {
	Utterance  string         `json:"utterance"`
	Trigger    string         `json:"trigger,omitempty"`
	Drivers    []string       `json:"drivers"`
	Loadpoints []LoadpointRef `json:"loadpoints"`
	Snapshot   string         `json:"snapshot,omitempty"`
}

// LoadpointRef is a charger the household can name.
type LoadpointRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// AskRoute is the composed Ask why decision. Tools is a subset of the
// read-only allow-list. FileIssue is a hint for the writer, not a post.
type AskRoute struct {
	Handler         AskHandler
	Topic           string
	TopicConfidence float64
	Tools           []string
	FileIssue       bool
	ControlRequest  bool
	Urgency         float64
	Uncertain       bool
	Driver          string
}

// AskState is the JSON object sent as TypeSafe state.
func AskState(site SiteContext) map[string]any {
	drivers := site.Drivers
	if drivers == nil {
		drivers = []string{}
	}
	lps := site.Loadpoints
	if lps == nil {
		lps = []LoadpointRef{}
	}
	return map[string]any{
		"utterance":  strings.TrimSpace(site.Utterance),
		"trigger":    strings.TrimSpace(site.Trigger),
		"drivers":    drivers,
		"loadpoints": lps,
		"snapshot":   strings.TrimSpace(site.Snapshot),
		"policy": map[string]string{
			"ask_why":  "read-only helper; it cannot command hardware or change config",
			"dispatch": "only Core may dispatch, and only after admission and freshness checks",
		},
	}
}

// AskQuestions is one speculative fan-out: topic, tool needs, control
// vs explanation, and whether this looks like an FTW bug. Independent
// questions share the same state; code consumes only the relevant answers.
func AskQuestions(site SiteContext) map[string]typesafe.Question {
	driverOpts := map[string]string{
		"none": "No specific configured driver is implicated, or the utterance is not about a device fault.",
	}
	for _, name := range site.Drivers {
		name = strings.TrimSpace(name)
		if name == "" || name == "none" {
			continue
		}
		driverOpts[name] = "The utterance is about the configured driver named " + name + "."
	}
	return map[string]typesafe.Question{
		askQTopic: typesafe.Choice(
			"What is the person trying to do with this FTW home energy box?",
			map[string]string{
				askTopicExplainPlan: "Understand the current energy plan, battery charge/discharge, or why the box is idle or importing.",
				askTopicCharging:    "Car charging: plugged in, SoC, ready-by time, Charge now, or whether the car will be ready.",
				askTopicDriver:      "A device, inverter, meter or charger looks broken, offline, stale or in fault.",
				askTopicSavings:     "Cost, prices, export, or whether FTW is saving money.",
				askTopicHowTo:       "How to use a setting, the UI, or a feature. Not a live diagnosis.",
				askTopicReportBug:   "They want to file a bug or they claim FTW itself is wrong.",
				askTopicChat:        "Greeting, thanks, or something unrelated to this house's energy system.",
				askTopicControl:     "They want the box to change power, mode, charging or hardware right now, not merely explain it.",
				askTopicOther:       "None of the other options fit.",
			},
		),
		askQPlan: typesafe.Noul(
			"Does answering require the current plan slot and the hours-ahead battery intent?",
			"The question is about what the planner is doing now or later today.",
			"The question can be answered without the plan.",
		),
		askQHealth: typesafe.Noul(
			"Does answering require driver health (online/offline, last success, faults)?",
			"A device, poll or fault is in question.",
			"Driver health would not change the answer.",
		),
		askQLogs: typesafe.Noul(
			"Does answering require recent warning or error logs?",
			"They are diagnosing a failure or unexpected behaviour.",
			"Logs are not needed.",
		),
		askQReport: typesafe.Noul(
			"Is the compact snapshot in `snapshot` insufficient, so the full local help report is needed?",
			"The snapshot is missing the evidence this question needs.",
			"The snapshot, or a single targeted tool, is enough.",
		),
		askQControl: typesafe.Noul(
			"Is the person asking FTW to change equipment behaviour, not just explain it?",
			"Charge now, stop charging, set a schedule or SoC, change mode, turn equipment off, or otherwise command the site.",
			"They want an explanation, a diagnosis, or how-to help.",
		),
		askQBug: typesafe.Noul(
			"Does this look like a defect in FTW or a bundled driver, rather than expected control or a site/config issue?",
			"FTW or a driver is misbehaving relative to the evidence.",
			"Expected control, operator misunderstanding, or a site/config problem. Leave issue fields empty.",
		),
		askQUrgency: typesafe.Score(
			"How time-sensitive is this for the household?",
			[]string{
				"Routine curiosity, a past event, or no deadline.",
				"Something is wrong or at risk later today: a charging deadline, an offline device, a plan that will miss a goal.",
				"Immediate physical or safety language: fuse, fire, overheating, stuck dispatch, flooding power.",
			},
		),
		askQDriver: typesafe.Choice(
			"Which configured driver is implicated in `drivers`, if any?",
			driverOpts,
		),
	}
}

// ComposeAskRoute turns Jev's answers into an Ask why code path.
// Missing or low-confidence topic falls back to today's full LLM path.
func ComposeAskRoute(res *typesafe.Result) AskRoute {
	out := AskRoute{Handler: AskFullLLM, Uncertain: true}
	if res == nil {
		return out
	}
	if topic, ok := res.ChoiceOf(askQTopic); ok {
		out.Topic = topic.Choice
		out.TopicConfidence = topic.Confidence
		out.Uncertain = topic.Confidence < askChoiceConfidenceMin || topic.Choice == ""
	}
	if v, ok := res.NoulOf(askQControl); ok {
		out.ControlRequest = v >= askControlNoulMin
	}
	if v, ok := res.NoulOf(askQBug); ok {
		// Expected plan questions must not become GitHub issues just
		// because the operator is surprised by cheap-hour idle.
		out.FileIssue = v >= askBugNoulMin && out.Topic != askTopicExplainPlan
	}
	if urg, ok := res.ScoreOf(askQUrgency); ok {
		out.Urgency = urg.Score
	}
	if drv, ok := res.ChoiceOf(askQDriver); ok && drv.Choice != "" && drv.Choice != "none" {
		out.Driver = drv.Choice
	}

	if out.ControlRequest {
		out.Handler = AskRefuseControl
		return out
	}
	if out.Uncertain {
		out.Handler = AskFullLLM
		out.Tools = allAskTools()
		return out
	}
	if out.Topic == askTopicChat {
		out.Handler = AskSkipLLM
		return out
	}

	out.Tools = selectAskTools(res)
	out.Handler = AskToolsLLM
	return out
}

func selectAskTools(res *typesafe.Result) []string {
	var tools []string
	add := func(noulID, tool string) {
		v, ok := res.NoulOf(noulID)
		if ok && v >= askToolNoulMin {
			tools = append(tools, tool)
		}
	}
	add(askQReport, ToolSupportReport)
	add(askQHealth, ToolDriverHealth)
	add(askQLogs, ToolRecentLogs)
	add(askQPlan, ToolPlanNow)
	return tools
}

func allAskTools() []string {
	return []string{ToolSupportReport, ToolDriverHealth, ToolRecentLogs, ToolPlanNow, ToolVersion}
}
