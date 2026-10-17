package appproto

import (
	"maps"
	"math"
	"regexp"
	"slices"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// What an agent may put in an op, from the registry's agent_args, checked as
// a cmd or an agent.request arrives. The box refuses any other key, a value
// of another kind and one outside its range, rather than passing it to a
// handler that would read it.

func badArg(op, arg string, value any) *ErrorBody {
	return &ErrorBody{Code: ErrUnknownOp, Args: map[string]any{"op": op, "arg": arg, "value": value}}
}

// agentArgs checks an agent's args for op against the registry and the
// grant's caps, and returns them with every agent id resolved to the box's
// own name. The caller passes the resolved args on; the agent's stay as
// sent for its own view.
func (h *Handler) agentArgs(op string, args map[string]any, caps protocol.Caps) (map[string]any, *ErrorBody) {
	specs, ok := protocol.RegistryAgentArgs[op]
	if !ok {
		return nil, &ErrorBody{Code: protocol.ErrOpNotGranted, Args: map[string]any{"op": op}}
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		i := slices.IndexFunc(specs, func(a protocol.AgentArg) bool { return a.Name == key })
		if i < 0 {
			return nil, badArg(op, key, nil)
		}
		resolved, refusal := h.agentArg(op, specs[i], value, caps)
		if refusal != nil {
			return nil, refusal
		}
		out[key] = resolved
	}
	for _, a := range specs {
		if _, present := args[a.Name]; a.Required && !present {
			return nil, badArg(op, a.Name, nil)
		}
	}
	return out, nil
}

func (h *Handler) agentArg(op string, a protocol.AgentArg, value any, caps protocol.Caps) (any, *ErrorBody) {
	switch a.Kind {
	case "bool":
		if _, ok := value.(bool); !ok {
			return nil, badArg(op, a.Name, value)
		}
		return value, nil
	case "enum":
		s, ok := value.(string)
		if !ok || !slices.Contains(a.Values, s) {
			return nil, badArg(op, a.Name, value)
		}
		return s, nil
	case "mode":
		s, ok := value.(string)
		if !ok || !control.IsValidMode(control.Mode(s)) {
			return nil, badArg(op, a.Name, value)
		}
		return s, nil
	case "loadpoint":
		s, ok := value.(string)
		if !ok {
			return nil, badArg(op, a.Name, value)
		}
		id, found := h.resolveLoadpoint(s)
		if !found {
			return nil, badArg(op, a.Name, s)
		}
		return id, nil
	case "car":
		s, ok := value.(string)
		if !ok {
			return nil, badArg(op, a.Name, value)
		}
		id, found := h.resolveCar(s)
		if !found {
			return nil, badArg(op, a.Name, s)
		}
		return id, nil
	case "number", "power", "duration", "ratio":
		n, ok := argNum(map[string]any{"v": value}, "v")
		if !ok || (a.Min != nil && n < *a.Min) || (a.Max != nil && n > *a.Max) {
			return nil, badArg(op, a.Name, value)
		}
		if a.Kind == "duration" && n != math.Trunc(n) {
			return nil, badArg(op, a.Name, value)
		}
		var limit uint64
		switch a.Cap {
		case "holdS":
			limit = caps.HoldS
		case "maxW":
			limit = caps.MaxW
		}
		// maxW holds in either direction.
		if a.Cap != "" && math.Abs(n) > float64(limit) {
			return nil, capExceeded(a.Cap, limit)
		}
		return value, nil
	}
	// A kind this box does not know is one it cannot check.
	return nil, badArg(op, a.Name, value)
}

// opArgs runs op's own check of its args, the one its handler runs before
// it takes a cmd. An agent.request runs it as it arrives, so an owner is
// never asked to approve something that cannot run. An op without a check
// here is one the box cannot judge ahead, and no owner is asked about it.
func (h *Handler) opArgs(op string, args map[string]any) *ErrorBody {
	switch op {
	case OpSetMode:
		_, bad := modeArg(op, args)
		return bad
	case OpPlannerPrefsSet:
		if h.cfg.PlannerPrefs == nil {
			return &ErrorBody{Code: ErrUnavailable, Retryable: ErrorRetryable[ErrUnavailable], Args: map[string]any{"op": op}}
		}
		_, _, bad := plannerPrefsArgs(op, args)
		return bad
	case OpLoadpointSoCSet:
		if _, _, bad := h.loadpointArg(op, args); bad != nil {
			return bad
		}
		_, bad := socArg(op, args)
		return bad
	case OpLoadpointSurplusOnlySet:
		if _, _, bad := h.loadpointArg(op, args); bad != nil {
			return bad
		}
		_, bad := surplusOnlyArg(op, args)
		return bad
	case OpCarGoalSet, OpCarSoCSet:
		if h.cfg.Cars == nil {
			return &ErrorBody{Code: ErrUnavailable, Retryable: ErrorRetryable[ErrUnavailable],
				Args: map[string]any{"op": op, "subsystem": "cars"}}
		}
		return carIntentArgs(op, args)
	}
	return &ErrorBody{Code: protocol.ErrOpNotGranted, Args: map[string]any{"op": op}}
}

// agentLoadpointID is a charger's id as agents see it.
func (h *Handler) agentLoadpointID(id string) string {
	return cloudwire.LoadpointID(h.cfg.AgentKey, id)
}

// agentCarID is a car's id as agents see it: the car class's id under the
// agent key. Empty stays empty.
func (h *Handler) agentCarID(id string) string {
	if id == "" {
		return ""
	}
	return cloudwire.CarID(h.cfg.AgentKey, id)
}

// resolveCar finds the car behind an agent's id.
func (h *Handler) resolveCar(agentID string) (string, bool) {
	if h.cfg.Cars == nil {
		return "", false
	}
	for _, c := range h.cfg.Cars.Cars() {
		if h.agentCarID(c.ID) == agentID {
			return c.ID, true
		}
	}
	return "", false
}

// resolveLoadpoint finds the charger behind an agent's id.
func (h *Handler) resolveLoadpoint(agentID string) (string, bool) {
	if h.cfg.Loadpoints == nil {
		return "", false
	}
	for _, r := range h.cfg.Loadpoints.Readings() {
		if h.agentLoadpointID(r.ID) == agentID {
			return r.ID, true
		}
	}
	return "", false
}

// ArgNames names each charger and car in a request as the owner's view
// holds it, by the box's own id, with what the box calls it: a car by its
// name, a charger by its id, which is its name on the box. An id the box no
// longer knows gets none. Only the box page reads these; answers to agents
// name nothing, since FTW Cloud reads them.
func (h *Handler) ArgNames(req protocol.Request) map[string]string {
	var out map[string]string
	for _, a := range protocol.RegistryAgentArgs[req.Op] {
		id, _ := req.Args[a.Name].(string)
		if id == "" {
			continue
		}
		var name string
		switch a.Kind {
		case "loadpoint":
			if h.cfg.Loadpoints != nil && h.cfg.Loadpoints.Exists(id) {
				name = id
			}
		case "car":
			if h.cfg.Cars != nil {
				for _, c := range h.cfg.Cars.Cars() {
					if c.ID == id {
						name = c.Name
						break
					}
				}
			}
		}
		if name == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[a.Name] = name
	}
	return out
}

// holdRules are the hold rules beyond the args: an agent's hold always ends,
// and an agent never touches a hold someone else set.
func (h *Handler) holdRules(cmd Cmd) *ErrorBody {
	if cmd.Op != OpLoadpointHold {
		return nil
	}
	clear, _ := cmd.Args["clear"].(bool)
	if !clear {
		if _, ok := cmd.Args["hold_s"]; !ok {
			// No hold_s is the persistent operator hold, which no agent
			// may set.
			return badArg(cmd.Op, "hold_s", nil)
		}
	}
	id, _ := cmd.Args["id"].(string)
	if h.cfg.Loadpoints == nil || id == "" {
		return nil
	}
	held, ok := h.cfg.Loadpoints.ObservedHold(id, h.cfg.Clock.Now())
	if !ok {
		if clear {
			// Nothing of its own to release.
			return &ErrorBody{Code: protocol.ErrNotFound, Args: map[string]any{"op": cmd.Op}}
		}
		return nil
	}
	if held.SetBy != h.cfg.Caller.Subject {
		return &ErrorBody{Code: protocol.ErrNeedsOwner, Args: map[string]any{"op": cmd.Op, "reason": "hold"}}
	}
	return nil
}

// withArgs is cmd with its args replaced.
func withArgs(cmd Cmd, args map[string]any) Cmd {
	cmd.Args = maps.Clone(args)
	return cmd
}

// safeArgKeys are the error args an agent may read. Any other key goes: a
// handler written for a phone may name a device, and FTW Cloud reads every
// answer an agent gets.
var safeArgKeys = map[string]bool{
	"op": true, "arg": true, "value": true, "cap": true, "max": true, "min": true, "use": true,
	"needScope": true, "role": true, "reason": true, "field": true, "missing": true, "t": true,
	"expected": true, "actual": true, "fid": true, "guards": true, "subsystem": true,
	"dispatchBlockedBy": true, "id": true, "notValidAfterMs": true, "uptimeMs": true, "boxMs": true,
	"agentId": true, "reqId": true,
}

// forAgent is error args as an agent may read them: names become agent ids,
// and anything else that could name something goes.
func (h *Handler) forAgent(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if !safeArgKeys[k] {
			continue
		}
		switch k {
		case "dispatchBlockedBy":
			names, _ := v.([]string)
			ids := make([]string, 0, len(names))
			for _, n := range names {
				ids = append(ids, h.srcID(n))
			}
			out[k] = ids
			continue
		case "id":
			if s, ok := v.(string); ok {
				out[k] = h.agentLoadpointID(s)
			}
			continue
		case "value":
			if s, ok := v.(string); ok {
				switch {
				case args["arg"] == "id" && (args["op"] == OpCarGoalSet || args["op"] == OpCarSoCSet):
					out[k] = h.agentCarID(s)
				case args["arg"] == "id" || args["arg"] == "charger_id":
					out[k] = h.agentLoadpointID(s)
				case codeWord.MatchString(s):
					out[k] = s
				}
				continue
			}
		}
		if s, ok := v.(string); ok && !codeWord.MatchString(s) {
			continue
		}
		out[k] = v
	}
	return out
}

// forAgentResult is a command's outcome as an agent may read it: a car by
// its agent id with its level and goal, a charger's car by agent ids.
func (h *Handler) forAgentResult(res CmdResult) CmdResult {
	if res.Error != nil {
		e := *res.Error
		e.Args = h.forAgent(e.Args)
		res.Error = &e
	}
	if res.Car != nil {
		c := briefCar(*res.Car)
		c.ID = h.agentCarID(c.ID)
		res.Car = &c
	}
	if res.Charger != nil {
		cc := *res.Charger
		cc.CarID, cc.CarCorrectedFrom = h.agentCarID(cc.CarID), h.agentCarID(cc.CarCorrectedFrom)
		if cc.Choose != nil {
			choose := make([]string, 0, len(cc.Choose))
			for _, id := range cc.Choose {
				choose = append(choose, h.agentCarID(id))
			}
			cc.Choose = choose
		}
		res.Charger = &cc
	}
	return res
}

var codeWord = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,48}$`)
