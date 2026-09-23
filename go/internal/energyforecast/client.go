package energyforecast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

const DefaultTimeout = 5 * time.Second
const MaxTimeout = 30 * time.Second

type Client struct {
	transport RoundTripper
	// Timeout may be set at construction. Calls always enforce MaxTimeout.
	Timeout time.Duration
}

func NewClient(transport RoundTripper) *Client {
	return &Client{transport: transport, Timeout: DefaultTimeout}
}

type workerError struct{ detail string }

func (e workerError) Error() string { return "forecast worker rejected request: " + e.detail }

func (c *Client) Update(ctx context.Context, request UpdateRequest) (UpdateReply, error) {
	var reply UpdateReply
	if err := validateContext(request.RequestContext); err != nil {
		return reply, err
	}
	if len(request.Observations) > MaxObservations {
		return reply, errors.New("forecast update accepts at most 4096 observations")
	}
	if request.Observations == nil {
		request.Observations = []Observation{}
	}
	var previousEnd int64
	for i, o := range request.Observations {
		if err := validateInterval(o.Interval); err != nil {
			return reply, fmt.Errorf("observation %d: %w", i, err)
		}
		if o.ValidEndMs-o.ValidStartMs != 900000 || o.ValidStartMs%900000 != 0 {
			return reply, errors.New("forecast observation must cover a complete quarter-hour")
		}
		if i > 0 && o.ValidStartMs < previousEnd {
			return reply, errors.New("forecast observations overlap or are out of order")
		}
		previousEnd = o.ValidEndMs
		if o.AvailableAtMs < o.ValidEndMs || o.AvailableAtMs > request.OriginMs {
			return reply, errors.New("forecast observation was not available at origin")
		}
		if err := validateFeatures(o.Features, request.OriginMs); err != nil {
			return reply, fmt.Errorf("observation %d: %w", i, err)
		}
		for _, signal := range []struct {
			value   *float64
			quality ObservationQuality
		}{{o.PVAvailableW, o.PVQuality}, {o.HouseholdLoadW, o.LoadQuality}} {
			if !validObservationQuality(signal.quality) {
				return reply, errors.New("forecast observation needs explicit quality")
			}
			if signal.value != nil && (!finite(*signal.value) || *signal.value < 0) {
				return reply, errors.New("forecast observation power must be finite and nonnegative")
			}
			if signal.value == nil && (signal.quality == QualityGood || signal.quality == QualityClipped) {
				return reply, errors.New("good forecast observation has no measured power")
			}
		}
	}
	payload := struct {
		Op      string `json:"op"`
		Version int    `json:"version"`
		Action  string `json:"action"`
		UpdateRequest
	}{"forecast", ProtocolVersion, "update", request}
	if err := c.call(ctx, payload, request.RequestContext, "update", &reply); err != nil {
		return UpdateReply{}, err
	}
	if err := validateState(reply.State, true); err != nil {
		return UpdateReply{}, err
	}
	if err := validateCounts(reply.Updates.PV, request.Config.PV != nil, len(request.Observations)); err != nil {
		return UpdateReply{}, err
	}
	if err := validateCounts(reply.Updates.Load, request.Config.Load != nil, len(request.Observations)); err != nil {
		return UpdateReply{}, err
	}
	return reply, nil
}

