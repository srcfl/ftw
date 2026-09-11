import { fillPlanSoC } from "./plan-soc.js";

function formatClock(tsMs) {
  const date = new Date(tsMs);
  return `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

function readableReason(reason) {
  if (!reason) return "Balancing expected energy use and supply";
  const known = {
    scheduled: "Regular schedule refresh",
    manual: "You requested a fresh plan",
    "reactive-pv": "Solar production changed more than expected",
    "reactive-load": "Home use changed more than expected",
    "twin-drift-pv": "The solar forecast was corrected",
    "twin-drift-load": "The home-use forecast was corrected",
    surplus_only_disabled: "EV surplus-only charging was changed",
    loadpoint_schedule_changed: "An EV schedule was changed",
    loadpoint_target_changed: "An EV charge target was changed",
  };
  if (known[reason]) return known[reason];
  const text = String(reason).replace(/[_-]+/g, " ").trim();
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function actionLabel(action) {
  if (!action) return "Hold current operation";
  if ((action.loadpoint_w || 0) > 100) {
    return `Charge EV at ${(action.loadpoint_w / 1000).toFixed(1)} kW`;
  }
  if ((action.pv_limit_w || 0) > 0) {
    return `Limit solar output to ${(action.pv_limit_w / 1000).toFixed(1)} kW`;
  }
  if ((action.battery_w || 0) > 100) {
    return `Charge battery at ${(action.battery_w / 1000).toFixed(1)} kW`;
  }
  if ((action.battery_w || 0) < -100) {
    return `Use battery at ${(Math.abs(action.battery_w) / 1000).toFixed(1)} kW`;
  }
  return "Keep the battery steady";
}

function briefPower(watts) {
  const value = Number(watts) || 0;
  return Math.abs(value) >= 1000
    ? `${(value / 1000).toFixed(1)} kW`
    : `${Math.round(value)} W`;
}

function batteryIsPresent(status, actions) {
  if (Number.isFinite(status.bat_soc)) return true;
  const drivers = status.drivers || {};
  if (Object.values(drivers).some((driver) => (
    driver && (driver.bat_w != null || driver.bat_soc != null)
  ))) {
    return true;
  }
  return actions.some((action) => Number.isFinite(action && action.soc));
}

function socPercent(frac) {
  if (!Number.isFinite(frac)) return null;
  return frac * 100;
}

export function unavailablePlannerCopy(reason) {
  const known = {
    "no-battery-capacity": {
      label: "Cannot plan",
      action: "Planning needs a controllable battery",
      nextStep: "Add a battery in Devices, or pick a manual strategy",
      why: "No battery with a known capacity is available to schedule",
      detail: "The planner starts only when at least one battery reports capacity",
      summary: "Cannot plan — no controllable battery",
    },
    "no-price-provider": {
      label: "Cannot plan",
      action: "Planning needs electricity prices",
      nextStep: "Set a price zone in Settings → Price",
      why: "No price source is configured",
      detail: "The planner uses prices to decide when to charge and discharge",
      summary: "Cannot plan — no electricity prices",
    },
    "planner-disabled": {
      label: "Planner off",
      action: "Planning is turned off",
      nextStep: "Turn the planner on in Settings → Planner",
      why: "The planner is disabled in Settings",
      detail: "Enable it under Settings → Planner to create a schedule",
      summary: "Planner is turned off",
    },
  };
  return known[reason] || {
    label: "Cannot plan",
    action: "Planning cannot run right now",
    nextStep: "Check Settings → Planner, a battery, and a price zone",
    why: "A planning strategy is selected, but the planner is not running",
    detail: "The planner did not start, so no schedule is being dispatched",
    summary: "Cannot plan — planner is not running",
  };
}

function batterySocNow(status, hasBattery, detail) {
  if (!hasBattery) return null;
  return {
    label: Number.isFinite(status.bat_soc)
      ? `${(status.bat_soc * 100).toFixed(0)}% now`
      : "Live value unavailable",
    detail,
  };
}

function manualBrief(status, hasBattery) {
  return {
    state: { key: "manual", label: "Manual", tone: "idle" },
    next: {
      action: "Manual control is active",
      time: "Use the plan to create a schedule",
    },
    reason: "Planning is not controlling the battery",
    constraint: "FTW safety limits still apply to manual control",
    forecast: {
      label: "No plan forecast",
      detail: "Live readings continue without a forward schedule",
    },
    soc: batterySocNow(status, hasBattery, "Expected charge needs an active plan"),
    planner: {
      label: "Planner off",
      detail: "Use the plan to enable it",
    },
  };
}

function blockedBrief(status, hasBattery, reason) {
  const copy = unavailablePlannerCopy(reason);
  return {
    state: { key: "blocked", label: copy.label, tone: "warn" },
    next: {
      action: copy.action,
      time: copy.nextStep,
    },
    reason: copy.why,
    constraint: "No schedule is being dispatched",
    forecast: {
      label: "No plan forecast",
      detail: "Planning cannot run until this is fixed",
    },
    soc: batterySocNow(status, hasBattery, "Expected charge needs an active plan"),
    planner: {
      label: "Cannot plan",
      detail: copy.detail,
    },
  };
}

function preparingBrief(status, hasBattery) {
  return {
    state: { key: "preparing", label: "Preparing", tone: "warn" },
    next: {
      action: "Preparing the first plan",
      time: "FTW needs current price and forecast data",
    },
    reason: "Gathering enough data to plan safely",
    constraint: "No schedule is being dispatched",
    forecast: {
      label: "Inputs pending",
      detail: "Price and energy forecasts are still loading",
    },
    soc: hasBattery
      ? {
          label: Number.isFinite(status.bat_soc)
            ? `${(status.bat_soc * 100).toFixed(0)}% now`
            : "Live value unavailable",
          detail: "No planned charge path yet",
        }
      : null,
    planner: {
      label: "Waiting",
      detail: "No plan has passed validation yet",
    },
  };
}

export function derivePlanBrief({
  enabled = false,
  unavailableReason = "",
  plan = null,
  status = {},
  now = Date.now(),
  socOpts = {},
} = {}) {
  fillPlanSoC(plan, socOpts);
  const actions = plan && Array.isArray(plan.actions) ? plan.actions : [];
  const hasBattery = batteryIsPresent(status, actions);
  const plannerMode = String(status.mode || "").startsWith("planner_");

  if (!enabled) {
    if (plannerMode) return blockedBrief(status, hasBattery, unavailableReason);
    return manualBrief(status, hasBattery);
  }
  if (!actions.length) return preparingBrief(status, hasBattery);

  const solver = plan.solver || {};
  const stale = Boolean(status.plan_stale);
  const plannerActive = String(status.mode || "").startsWith("planner_");
  const state = stale
    ? { key: "stale", label: "Fallback active", tone: "warn" }
    : solver.fallback
      ? { key: "fallback", label: "Built-in plan active", tone: "warn" }
      : plannerActive
        ? { key: "active", label: "Plan active", tone: "active" }
        : { key: "ready", label: "Plan ready", tone: "idle" };

  const live = actions.find((action) => (
    now >= action.slot_start_ms &&
    now < action.slot_start_ms + action.slot_len_min * 60_000
  ));
  const meaningful = (action) => (
    Math.abs(action.battery_w || 0) > 100 ||
    (action.loadpoint_w || 0) > 100 ||
    (action.pv_limit_w || 0) > 0
  );
  const futureMeaningful = actions.find((action) => (
    action.slot_start_ms >= now && meaningful(action)
  ));
  const next = live && meaningful(live)
    ? live
    : futureMeaningful ||
      live ||
      actions.find((action) => action.slot_start_ms >= now) ||
      actions[actions.length - 1];
  const isNow = next === live;
  const nextEnd = next && next.slot_start_ms + next.slot_len_min * 60_000;

  const clamps = (status.dispatch || []).filter((dispatch) => dispatch.clamped);
  let constraint = "No active safety adjustment";
  if (clamps.length) {
    const adjusted = clamps
      .map((dispatch) => `${dispatch.driver || "device"} to ${briefPower(dispatch.target_w)}`)
      .join(", ");
    constraint = `Safety adjusted ${adjusted} to stay within battery or site limits`;
  } else if (stale) {
    constraint = "The schedule is old, so FTW is using safe live balancing";
  }

  const uncertain = actions.filter((action) => (
    action.confidence != null && action.confidence < 0.999
  ));
  const forecast = uncertain.length
    ? {
        label: (
          uncertain.reduce((sum, action) => sum + action.confidence, 0) /
          uncertain.length
        ) >= 0.75
          ? "Some modeled inputs"
          : "Higher uncertainty later",
        detail: `Observed market data to ${formatClock(uncertain[0].slot_start_ms)}; forecast after that`,
      }
    : {
        label: "Current published inputs",
        detail: "No modeled price period in this plan",
      };

  const finalAction = actions[actions.length - 1];
  const nextSocPct = next ? socPercent(next.soc) : null;
  const finalSocPct = finalAction ? socPercent(finalAction.soc) : null;
  const soc = hasBattery
    ? {
        label: nextSocPct != null
          ? `${nextSocPct.toFixed(0)}% after next step`
          : "—",
        detail: finalSocPct != null
          ? `${finalSocPct.toFixed(0)}% at the end of the plan`
          : "No battery forecast available",
      }
    : null;

  const solverName = [solver.engine, solver.backend].filter(Boolean).join(" / ") ||
    "FTW planner";

  return {
    state,
    next: {
      action: actionLabel(next),
      time: next
        ? isNow
          ? `Now, until ${formatClock(nextEnd)}`
          : `At ${formatClock(next.slot_start_ms)}`
        : "No action inside the current horizon",
    },
    reason: readableReason(next && next.reason),
    constraint,
    forecast,
    soc,
    planner: {
      label: solver.fallback ? "Built-in fallback" : solverName,
      detail: solver.fallback
        ? readableReason(solver.fallback_reason || "Primary solver unavailable")
        : solver.status
          ? `Plan result: ${readableReason(solver.status)}`
          : "Plan passed FTW validation",
    },
  };
}
