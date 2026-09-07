package energyforecast

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

type exchangeFunc func(context.Context, []byte) ([]byte, error)

func (f exchangeFunc) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	return f(ctx, payload)
}
func ptr[T any](v T) *T { return &v }

func requestContext() RequestContext {
	return RequestContext{RequestID: "pv-1", SiteID: "site-1", ConfigRevision: "config-1", OriginMs: 1700000100000, Config: Config{PV: &PVConfig{LatitudeDeg: 57, LongitudeDeg: 15}, Load: &LoadConfig{}}}
}

func predictionRequest() PredictRequest {
	r := requestContext()
	return PredictRequest{RequestContext: r, Horizon: []HorizonSlot{{Interval: Interval{r.OriginMs, r.OriginMs + 900000}, Features: Features{LocalDay: 0, LocalWeekday: 3, LocalMinute: 12 * 60, GHIWm2: ptr(500.0), WeatherAvailableAtMs: ptr(r.OriginMs - 3600000)}}}}
}

func updateRequest() UpdateRequest {
	r := requestContext()
	return UpdateRequest{RequestContext: r, Observations: []Observation{{Interval: Interval{r.OriginMs - 900000, r.OriginMs}, Features: Features{LocalDay: 0, LocalWeekday: 3, LocalMinute: 12 * 60, GHIWm2: ptr(500.0), WeatherAvailableAtMs: ptr(r.OriginMs - 3600000)}, AvailableAtMs: r.OriginMs, HouseholdLoadW: ptr(1000.0), PVAvailableW: ptr(8000.0), LoadQuality: QualityGood, PVQuality: QualityGood}}}
}

func goodReply(payload []byte) map[string]any {
	var request map[string]any
	_ = json.Unmarshal(payload, &request)
	reply := map[string]any{"ok": true, "model_revision": 1, "latest_input_ms": map[string]any{"pv": nil, "load": nil}, "latest_training_ms": map[string]any{"pv": nil, "load": nil}, "latest_available_at_ms": map[string]any{"pv": nil, "load": nil}}
	for _, key := range []string{"op", "version", "action", "request_id", "site_id", "config_revision", "origin_ms"} {
		reply[key] = request[key]
	}
	if request["action"] == "update" {
		reply["state"] = map[string]any{"opaque": "rust-state"}
		n := len(request["observations"].([]any))
		reply["updates"] = map[string]any{"pv": map[string]any{"applied": n, "skipped": 0}, "load": map[string]any{"applied": n, "skipped": 0}}
	} else {
		var predictions []any
		for _, raw := range request["horizon"].([]any) {
			slot := raw.(map[string]any)
			predictions = append(predictions, map[string]any{"valid_start_ms": slot["valid_start_ms"], "valid_end_ms": slot["valid_end_ms"], "pv": map[string]any{"known": false, "quality": "unknown", "uncertainty": "unavailable", "coverage": 0}, "load": map[string]any{"known": true, "point_w": 500, "lower_w": 100, "upper_w": 900, "quality": "cold_start", "uncertainty": "provisional", "coverage": 0}})
		}
		reply["predictions"] = predictions
	}
	return reply
}

func replying(mutate func(map[string]any)) *Client {
	return NewClient(exchangeFunc(func(_ context.Context, payload []byte) ([]byte, error) {
		r := goodReply(payload)
		if mutate != nil {
			mutate(r)
		}
		return json.Marshal(r)
	}))
}

func firstEstimate(r map[string]any, signal string) map[string]any {
	return r["predictions"].([]any)[0].(map[string]any)[signal].(map[string]any)
}

func TestClientPreservesUnknownAndProvisional(t *testing.T) {
	reply, err := replying(nil).Predict(context.Background(), predictionRequest())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Predictions[0].PV.Known || reply.Predictions[0].PV.PointW != nil {
		t.Fatal("unknown became zero")
	}
	if reply.Predictions[0].Load.Uncertainty != "provisional" || reply.Predictions[0].Load.Quality != "cold_start" {
		t.Fatal("provisional bounds changed meaning")
	}
	if reply.ModelRevision != 1 {
		t.Fatal("numeric model revision lost")
	}
}

