"""Record paired read-only FTW observations around Easee starts."""
from collections import deque
from datetime import datetime
import json
import logging
from logging.handlers import RotatingFileHandler
import sys
import time
from urllib.request import urlopen

BASE = "http://127.0.0.1:8080"
DRIVER = "easee-cloud"
INTERVAL = 5
PREHISTORY_MS = 30_000
WINDOW = 20 * 60
PILOT_MAX_AGE_MS = 90_000
DRIVER_LOG_LIMIT = 160
TRACE_EVENTS = {"phase_write", "phase_result", "charge_start", "current_request",
                "current_write", "current_reassert", "resume_request", "resume_result", "end"}
VEHICLE_REFRESH_MS = 60_000
NO_DRAW_MAX_W = 1


def read(path):
    with urlopen(BASE + path, timeout=3) as response:
        return json.load(response)


def select(data, fields):
    return {key: data[key] for key in fields.split() if key in data}


def loadpoint():
    return next((row for row in read("/api/loadpoints").get("loadpoints", [])
                 if row.get("driver_name") == DRIVER), {})


def snapshot(lp):
    ev = read(f"/api/ev/status?driver={DRIVER}")
    diag = ev.get("charger_diagnostics", {})
    # This setup uses Pixii as the grid meter. Without it, grid phase currents are missing.
    meter = read("/api/drivers/pixii")
    return {
        "recorded_at_ms": time.time_ns() // 1_000_000,
        "loadpoint": select(lp, "plugged_in phases voltage_v power_updated_at_ms power_unavailable commanded_reason commanded_w current_power_w"),
        "feedback": select(lp.get("control_feedback") or {}, "reason requested_a sent_w actual_w command_at_ms command_id observed_at_ms readings_fresh"),
        "easee": select(ev, "updated connected charging op_mode state_label request_active pilot_confirmed_at_ms w control_power_observed_at control_power_available dynamic_limit_a pilot_a pilot_observed_at reason_no_current reason_no_current_label reason_observed_at state_observed_at"),
        "allocation": select(diag, "allocated_l1_a allocated_l2_a allocated_l3_a output_phase phase_mode pilot_mode pilot_mode_observed_at start_trace_ms order start_observations"),
        "pixii": {m["name"]: select(m, "value updated_at") for m in meter.get("metrics", [])
                  if m["name"] in {"meter_l1_a", "meter_l2_a", "meter_l3_a"}},
    }


def source_ms(value):
    if not value:
        return None
    try:
        stamp = datetime.fromisoformat(value.replace("Z", "+00:00"))
        return int(stamp.timestamp() * 1000) if stamp.tzinfo else None
    except ValueError:
        return None


def start_sample(row, baseline, previous_mode):
    ev = row["easee"]
    observed = source_ms(ev.get("control_power_observed_at"))
    available = ev.get("control_power_available") and observed is not None
    fresh = available and baseline <= observed <= row["recorded_at_ms"]
    row["power_sample"] = {
        "status": "fresh" if fresh else "stale" if available else "missing",
        "source_at": ev.get("control_power_observed_at"),
        "recorded_at_ms": row["recorded_at_ms"],
        "age_ms": row["recorded_at_ms"] - observed if observed is not None else None,
        "confirmed_w": ev.get("w") if fresh else None,
    }
    mode = row["allocation"].get("pilot_mode")
    if mode is not None and mode != previous_mode:
        row["pilot_transition"] = {
            "from": previous_mode, "to": mode,
            "source_at": row["allocation"].get("pilot_mode_observed_at"),
            "recorded_at_ms": row["recorded_at_ms"],
            "op_mode": ev.get("op_mode"),
            "reason_no_current": ev.get("reason_no_current"),
        }
    return mode or previous_mode


def pilot_sample(row, baseline):
    ev = row["easee"]
    amps = ev.get("pilot_a")
    source_at = ev.get("pilot_observed_at")
    observed = source_ms(source_at)
    now = row["recorded_at_ms"]
    age = now - observed if observed is not None else None
    if amps is None or age is None:
        status = "missing"
    elif age < 0:
        status = "future"
    elif observed < baseline or age > PILOT_MAX_AGE_MS:
        status = "stale"
    else:
        status = "fresh"
    return {
        "status": status,
        "source": "easee_status",
        "source_at": source_at,
        "age_ms": age,
        "reported_a": amps,
        "confirmed_a": amps if status == "fresh" else None,
    }


def timestamped_sample(value, source_at, now, baseline, max_age_ms):
    observed = source_ms(source_at)
    age = now - observed if observed is not None else None
    if value is None or age is None:
        status = "missing"
    elif age < 0:
        status = "future"
    elif observed < baseline or age > max_age_ms:
        status = "stale"
    else:
        status = "fresh"
    return {
        "status": status,
        "value": value,
        "source_at": source_at,
        "age_ms": age,
    }


