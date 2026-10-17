package appproto

import (
	"errors"

	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/units"
	"github.com/srcfl/ftw/go/protocol"
)

// carOp runs the car ops (docs/cars.md). Arguments follow the box's
// convention: snake_case, id for what the op acts on, a level as a fraction.
// Each reads its arguments and refuses a bad one before taking the command,
// and reports the car or the charger's car as the box holds it afterwards.
func (h *Handler) carOp(cmd Cmd, uptimeMs int64) error {
	c := h.cfg.Cars
	if c == nil {
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID, State: CmdRejected,
			Error: &ErrorBody{Code: ErrUnavailable, Retryable: ErrorRetryable[ErrUnavailable],
				Args: map[string]any{"subsystem": "cars"}},
		})
	}
	id, _ := cmd.Args["id"].(string)
	charger, _ := cmd.Args["charger_id"].(string)

	var act func() (CmdResult, error)
	switch cmd.Op {
	case OpCarGoalSet:
		if (id == "") == (charger == "") {
			return h.rejectArg(cmd, "id", cmd.Args["id"])
		}
		goal, clear, bad := goalArgs(cmd.Args)
		if bad != "" {
			return h.rejectArg(cmd, bad, cmd.Args[bad])
		}
		surplus, bad := optBool(cmd.Args, "surplus_only")
		if bad != "" {
			return h.rejectArg(cmd, bad, cmd.Args[bad])
		}
		act = func() (CmdResult, error) {
			if charger != "" {
				held, err := c.SetSessionGoal(charger, goal, clear, surplus)
				return chargerResult(cmd, held, err)
			}
			car, err := c.SetGoal(id, goal, clear, surplus)
			return carResult(cmd, car, err)
		}
	case OpCarSoCSet:
		if (id == "") == (charger == "") {
			return h.rejectArg(cmd, "id", cmd.Args["id"])
		}
		soc, ok := argNum(cmd.Args, "soc")
		if !ok || soc < 0 || soc > 1 {
			return h.rejectArg(cmd, "soc", cmd.Args["soc"])
		}
		act = func() (CmdResult, error) {
			if charger != "" {
				held, err := c.SetSessionLevel(charger, soc)
				res, err := chargerResult(cmd, held, err)
				if res.State == CmdApplied && h.cfg.Loadpoints != nil {
					if level, ok := h.cfg.Loadpoints.ObservedSoC(charger); ok {
						res.Observed = &Observed{Value: level, Src: ObservedSrcCore, UptimeMs: h.cfg.Clock.UptimeMs()}
					}
				}
				return res, err
			}
			car, err := c.SetLevel(id, soc)
			res, err := carResult(cmd, car, err)
			if res.Car != nil && res.Car.SocPermille != nil {
				res.Observed = &Observed{Value: float64(*res.Car.SocPermille) / 1000, Src: ObservedSrcCore, UptimeMs: h.cfg.Clock.UptimeMs()}
			}
			return res, err
		}
	case OpSessionCarSet:
		session, _ := cmd.Args["session"].(string)
		car, ok := cmd.Args["car_id"].(string)
		switch {
		case id == "":
			return h.rejectArg(cmd, "id", cmd.Args["id"])
		case session == "":
			return h.rejectArg(cmd, "session", cmd.Args["session"])
		case !ok:
			return h.rejectArg(cmd, "car_id", cmd.Args["car_id"])
		}
		act = func() (CmdResult, error) {
			held, err := c.SetSessionCar(id, session, car)
			return chargerResult(cmd, held, err)
		}
	case OpCarAdd:
		car, bad := carArgs(cmd.Args)
		if bad != "" {
			return h.rejectArg(cmd, bad, cmd.Args[bad])
		}
		act = func() (CmdResult, error) {
			added, err := c.Add(car)
			return carResult(cmd, added, err)
		}
	case OpCarUpdate:
		u, bad := carUpdateArgs(cmd.Args)
		if bad != "" {
			return h.rejectArg(cmd, bad, cmd.Args[bad])
		}
		act = func() (CmdResult, error) {
			car, err := c.UpdateCar(id, u)
			return carResult(cmd, car, err)
		}
	case OpCarRemove:
		if id == "" {
			return h.rejectArg(cmd, "id", cmd.Args["id"])
		}
		act = func() (CmdResult, error) {
			if err := c.Remove(id); err != nil {
				return carResult(cmd, protocol.Car{}, err)
			}
			return CmdResult{CmdID: cmd.CmdID, State: CmdApplied}, nil
		}
	}

	if took, err := h.acceptCmd(cmd, uptimeMs); !took || err != nil {
		return err
	}
	res, err := act()
	if err != nil {
		// A save the box could not confirm: it may hold the change or not.
		return h.settleAndReport(cmd.CmdID, CmdResult{
			CmdID: cmd.CmdID, State: CmdUnconfirmed,
			Error: &ErrorBody{Code: ErrUnavailable, Retryable: ErrorRetryable[ErrUnavailable],
				Args: map[string]any{"op": cmd.Op, "reason": "persistence_unconfirmed"}},
		})
	}
	return h.settleAndReport(cmd.CmdID, res)
}