func TestClientRejectsWrongReplyIdentity(t *testing.T) {
	for _, key := range []string{"op", "version", "action", "request_id", "site_id", "config_revision", "origin_ms", "ok"} {
		t.Run(key, func(t *testing.T) {
			for _, drop := range []bool{false, true} {
				c := replying(func(r map[string]any) {
					if drop {
						delete(r, key)
					} else {
						switch key {
						case "version", "origin_ms":
							r[key] = 999
						case "ok":
							r[key] = false
						default:
							r[key] = "wrong"
						}
					}
				})
				if _, err := c.Predict(context.Background(), predictionRequest()); err == nil {
					t.Fatalf("invalid %s accepted, dropped=%v", key, drop)
				}
			}
		})
	}
}

func TestClientRejectsMalformedForecast(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"missing_slot":    func(r map[string]any) { r["predictions"] = []any{} },
		"wrong_interval":  func(r map[string]any) { r["predictions"].([]any)[0].(map[string]any)["valid_end_ms"] = 1 },
		"missing_model":   func(r map[string]any) { delete(r["predictions"].([]any)[0].(map[string]any), "pv") },
		"negative_point":  func(r map[string]any) { firstEstimate(r, "load")["point_w"] = -1 },
		"crossed_bounds":  func(r map[string]any) { firstEstimate(r, "load")["lower_w"] = 600 },
		"missing_point":   func(r map[string]any) { delete(firstEstimate(r, "load"), "point_w") },
		"missing_known":   func(r map[string]any) { delete(firstEstimate(r, "pv"), "known") },
		"unknown_point":   func(r map[string]any) { firstEstimate(r, "pv")["point_w"] = 0 },
		"unknown_quality": func(r map[string]any) { firstEstimate(r, "load")["quality"] = "excellent" },
		"fake_quantiles":  func(r map[string]any) { firstEstimate(r, "load")["uncertainty"] = "calibrated_p10_p90" },
		"wrong_coverage":  func(r map[string]any) { firstEstimate(r, "load")["coverage"] = 1.1 },
		"future_input":    func(r map[string]any) { r["latest_input_ms"] = map[string]any{"pv": requestContext().OriginMs + 1} },
		"future_availability": func(r map[string]any) {
			r["latest_available_at_ms"] = map[string]any{"pv": requestContext().OriginMs + 1}
		},
		"future_training": func(r map[string]any) {
			r["latest_training_ms"] = map[string]any{"pv": requestContext().OriginMs + 1, "load": nil}
		},
		"missing_clock":     func(r map[string]any) { r["latest_input_ms"] = map[string]any{} },
		"predict_has_state": func(r map[string]any) { r["state"] = map[string]any{} },
		"input_without_availability": func(r map[string]any) {
			r["latest_input_ms"] = map[string]any{"pv": requestContext().OriginMs, "load": nil}
		},
		"missing_revision":    func(r map[string]any) { delete(r, "model_revision") },
		"string_revision":     func(r map[string]any) { r["model_revision"] = "1" },
		"contradictory_error": func(r map[string]any) { r["error"] = map[string]any{"code": "broken"} },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := replying(mutate).Predict(context.Background(), predictionRequest()); err == nil {
				t.Fatal("malformed forecast accepted")
			}
		})
	}
}