def paired_evidence(row, baseline, order):
    now = row["recorded_at_ms"]
    ev = row["easee"]
    allocation = row["allocation"]
    observations = allocation.get("start_observations") or {}
    phases = {}
    for phase in ("l1", "l2", "l3"):
        key = f"allocated_{phase}_a"
        observation = observations.get(key) or {}
        sample = timestamped_sample(
            observation.get("value", allocation.get(key)),
            observation.get("source_at"), now, baseline, 15_000,
        )
        sample["received_at_ms"] = observation.get("confirmed_at_ms")
        phases[phase] = sample
    grid = {}
    for phase in phases:
        metric = row["pixii"].get(f"meter_{phase}_a") or {}
        grid[phase] = timestamped_sample(
            metric.get("value"), metric.get("updated_at"), now, 0, 15_000,
        )
    pilot = timestamped_sample(ev.get("pilot_a"), ev.get("pilot_observed_at"),
                               now, baseline, 15_000)
    power = timestamped_sample(
        ev.get("w") if ev.get("control_power_available") else None,
        ev.get("control_power_observed_at"), now, baseline, 15_000,
    )
    if (order.get("watts") or 0) <= 0:
        state = "no_positive_order"
    elif power["status"] != "fresh":
        state = "power_unverified"
    elif power["value"] > NO_DRAW_MAX_W:
        state = "power_observed"
    elif pilot["status"] != "fresh":
        state = "no_draw_pilot_unverified"
    elif pilot["value"] == 0:
        state = "zero_pilot_no_draw"
    else:
        state = "positive_pilot_no_draw"
    return {
        "state": state,
        "sent_order": order,
        "pilot": pilot,
        "power": power,
        "allocation_a": phases,
        "grid_a": grid,
        "charger_reason": timestamped_sample(
            ev.get("reason_no_current"), ev.get("reason_observed_at"),
            now, baseline, 15_000,
        ),
        "driver_order": allocation.get("order"),
    }


def sent_order(row):
    feedback = row["feedback"]
    loadpoint = row["loadpoint"]
    watts = feedback.get("sent_w")
    if watts is None:
        watts = loadpoint.get("commanded_w")
    return {
        "watts": watts,
        "reason": feedback.get("reason") or loadpoint.get("commanded_reason"),
        "command_at_ms": feedback.get("command_at_ms"),
        "command_id": feedback.get("command_id"),
    }


def order_change(previous, current):
    if previous and (previous["watts"], previous["reason"]) == (current["watts"], current["reason"]):
        return None
    return {"previous": previous, "current": current}


def driver_events(entries, since_ms, seen):
    prefix = "Easee phase trace "
    for entry in entries:
        stamp = entry.get("ts")
        observed = source_ms(stamp)
        if observed is None or observed < since_ms:
            continue
        message = entry.get("msg", "")
        if message.startswith(prefix):
            try:
                trace = json.loads(message[len(prefix):])
            except ValueError:
                continue
            if not isinstance(trace, dict) or trace.get("event") not in TRACE_EVENTS:
                continue
            event = select(trace, "event trace_ms recorded_at_ms requested_a write_a cap_a accepted reported_a measured_a over_order method reason previous_mode requested_mode command_id power_w command_reason")
            event["source"] = "easee_driver"
        elif message == "loadpoint resume acknowledged; waiting for measured charge":
            event = {"source": "ftw_core", "event": "resume_result", "accepted": True}
        elif message.startswith("Easee: re-sent current limit as a command: ") and message.endswith(" A"):
            amps = message[len("Easee: re-sent current limit as a command: "):-2]
            if not amps.isdecimal():
                continue
            event = {"source": "easee_driver", "event": "command_ack_summary",
                     "requested_a": int(amps), "accepted": True, "method": "command"}
        else:
            continue
        key = (stamp, message)
        if key not in seen:
            seen[key] = observed
            yield {"at": stamp, "at_ms": observed, **event}


def recent_driver_events(now, seen):
    # A short overlap catches buffered log delivery while bounding deduplication.
    since = now - 30_000
    for key, stamp in list(seen.items()):
        if stamp < since:
            del seen[key]
    try:
        payload = read(f"/api/drivers/{DRIVER}/logs?limit={DRIVER_LOG_LIMIT}")
    except (OSError, ValueError):
        return None
    return list(driver_events(payload.get("entries", []), since, seen))