func (c *Client) Predict(ctx context.Context, request PredictRequest) (PredictReply, error) {
	var reply PredictReply
	if err := validateContext(request.RequestContext); err != nil {
		return reply, err
	}
	if len(request.Horizon) == 0 || len(request.Horizon) > MaxHorizon {
		return reply, errors.New("forecast predict needs 1..512 intervals")
	}
	var previousEnd int64
	for i, slot := range request.Horizon {
		if err := validateInterval(slot.Interval); err != nil {
			return reply, fmt.Errorf("horizon %d: %w", i, err)
		}
		if slot.ValidStartMs < request.OriginMs || (i > 0 && slot.ValidStartMs < previousEnd) {
			return reply, errors.New("forecast horizon is past, overlapping or out of order")
		}
		previousEnd = slot.ValidEndMs
		if err := validateFeatures(slot.Features, request.OriginMs); err != nil {
			return reply, fmt.Errorf("horizon %d: %w", i, err)
		}
	}
	payload := struct {
		Op      string `json:"op"`
		Version int    `json:"version"`
		Action  string `json:"action"`
		PredictRequest
	}{"forecast", ProtocolVersion, "predict", request}
	if err := c.call(ctx, payload, request.RequestContext, "predict", &reply); err != nil {
		return PredictReply{}, err
	}
	for _, latest := range []*int64{reply.LatestTrainingMs.PV, reply.LatestTrainingMs.Load} {
		if latest != nil && (*latest < 0 || *latest > request.OriginMs) {
			return PredictReply{}, errors.New("forecast reply contains future training")
		}
	}
	if len(reply.Predictions) != len(request.Horizon) {
		return PredictReply{}, errors.New("forecast reply has wrong interval count")
	}
	for _, latest := range []*int64{reply.LatestInputMs.PV, reply.LatestInputMs.Load} {
		if latest != nil && (*latest < 0 || *latest > request.OriginMs) {
			return PredictReply{}, errors.New("forecast reply contains future model inputs")
		}
	}
	for i, p := range reply.Predictions {
		if p.Interval != request.Horizon[i].Interval {
			return PredictReply{}, fmt.Errorf("forecast reply interval %d does not match request", i)
		}
		if (p.PV != nil) != (request.Config.PV != nil) || (p.Load != nil) != (request.Config.Load != nil) {
			return PredictReply{}, errors.New("forecast reply changed requested model set")
		}
		if err := validateEstimate(p.PV); err != nil {
			return PredictReply{}, fmt.Errorf("PV interval %d: %w", i, err)
		}
		if err := validateEstimate(p.Load); err != nil {
			return PredictReply{}, fmt.Errorf("load interval %d: %w", i, err)
		}
	}
	return reply, nil
}