// carResult reports a car op: the car read back, or a refusal. err is
// returned only for a save that failed.
func carResult(cmd Cmd, car protocol.Car, err error) (CmdResult, error) {
	if err != nil {
		if res, ok := carRefusal(cmd, err); ok {
			return res, nil
		}
		return CmdResult{}, err
	}
	brief := briefCar(car)
	return CmdResult{CmdID: cmd.CmdID, State: CmdApplied, Car: &brief}, nil
}

// briefCar is the car a cmd.result carries: its id, level, goal and surplus
// choice. A result rides lane 0, whose smallest bucket is 256 B; the app
// reads the rest from GET /api/cars.
func briefCar(c protocol.Car) protocol.Car {
	return protocol.Car{ID: c.ID, SocPermille: c.SocPermille, SocSource: c.SocSource, SocAgeS: c.SocAgeS,
		EnergySinceWh: c.EnergySinceWh, SocEstimate: c.SocEstimate, ChargeLimitPermille: c.ChargeLimitPermille,
		Goal: c.Goal, SurplusOnly: c.SurplusOnly}
}

func chargerResult(cmd Cmd, held protocol.ChargerCar, err error) (CmdResult, error) {
	if err != nil {
		if res, ok := carRefusal(cmd, err); ok {
			return res, nil
		}
		return CmdResult{}, err
	}
	return CmdResult{CmdID: cmd.CmdID, State: CmdApplied, Charger: &held}, nil
}

// carRefusal names why, in machine form, after the ack, as the loadpoint ops
// do (docs/cars.md, Ops). ok is false for an error that is not a refusal.
func carRefusal(cmd Cmd, err error) (CmdResult, bool) {
	code, reason, ok := CarRefusal(err)
	if !ok {
		return CmdResult{}, false
	}
	return CmdResult{
		CmdID: cmd.CmdID, State: CmdRejected,
		Error: &ErrorBody{Code: code, Retryable: ErrorRetryable[code], Args: map[string]any{"op": cmd.Op, "reason": reason}},
	}, true
}

// CarRefusal is the code and reason a car op's error is refused with. The
// box page's car routes answer with the same pair. ok is false for an error
// that is not a refusal, such as a save that failed.
func CarRefusal(err error) (code, reason string, ok bool) {
	switch {
	case errors.Is(err, cars.ErrUnknownCar):
		return ErrNotFound, "car", true
	case errors.Is(err, cars.ErrUnknownCharger):
		return ErrNotFound, "charger", true
	case errors.Is(err, cars.ErrSessionEnded):
		return ErrPrecondition, "session", true
	case errors.Is(err, cars.ErrUnplugged):
		return ErrUnavailable, "unplugged", true
	case errors.Is(err, cars.ErrDuplicate):
		return ErrDuplicate, "duplicate", true
	case errors.Is(err, cars.ErrElsewhere):
		return ErrPrecondition, "elsewhere", true
	case errors.Is(err, cars.ErrTooMany):
		return protocol.ErrCapExceeded, "cars", true
	case errors.Is(err, cars.ErrInvalid):
		return ErrPrecondition, "invalid", true
	}
	return "", "", false
}