def annotate_start(row, baseline, previous_order, events):
    row["pilot_sample"] = pilot_sample(row, baseline)
    order = sent_order(row)
    change = order_change(previous_order, order)
    if change is not None:
        row["order_change"] = change
    if events is None:
        row["resume_log_unavailable"] = True
    elif events:
        row["driver_events"] = events
        resumed = [event for event in events if event["event"] == "resume_result"]
        if resumed:
            row["resume_results"] = resumed
    return order


def vehicle_context(lp):
    context = select(lp, "current_soc soc_source soc_age_s")
    context["vehicle_charge_limit_status"] = "unavailable"
    if not lp.get("car_id"):
        return context
    try:
        car = next((car for car in read("/api/cars").get("cars", [])
                    if car.get("id") == lp["car_id"]), {})
        context["vehicle_read_at_ms"] = time.time_ns() // 1_000_000
        context.update(select(car, "soc_permille soc_age_s soc_estimate"))
        if car:
            context["vehicle_soc_source"] = car.get("soc_source")
        for field in ("vehicle_charge_limit_pct", "charge_limit_pct", "charge_limit"):
            if car.get(field) is not None:
                context[field] = car[field]
                context["vehicle_charge_limit_status"] = "reported"
                return context
        if car.get("sources"):
            drivers = read("/api/status").get("drivers", {})
            for name in car["sources"]:
                source = drivers.get(name, {})
                if source.get("vehicle_charge_limit_pct") is not None:
                    context["vehicle_charge_limit_pct"] = source["vehicle_charge_limit_pct"]
                    context["vehicle_charge_limit_status"] = "reported"
                    context["charge_limit_source"] = name
                    context["charge_limit_source_stale"] = source.get("vehicle_stale")
                    break
    except (OSError, ValueError):
        context["vehicle_read_failed"] = True
    return context


def recent_charging_baseline(samples, start_ms):
    for sample in reversed(samples):
        ev = sample["easee"]
        observed = source_ms(ev.get("control_power_observed_at"))
        if (sample["loadpoint"].get("plugged_in") and ev.get("connected")
                and (sent_order(sample)["watts"] or 0) > 0
                and ev.get("charging") and ev.get("control_power_available")
                and (ev.get("w") or 0) >= 500 and observed is not None
                and start_ms - PREHISTORY_MS <= sample["recorded_at_ms"] <= start_ms
                and observed <= sample["recorded_at_ms"]):
            # An unchanged power value can retain its source timestamp for
            # the whole charge. Require a newer source sample within the
            # capture window; the old positive value is only context.
            return max(observed + 1, start_ms - PREHISTORY_MS)
    return None


def no_draw_evidence(row, order, stop_baseline=None):
    pilot = row["pilot_sample"]["confirmed_a"]
    power = row["power_sample"]
    ev = row["easee"]
    pilot_confirmed_at = ev.get("pilot_confirmed_at_ms")
    # The pilot value may be unchanged across the stop. Keep its source
    # timestamp and accept a separate driver confirmation during the episode.
    if (pilot is None and stop_baseline is not None and pilot_confirmed_at
            and 0 <= row["recorded_at_ms"] - pilot_confirmed_at <= PILOT_MAX_AGE_MS
            and source_ms(ev.get("pilot_observed_at")) is not None
            and source_ms(ev.get("pilot_observed_at")) <= pilot_confirmed_at):
        pilot = ev.get("pilot_a")
    if not (row["loadpoint"].get("plugged_in") and ev.get("connected") and
            (order["watts"] or 0) > 0 and pilot is not None and pilot > 0):
        return None
    confirmed_w = power["confirmed_w"]
    age_ms = power["age_ms"]
    if confirmed_w is None or age_ms is None or age_ms > PILOT_MAX_AGE_MS:
        status = "power_unconfirmed"
    elif abs(confirmed_w) <= NO_DRAW_MAX_W:
        status = "confirmed_no_draw"
    else:
        return None
    return {
        "status": status,
        "sent_w": order["watts"],
        "pilot_a": pilot,
        "pilot_confirmed_at_ms": pilot_confirmed_at,
        "pilot_source_at": row["pilot_sample"]["source_at"],
        "power_source_at": power["source_at"],
        "confirmed_w": confirmed_w if status == "confirmed_no_draw" else None,
        "charger": select(ev, "op_mode state_label state_observed_at request_active reason_no_current reason_no_current_label reason_observed_at"),
        "pilot_mode": select(row["allocation"], "pilot_mode pilot_mode_observed_at output_phase phase_mode"),
    }