func (c *Client) call(ctx context.Context, request any, expected RequestContext, action string, reply any) error {
	if c == nil || c.transport == nil {
		return errors.New("forecast worker unavailable")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode forecast request: %w", err)
	}
	if len(payload)+1 > MaxPayloadBytes {
		return errors.New("forecast request exceeds 2 MiB")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return err
	}
	line, err := c.transport.RoundTrip(bounded, payload)
	if err != nil {
		return fmt.Errorf("forecast worker exchange: %w", err)
	}
	if err := bounded.Err(); err != nil {
		return err
	}
	if len(line) > MaxPayloadBytes {
		return errors.New("forecast reply exceeds 2 MiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return fmt.Errorf("decode forecast reply: %w", err)
	}
	for _, name := range []string{"op", "version", "action", "request_id", "site_id", "config_revision", "origin_ms", "ok"} {
		if raw, ok := fields[name]; !ok || bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("forecast reply missing %s", name)
		}
	}
	var echo ReplyContext
	if err := json.Unmarshal(line, &echo); err != nil {
		return fmt.Errorf("decode forecast reply: %w", err)
	}
	if echo.Op != "forecast" || echo.Version != ProtocolVersion || echo.Action != action || echo.RequestID != expected.RequestID || echo.SiteID != expected.SiteID || echo.ConfigRevision != expected.ConfigRevision || echo.OriginMs != expected.OriginMs {
		return errors.New("forecast reply identity does not match request")
	}
	if !echo.OK {
		detail := string(echo.Error)
		if len(detail) > 512 {
			detail = detail[:512]
		}
		if detail == "" || detail == "null" {
			detail = "unspecified error"
		}
		return workerError{detail}
	}
	if raw, ok := fields["model_revision"]; !ok || bytes.Equal(raw, []byte("null")) {
		return errors.New("forecast reply lacks a model revision")
	}
	if len(echo.Error) > 0 && !bytes.Equal(echo.Error, []byte("null")) {
		return errors.New("successful forecast reply also has an error")
	}
	if action == "predict" {
		for _, name := range []string{"state", "updates"} {
			if _, ok := fields[name]; ok {
				return fmt.Errorf("predict reply unexpectedly contains %s", name)
			}
		}
	} else if _, ok := fields["predictions"]; ok {
		return errors.New("update reply unexpectedly contains predictions")
	}
	for _, name := range []string{"latest_input_ms", "latest_training_ms", "latest_available_at_ms"} {
		raw, ok := fields[name]
		if !ok || bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("forecast reply missing %s", name)
		}
		var clocks map[string]json.RawMessage
		if err := json.Unmarshal(raw, &clocks); err != nil {
			return fmt.Errorf("forecast reply invalid %s", name)
		}
		for signal, enabled := range map[string]bool{"pv": expected.Config.PV != nil, "load": expected.Config.Load != nil} {
			if enabled {
				if _, ok := clocks[signal]; !ok {
					return fmt.Errorf("forecast reply missing %s.%s", name, signal)
				}
			}
		}
	}
	for _, pair := range [][3]*int64{{echo.LatestInputMs.PV, echo.LatestAvailableAtMs.PV, echo.LatestTrainingMs.PV}, {echo.LatestInputMs.Load, echo.LatestAvailableAtMs.Load, echo.LatestTrainingMs.Load}} {
		for _, latest := range pair {
			if latest != nil && (*latest < 0 || *latest > expected.OriginMs) {
				return errors.New("forecast reply contains future model inputs")
			}
		}
		if pair[0] != nil && pair[1] != nil && *pair[1] < *pair[0] {
			return errors.New("model availability precedes latest input")
		}
		if (pair[0] == nil) != (pair[1] == nil) {
			return errors.New("model input and availability metadata disagree")
		}
		if pair[2] != nil && (pair[0] == nil || *pair[2] > *pair[0]) {
			return errors.New("model training exceeds latest input")
		}
	}
	if expected.Config.PV == nil && (echo.LatestInputMs.PV != nil || echo.LatestAvailableAtMs.PV != nil || echo.LatestTrainingMs.PV != nil) {
		return errors.New("forecast reply includes unrequested PV metadata")
	}
	if expected.Config.Load == nil && (echo.LatestInputMs.Load != nil || echo.LatestAvailableAtMs.Load != nil || echo.LatestTrainingMs.Load != nil) {
		return errors.New("forecast reply includes unrequested load metadata")
	}
	if action == "predict" {
		for _, name := range []string{"predictions"} {
			if _, ok := fields[name]; !ok {
				return fmt.Errorf("forecast reply missing %s", name)
			}
		}
	}
	if err := json.Unmarshal(line, reply); err != nil {
		return fmt.Errorf("decode forecast payload: %w", err)
	}
	return nil
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func validID(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 128 && strings.IndexFunc(s, unicode.IsControl) < 0
}

func validateContext(r RequestContext) error {
	if !validID(r.RequestID) || !validID(r.SiteID) || !validID(r.ConfigRevision) || r.OriginMs < 0 {
		return errors.New("forecast request needs bounded site, revision, request ID and valid origin")
	}

	if r.Config.PV == nil && r.Config.Load == nil {
		return errors.New("forecast request has no configured model")
	}
	if p := r.Config.PV; p != nil {
		if !finite(p.LatitudeDeg) || math.Abs(p.LatitudeDeg) > 90 || !finite(p.LongitudeDeg) || math.Abs(p.LongitudeDeg) > 180 || (p.ACLimitW != nil && (!finite(*p.ACLimitW) || *p.ACLimitW <= 0)) {
			return errors.New("forecast PV config has invalid coordinates or AC limit")
		}
	}
	return validateState(r.State, false)
}

func validateState(state json.RawMessage, required bool) error {
	if len(state) == 0 {
		if required {
			return errors.New("forecast update reply has no state")
		}
		return nil
	}
	if len(state) > MaxStateBytes || !json.Valid(state) {
		return errors.New("forecast state is invalid or exceeds 1 MiB")
	}
	s := bytes.TrimSpace(state)
	if len(s) == 0 || s[0] != '{' {
		return errors.New("forecast state must be an opaque JSON object")
	}
	return nil
}

func validateInterval(i Interval) error {
	if i.ValidStartMs < 0 || i.ValidEndMs <= i.ValidStartMs || i.ValidEndMs-i.ValidStartMs > 900000 || i.ValidStartMs/900000 != (i.ValidEndMs-1)/900000 {
		return errors.New("invalid forecast interval")
	}
	return nil
}