func TestClientUpdateStateIsExplicitAndReplayable(t *testing.T) {
	var seen []json.RawMessage
	c := NewClient(exchangeFunc(func(_ context.Context, payload []byte) ([]byte, error) {
		var r struct{ State json.RawMessage }
		_ = json.Unmarshal(payload, &r)
		seen = append(seen, append(json.RawMessage(nil), r.State...))
		return json.Marshal(goodReply(payload))
	}))
	updated, err := c.Update(context.Background(), updateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if updated.Updates.PV.Applied != 1 || len(updated.State) == 0 {
		t.Fatal("lost update state")
	}
	r := predictionRequest()
	r.State = updated.State
	first, err := c.Predict(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Predict(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if string(seen[1]) != string(seen[2]) || first.ModelRevision != second.ModelRevision {
		t.Fatal("predict mutated supplied state")
	}
	if len(seen[0]) != 0 {
		t.Fatal("client invented initial state")
	}
}

func TestClientRejectsInvalidUpdateReply(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"state_missing":  func(r map[string]any) { delete(r, "state") },
		"state_array":    func(r map[string]any) { r["state"] = []any{1} },
		"counts_missing": func(r map[string]any) { delete(r, "updates") },
		"counts_negative": func(r map[string]any) {
			r["updates"].(map[string]any)["pv"] = map[string]any{"applied": -1, "skipped": 2}
		},
		"counts_wrong": func(r map[string]any) {
			r["updates"].(map[string]any)["pv"] = map[string]any{"applied": 2, "skipped": 0}
		},
		"future_training": func(r map[string]any) {
			r["latest_training_ms"] = map[string]any{"pv": requestContext().OriginMs + 1, "load": nil}
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := replying(mutate).Update(context.Background(), updateRequest()); err == nil {
				t.Fatal("invalid update accepted")
			}
		})
	}
}

func TestClientRejectsUnavailableWeatherAndInvalidObservationsBeforeIO(t *testing.T) {
	calls := 0
	c := NewClient(exchangeFunc(func(context.Context, []byte) ([]byte, error) { calls++; return nil, nil }))
	for _, mutate := range []func(*PredictRequest){
		func(r *PredictRequest) { r.Horizon[0].WeatherAvailableAtMs = ptr(r.OriginMs + 1) },
		func(r *PredictRequest) { r.Horizon[0].WeatherAvailableAtMs = nil },
		func(r *PredictRequest) { r.Horizon[0].GHIWm2 = ptr(math.NaN()) },
		func(r *PredictRequest) { r.Horizon[0].LocalWeekday = 0 },
		func(r *PredictRequest) { r.Horizon[0].ValidStartMs = r.OriginMs - 1 },
	} {
		r := predictionRequest()
		mutate(&r)
		if _, err := c.Predict(context.Background(), r); err == nil {
			t.Fatal("invalid forecast request accepted")
		}
	}
	for _, mutate := range []func(*UpdateRequest){
		func(r *UpdateRequest) { r.Observations[0].AvailableAtMs = r.OriginMs + 1 },
		func(r *UpdateRequest) { r.Observations[0].PVAvailableW = ptr(-1.0) },
		func(r *UpdateRequest) { r.Observations[0].ValidStartMs++ },
		func(r *UpdateRequest) { r.Observations[0].PVAvailableW = nil },
		func(r *UpdateRequest) { r.Observations = append(r.Observations, r.Observations[0]) },
	} {
		r := updateRequest()
		mutate(&r)
		if _, err := c.Update(context.Background(), r); err == nil {
			t.Fatal("invalid observation accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid request reached worker")
	}
}

func TestClientBoundsAndTimeout(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		c := NewClient(exchangeFunc(func(ctx context.Context, _ []byte) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }))
		c.Timeout = 10 * time.Millisecond
		start := time.Now()
		_, err := c.Predict(context.Background(), predictionRequest())
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
			t.Fatalf("timeout failed: %v", err)
		}
	})
	t.Run("oversize_reply", func(t *testing.T) {
		c := NewClient(exchangeFunc(func(context.Context, []byte) ([]byte, error) {
			return []byte(strings.Repeat(" ", MaxPayloadBytes+1)), nil
		}))
		if _, err := c.Predict(context.Background(), predictionRequest()); err == nil {
			t.Fatal("oversize reply accepted")
		}
	})
	t.Run("invalid_json", func(t *testing.T) {
		c := NewClient(exchangeFunc(func(context.Context, []byte) ([]byte, error) { return []byte(`{} {}`), nil }))
		if _, err := c.Predict(context.Background(), predictionRequest()); err == nil {
			t.Fatal("multiple replies accepted")
		}
	})
	t.Run("oversize_state", func(t *testing.T) {
		r := predictionRequest()
		r.State = json.RawMessage(`{"padding":"` + strings.Repeat("x", MaxStateBytes) + `"}`)
		if _, err := replying(nil).Predict(context.Background(), r); err == nil {
			t.Fatal("oversize state accepted")
		}
	})
	t.Run("too_many_slots", func(t *testing.T) {
		r := predictionRequest()
		r.Horizon = make([]HorizonSlot, MaxHorizon+1)
		if _, err := replying(nil).Predict(context.Background(), r); err == nil {
			t.Fatal("oversize horizon accepted")
		}
	})
	t.Run("empty_update", func(t *testing.T) {
		r := updateRequest()
		r.Observations = nil
		if _, err := replying(nil).Update(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	})
}
