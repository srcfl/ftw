package api

import (
	"bytes"
	"encoding/json"
	"time"
)

func (s *Server) supportPlanner(now time.Time) map[string]any {
	out := map[string]any{
		"schema_version": 1, "collected_at_ms": now.UnixMilli(),
		"enabled":               s.deps.MPC != nil,
		"failure_capture_scope": "latest failure retained across restarts; no failure history is stored",
	}
	if s.deps.MPC == nil {
		return out
	}
	status := s.deps.MPC.PlanSnapshot()
	out["status"] = map[string]any{
		"last_accepted_replan_at_ms": status.ReplanAt.UnixMilli(),
		"last_accepted_trigger":      status.Reason, "pending": status.Pending,
		"outdated": status.Outdated, "error": status.Error, "warnings": status.Warnings,
	}
	// Encode separately: a damaged archived plan must not hide the evidence
	// from a newer failed attempt. Neither snapshot is an executable plan.
	out["last_accepted_plan"] = json.RawMessage(supportJSON(s.deps.MPC.Diagnose()))
	out["last_failure"] = json.RawMessage(supportJSON(s.deps.MPC.PlanningFailureSnapshot()))
	out["failure_storage"] = s.deps.MPC.PlanningFailureStorageSnapshot()
	return out
}

// Reuse the dump's credential/IP rules for structured data and error text.
// Redact after parsing, then encode again so escaped strings stay valid JSON.
func supportJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		raw, _ = json.Marshal(map[string]string{"export_error": redactDumpLog(err.Error())})
		return raw
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return []byte(`{"export_error":"invalid diagnostic JSON"}`)
	}
	var redact func(any) any
	redact = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				switch {
				case isSensitiveKey(k):
					x[k] = "***"
				case isCoordinateKey(k):
					x[k] = roundCoordinate(val)
				default:
					x[k] = redact(val)
				}
			}
		case []any:
			for i := range x {
				x[i] = redact(x[i])
			}
		case string:
			return redactDumpLog(x)
		}
		return v
	}
	pretty, _ := json.MarshalIndent(redact(decoded), "", "  ")
	return pretty
}