// carIntentArgs checks car.goal.set's and car.soc.set's arguments as carOp
// reads them, for an agent.request before an owner is asked.
func carIntentArgs(op string, args map[string]any) *ErrorBody {
	id, _ := args["id"].(string)
	charger, _ := args["charger_id"].(string)
	if (id == "") == (charger == "") {
		return badArg(op, "id", args["id"])
	}
	switch op {
	case OpCarGoalSet:
		if _, _, bad := goalArgs(args); bad != "" {
			return badArg(op, bad, args[bad])
		}
		if _, bad := optBool(args, "surplus_only"); bad != "" {
			return badArg(op, bad, args[bad])
		}
	case OpCarSoCSet:
		if soc, ok := argNum(args, "soc"); !ok || soc < 0 || soc > 1 {
			return badArg(op, "soc", args["soc"])
		}
	}
	return nil
}

// goalArgs reads car.goal.set's goal, flat as the box's other ops take
// theirs: target, a fraction, and ready_by_min, with days and
// finish_at_limit, replace it; clear: true removes it. With none of them the
// goal stays as it was.
func goalArgs(args map[string]any) (goal *protocol.CarGoal, clear bool, bad string) {
	if v, present := args["clear"]; present {
		b, ok := v.(bool)
		if !ok {
			return nil, false, "clear"
		}
		clear = b
	}
	set := false
	for _, k := range []string{"target", "ready_by_min", "days", "finish_at_limit"} {
		if _, present := args[k]; present {
			set = true
		}
	}
	if !set {
		return nil, clear, ""
	}
	if clear {
		return nil, false, "clear"
	}
	target, ok := argNum(args, "target")
	if !ok || target < 0 || target > 1 {
		return nil, false, "target"
	}
	g := protocol.CarGoal{TargetPermille: units.PermilleFromFraction(target)}
	if g.ReadyByMin, ok = argInt(args, "ready_by_min"); !ok || g.ReadyByMin < 0 || g.ReadyByMin >= 24*60 {
		return nil, false, "ready_by_min"
	}
	if _, present := args["days"]; present {
		days, ok := argInt(args, "days")
		if !ok || days < 0 || days > 0x7F {
			return nil, false, "days"
		}
		g.Days = uint8(days)
	}
	if v, present := args["finish_at_limit"]; present {
		b, ok := v.(bool)
		if !ok {
			return nil, false, "finish_at_limit"
		}
		g.FinishAtLimit = b
	}
	return &g, false, ""
}

// goalArg reads car.add's goal, a field of the car: absent is none, and a
// map of target_permille, ready_by_min, days and finish_at_limit sets it.
func goalArg(args map[string]any) (goal *protocol.CarGoal, clear bool, bad string) {
	raw, present := args["goal"]
	if !present {
		return nil, false, ""
	}
	if raw == nil {
		return nil, true, ""
	}
	m := stringKeys(raw)
	if m == nil {
		return nil, false, "goal"
	}
	g := protocol.CarGoal{}
	var ok bool
	if g.TargetPermille, ok = argInt(m, "target_permille"); !ok || g.TargetPermille < 0 || g.TargetPermille > 1000 {
		return nil, false, "goal"
	}
	if g.ReadyByMin, ok = argInt(m, "ready_by_min"); !ok || g.ReadyByMin < 0 || g.ReadyByMin >= 24*60 {
		return nil, false, "goal"
	}
	if _, present := m["days"]; present {
		days, ok := argInt(m, "days")
		if !ok || days < 0 || days > 0x7F {
			return nil, false, "goal"
		}
		g.Days = uint8(days)
	}
	if v, present := m["finish_at_limit"]; present {
		b, ok := v.(bool)
		if !ok {
			return nil, false, "goal"
		}
		g.FinishAtLimit = b
	}
	return &g, false, ""
}

