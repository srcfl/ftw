package energyforecast

import (
	"context"
	"encoding/json"
	"testing"
)

func resetRequest() ResetRequest {
	return ResetRequest{RequestContext: requestContext(), Signal: "pv", LearningStartedMs: requestContext().OriginMs}
}

func resetReply(payload []byte) map[string]any {
	var request map[string]any
	_ = json.Unmarshal(payload, &request)
	reply := map[string]any{
		"ok":                     true,
		"model_revision":         2,
		"latest_input_ms":        map[string]any{"pv": nil, "load": nil},
		"latest_training_ms":     map[string]any{"pv": nil, "load": nil},
		"latest_available_at_ms": map[string]any{"pv": nil, "load": nil},
		"state":                  map[string]any{"opaque": "reset-state"},
		"signal":                 request["signal"],
		"learning_started_ms":    request["learning_started_ms"],
	}
	for _, key := range []string{"op", "version", "action", "request_id", "site_id", "config_revision", "origin_ms"} {
		reply[key] = request[key]
	}
	return reply
}

func resetReplying(mutate func(map[string]any)) (*Client, *map[string]any) {
	var request map[string]any
	c := NewClient(exchangeFunc(func(_ context.Context, payload []byte) ([]byte, error) {
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, err
		}
		reply := resetReply(payload)
		if mutate != nil {
			mutate(reply)
		}
		return json.Marshal(reply)
	}))
	return c, &request
}

func TestClientResetSendsTargetAndAcceptsVersionedState(t *testing.T) {
	c, sent := resetReplying(nil)
	request := resetRequest()
	reply, err := c.Reset(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if (*sent)["op"] != "forecast" || (*sent)["action"] != "reset" || (*sent)["signal"] != "pv" {
		t.Fatalf("reset target = %#v", *sent)
	}
	if got := int64((*sent)["learning_started_ms"].(float64)); got != request.LearningStartedMs {
		t.Fatalf("reset cutoff = %d, want %d", got, request.LearningStartedMs)
	}
	if reply.Signal != request.Signal || reply.LearningStartedMs != request.LearningStartedMs || len(reply.State) == 0 {
		t.Fatalf("reset reply = %#v", reply)
	}
	if reply.Action != "reset" || reply.RequestID != request.RequestID || reply.SiteID != request.SiteID || reply.ConfigRevision != request.ConfigRevision || reply.OriginMs != request.OriginMs {
		t.Fatalf("reset metadata = %#v", reply.ReplyContext)
	}
}

func TestClientResetRejectsInvalidReply(t *testing.T) {
	request := resetRequest()
	for name, mutate := range map[string]func(map[string]any){
		"missing_state":  func(r map[string]any) { delete(r, "state") },
		"array_state":    func(r map[string]any) { r["state"] = []any{"not opaque"} },
		"wrong_signal":   func(r map[string]any) { r["signal"] = "load" },
		"missing_signal": func(r map[string]any) { delete(r, "signal") },
		"earlier_cutoff": func(r map[string]any) { r["learning_started_ms"] = request.LearningStartedMs - 1 },
		"future_cutoff":  func(r map[string]any) { r["learning_started_ms"] = request.OriginMs + 1 },
		"future_clock": func(r map[string]any) {
			r["latest_training_ms"] = map[string]any{"pv": request.OriginMs + 1, "load": nil}
		},
		"wrong_action": func(r map[string]any) { r["action"] = "update" },
		"predictions":  func(r map[string]any) { r["predictions"] = []any{} },
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := resetReplying(mutate)
			if _, err := c.Reset(context.Background(), request); err == nil {
				t.Fatal("invalid reset reply accepted")
			}
		})
	}
}

func TestClientResetRejectsInvalidRequestBeforeIO(t *testing.T) {
	calls := 0
	c := NewClient(exchangeFunc(func(context.Context, []byte) ([]byte, error) {
		calls++
		return nil, nil
	}))
	for _, mutate := range []func(*ResetRequest){
		func(r *ResetRequest) { r.Signal = "battery" },
		func(r *ResetRequest) { r.Signal = "pv"; r.Config.PV = nil },
		func(r *ResetRequest) { r.Signal = "load"; r.Config.Load = nil },
		func(r *ResetRequest) { r.LearningStartedMs = 0 },
		func(r *ResetRequest) { r.LearningStartedMs = r.OriginMs + 1 },
		func(r *ResetRequest) { r.State = json.RawMessage("[]") },
	} {
		request := resetRequest()
		mutate(&request)
		if _, err := c.Reset(context.Background(), request); err == nil {
			t.Fatal("invalid reset request reached client call")
		}
	}
	if calls != 0 {
		t.Fatalf("invalid reset requests reached worker %d times", calls)
	}
}