def main():
    if sys.argv[1:] == ["--probe"]:
        lp = loadpoint()
        row = snapshot(lp)
        row["vehicle_at_start"] = vehicle_context(lp)
        start_sample(row, row["recorded_at_ms"], None)
        row["pilot_sample"] = pilot_sample(row, row["recorded_at_ms"] - PILOT_MAX_AGE_MS)
        row["sent_order"] = sent_order(row)
        row["paired_evidence"] = paired_evidence(row, 0, row["sent_order"])
        print(json.dumps(row, separators=(",", ":")))
        return
    log = logging.getLogger("ftw-easee-start")
    log.setLevel(logging.INFO)
    log.addHandler(RotatingFileHandler(sys.argv[1], maxBytes=1_048_576, backupCount=4))
    start_ms = None
    deadline = None
    eligible_before = False
    consecutive_errors = 0
    previous_mode = None
    first_positive = False
    previous_order = None
    seen_driver_events = {}
    phase_change = None
    last_no_draw_status = None
    last_vehicle_read = None
    prehistory = deque(maxlen=PREHISTORY_MS // (INTERVAL * 1000))
    stop_baseline = None
    while True:
        try:
            lp = loadpoint()
            feedback = lp.get("control_feedback") or {}
            requested = feedback.get("requested_a") or lp.get("commanded_w") or 0
            power = lp.get("current_power_w") or 0
            eligible = bool(lp.get("plugged_in") and requested > 0 and power < 100)
            now_ms = time.time_ns() // 1_000_000
            events = recent_driver_events(now_ms, seen_driver_events) if lp.get("plugged_in") or start_ms is not None else []
            phase_event = next((event for event in reversed(events or [])
                                if event["event"] == "phase_write"), None)
            while prehistory and prehistory[0]["recorded_at_ms"] < now_ms - PREHISTORY_MS:
                prehistory.popleft()
            if not lp.get("plugged_in"):
                prehistory.clear()
            if start_ms is None and (phase_event is not None or (eligible and not eligible_before)):
                start_ms = now_ms
                phase_change = None
                deadline = time.monotonic() + WINDOW
                previous_mode = None
                first_positive = False
                previous_order = None
                last_no_draw_status = None
                last_vehicle_read = None
                start_vehicle = vehicle_context(lp)
                baseline = min(feedback.get("command_at_ms") or start_ms, start_ms)
                stop_baseline = recent_charging_baseline(prehistory, start_ms) if phase_event is None else None
                if stop_baseline is not None:
                    baseline = stop_baseline
                for sample in prehistory:
                    log.info(json.dumps({**sample, "start_ms": start_ms, "prehistory": True}, separators=(",", ":")))
                prehistory.clear()
            if start_ms is None and lp.get("plugged_in") and (lp.get("commanded_w") or 0) > 0:
                prehistory.append(snapshot(lp))
            if start_ms is not None:
                if phase_event is not None:
                    phase_change = phase_event
                    first_positive = False
                    deadline = time.monotonic() + WINDOW
                row = snapshot(lp)
                if phase_change is not None:
                    row["phase_change"] = phase_change
                row["start_ms"] = start_ms
                row["vehicle_at_start"] = start_vehicle
                measurement_baseline = phase_change["at_ms"] if phase_change else baseline
                previous_mode = start_sample(row, measurement_baseline, previous_mode)
                pilot_baseline = phase_change["at_ms"] if phase_change else max(baseline, start_ms - INTERVAL * 1000)
                previous_order = annotate_start(row, pilot_baseline, previous_order, events)
                row["paired_evidence"] = paired_evidence(row, measurement_baseline, previous_order)
                evidence = no_draw_evidence(row, previous_order, stop_baseline if phase_change is None else None)
                if evidence is not None:
                    row["positive_pilot_no_draw"] = evidence
                    if evidence["status"] == "confirmed_no_draw":
                        row["paired_evidence"]["state"] = "positive_pilot_no_draw"
                    first = evidence["status"] != last_no_draw_status
                    if first or last_vehicle_read is None or row["recorded_at_ms"] - last_vehicle_read >= VEHICLE_REFRESH_MS:
                        evidence["vehicle"] = vehicle_context(lp)
                        last_vehicle_read = row["recorded_at_ms"]
                last_no_draw_status = evidence["status"] if evidence is not None else None
                confirmed_w = row["power_sample"]["confirmed_w"] or 0
                if confirmed_w > 0 and not first_positive:
                    row["first_fresh_positive_power"] = True
                    first_positive = True
                if not lp.get("plugged_in"):
                    row["end"] = "unplugged"
                elif confirmed_w >= 500:
                    row["end"] = "charging_observed"
                elif time.monotonic() >= deadline:
                    row["end"] = "window_elapsed"
                log.info(json.dumps(row, separators=(",", ":")))
                if "end" in row:
                    start_ms = None
            eligible_before = eligible
            consecutive_errors = 0
        except (OSError, ValueError, KeyError) as error:
            if not consecutive_errors:
                print(f"FTW diagnostics read failed: {type(error).__name__}", file=sys.stderr, flush=True)
            consecutive_errors += 1
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