func validateFeatures(f Features, origin int64) error {
	if f.LocalDay < -1 || f.LocalDay > 3000000 || f.LocalWeekday < 0 || f.LocalWeekday > 6 || f.LocalMinute < 0 || f.LocalMinute >= 1440 || f.LocalMinute%15 != 0 || int((f.LocalDay+3)%7) != f.LocalWeekday {
		return errors.New("invalid local calendar features")
	}
	weather := f.GHIWm2 != nil || f.CloudPct != nil || f.TempC != nil
	if weather && f.WeatherAvailableAtMs == nil {
		return errors.New("weather input lacks availability time")
	}
	if f.WeatherAvailableAtMs != nil && (*f.WeatherAvailableAtMs < 0 || *f.WeatherAvailableAtMs > origin) {
		return errors.New("weather was not available at forecast origin")
	}
	if f.GHIWm2 != nil && (!finite(*f.GHIWm2) || *f.GHIWm2 < 0 || *f.GHIWm2 > 3000) {
		return errors.New("invalid GHI")
	}
	if f.CloudPct != nil && (!finite(*f.CloudPct) || *f.CloudPct < 0 || *f.CloudPct > 100) {
		return errors.New("invalid cloud cover")
	}
	if f.TempC != nil && (!finite(*f.TempC) || *f.TempC < -100 || *f.TempC > 80) {
		return errors.New("invalid temperature")
	}
	return nil
}

func validObservationQuality(q ObservationQuality) bool {
	switch q {
	case QualityGood, QualityMissing, QualityStale, QualityIncomplete, QualityCurtailed, QualityClipped:
		return true
	}
	return false
}

func validateCounts(c *UpdateCount, enabled bool, total int) error {
	if (c != nil) != enabled {
		return errors.New("forecast update changed requested model set")
	}
	if c != nil && (c.Applied < 0 || c.Skipped < 0 || c.Applied > total || c.Skipped > total || c.Applied+c.Skipped != total) {
		return errors.New("forecast update returned invalid counts")
	}
	return nil
}

func (c *UpdateCount) UnmarshalJSON(data []byte) error {
	type plain UpdateCount
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"applied", "skipped"} {
		if raw, ok := fields[name]; !ok || bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("forecast update missing %s count", name)
		}
	}
	return json.Unmarshal(data, (*plain)(c))
}

func (e *Estimate) UnmarshalJSON(data []byte) error {
	type plain Estimate
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"known", "quality", "uncertainty", "coverage"} {
		if raw, ok := fields[name]; !ok || bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("forecast estimate missing %s", name)
		}
	}
	return json.Unmarshal(data, (*plain)(e))
}

func validateEstimate(e *Estimate) error {
	if e == nil {
		return nil
	}
	if !finite(e.Coverage) || e.Coverage < 0 || e.Coverage > 1 {
		return errors.New("invalid forecast coverage")
	}
	switch e.Quality {
	case "unknown", "cold_start", "learning", "ready":
	default:
		return errors.New("invalid forecast quality")
	}
	if e.Uncertainty != "unavailable" && e.Uncertainty != "provisional" {
		return errors.New("forecast uncertainty is not supported")
	}
	for _, p := range []*float64{e.PointW, e.LowerW, e.UpperW} {
		if p != nil && (!finite(*p) || *p < 0) {
			return errors.New("forecast power must be finite and nonnegative")
		}
	}
	if !e.Known {
		if e.PointW != nil || e.LowerW != nil || e.UpperW != nil || e.Uncertainty != "unavailable" || (e.Quality != "unknown" && e.Quality != "cold_start") {
			return errors.New("unknown forecast contains a point or claimed uncertainty")
		}
		return nil
	}
	if e.PointW == nil || e.Quality == "unknown" {
		return errors.New("known forecast has no point")
	}
	if (e.LowerW == nil) != (e.UpperW == nil) {
		return errors.New("incomplete forecast bounds")
	}
	if e.Uncertainty == "provisional" && e.LowerW == nil {
		return errors.New("provisional forecast lacks bounds")
	}
	if e.LowerW != nil && (*e.LowerW > *e.PointW || *e.PointW > *e.UpperW || e.Uncertainty != "provisional") {
		return errors.New("invalid forecast bounds")
	}
	return nil
}
