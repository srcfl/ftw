package assistant

import "encoding/json"

// Read-only tools the model may call. Anything else is refused as unknown.
const (
	ToolSupportReport = "get_support_report"
	ToolDriverHealth  = "get_driver_health"
	ToolRecentLogs    = "get_recent_logs"
	ToolPlanNow       = "get_plan_now"
	ToolVersion       = "get_version"
	maxRounds         = 6
	maxToolResult     = 20_000
)

// Runner executes one allowed tool. The HTTP layer implements it against
// live site state. The assistant package never talks to hardware.
type Runner func(name string, args json.RawMessage) (string, error)

// ToolDef is the OpenAI-compatible tool schema sent to the model.
type ToolDef struct {
	Type     string `json:"type"`
	Function ToolFn `json:"function"`
}

// ToolFn is the function part of a ToolDef.
type ToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolDefs is the allow-list advertised to the model.
func ToolDefs() []ToolDef {
	return []ToolDef{
		fn(ToolSupportReport,
			"The local FTW help report: findings, live power, the current plan slot, devices, versions, and recent warnings. Call this first for a full picture.",
			`{"type":"object","properties":{}}`),
		fn(ToolDriverHealth,
			"Health for one driver (yaml name) or every driver if name is omitted. Status, last success age, error count. No IP addresses or serials.",
			`{"type":"object","properties":{"name":{"type":"string","description":"Driver yaml name. Omit for every driver."}}}`),
		fn(ToolRecentLogs,
			"Recent warning and error log lines. Optional driver yaml name. Lines are redacted.",
			`{"type":"object","properties":{"driver":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":80}}}`),
		fn(ToolPlanNow,
			"What the box is doing this minute: mode, live power, the plan slot covering now, and the last commands sent.",
			`{"type":"object","properties":{}}`),
		fn(ToolVersion,
			"FTW version running on this box.",
			`{"type":"object","properties":{}}`),
	}
}

func fn(name, desc, params string) ToolDef {
	return ToolDef{
		Type: "function",
		Function: ToolFn{
			Name:        name,
			Description: desc,
			Parameters:  json.RawMessage(params),
		},
	}
}

// AllowedTool is true only for the read-only tools advertised above.
func AllowedTool(name string) bool {
	switch name {
	case ToolSupportReport, ToolDriverHealth, ToolRecentLogs, ToolPlanNow, ToolVersion:
		return true
	default:
		return false
	}
}