// stringKeys reads a CBOR map, which may arrive with any-typed keys.
func stringKeys(raw any) map[string]any {
	switch m := raw.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, v := range m {
			if s, ok := k.(string); ok {
				out[s] = v
			}
		}
		return out
	}
	return nil
}

func optBool(args map[string]any, k string) (*bool, string) {
	raw, present := args[k]
	if !present {
		return nil, ""
	}
	b, ok := raw.(bool)
	if !ok {
		return nil, k
	}
	return &b, ""
}

// CarAddArgs and CarUpdateArgs read car.add's and car.update's arguments as
// the ops do, for the box page's routes. bad names an argument they refuse.
func CarAddArgs(args map[string]any) (car protocol.Car, bad string) { return carArgs(args) }

func CarUpdateArgs(args map[string]any) (u cars.Update, bad string) { return carUpdateArgs(args) }

// carArgs reads car.add: id, which the owner's app mints, and the car's own
// fields without the level.
func carArgs(args map[string]any) (protocol.Car, string) {
	var c protocol.Car
	if c.ID, _ = args["id"].(string); c.ID == "" {
		return c, "id"
	}
	u, bad := carUpdateArgs(args)
	if bad != "" {
		return c, bad
	}
	if u.Name != nil {
		c.Name = *u.Name
	}
	if u.Make != nil {
		c.Make = *u.Make
	}
	if u.VIN != nil {
		c.VIN = *u.VIN
	}
	if u.CapacityWh != nil {
		c.CapacityWh = int64(*u.CapacityWh)
	}
	if u.MaxW != nil {
		c.MaxW = int64(*u.MaxW)
	}
	if u.Phases != nil {
		c.Phases = int64(*u.Phases)
	}
	if u.Identifiers != nil {
		c.Identifiers = *u.Identifiers
	}
	if u.Sources != nil {
		c.Sources = *u.Sources
	}
	surplus, bad := optBool(args, "surplus_only")
	if bad != "" {
		return c, bad
	}
	if surplus != nil {
		c.SurplusOnly = *surplus
	}
	goal, _, bad := goalArg(args)
	if bad != "" {
		return c, bad
	}
	c.Goal = goal
	return c, ""
}

// carUpdateArgs reads car.update: id, new_id for a car the box minted, and
// the fields that change.
func carUpdateArgs(args map[string]any) (cars.Update, string) {
	var u cars.Update
	if id, _ := args["id"].(string); id == "" {
		return u, "id"
	}
	str := func(k string) (*string, string) {
		raw, present := args[k]
		if !present {
			return nil, ""
		}
		s, ok := raw.(string)
		if !ok {
			return nil, k
		}
		return &s, ""
	}
	var bad string
	for _, f := range []struct {
		k   string
		dst **string
	}{{"new_id", &u.NewID}, {"name", &u.Name}, {"make", &u.Make}, {"vin", &u.VIN}} {
		if *f.dst, bad = str(f.k); bad != "" {
			return u, bad
		}
	}
	num := func(k string) (*float64, string) {
		if _, present := args[k]; !present {
			return nil, ""
		}
		v, ok := argInt(args, k)
		if !ok || v < 0 {
			return nil, k
		}
		f := float64(v)
		return &f, ""
	}
	if u.CapacityWh, bad = num("capacity_wh"); bad != "" {
		return u, bad
	}
	if u.MaxW, bad = num("max_w"); bad != "" {
		return u, bad
	}
	if p, b := num("phases"); b != "" {
		return u, b
	} else if p != nil {
		phases := int(*p)
		u.Phases = &phases
	}
	for _, k := range []string{"identifiers", "sources"} {
		if _, present := args[k]; !present {
			continue
		}
		list, bad := stringList(args, k)
		if bad != "" {
			return u, bad
		}
		if list == nil {
			list = []string{}
		}
		if k == "identifiers" {
			u.Identifiers = &list
		} else {
			u.Sources = &list
		}
	}
	return u, ""
}

func stringList(args map[string]any, k string) ([]string, string) {
	raw, present := args[k]
	if !present || raw == nil {
		return nil, ""
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, k
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, k
		}
		out = append(out, s)
	}
	return out, ""
}
