// Settings → Planner tab: MPC planner scalars.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // strategyLabel maps a control-mode string to the operator-facing
  // label. Prefers the /api/modes catalog (PR #468) when provided so we
  // don't become yet another hard-coded copy of the mode list; falls
  // back to a local table, then to prettifying the raw mode string.
  // Non-planner modes get a "(manual …)" suffix: the planner computes a
  // plan but the dispatcher isn't following it.
  var STRATEGY_LABELS = {
    planner_passive_arbitrage: "Passive arbitrage",
    planner_arbitrage: "Active arbitrage",
    planner_self: "Self-consumption (planner, legacy)",
    planner_cheap: "Cheap charge (planner, legacy)",
  };

  function strategyLabel(mode, catalog) {
    if (!mode) return "—";
    var label = null;
    if (catalog && catalog.length) {
      for (var i = 0; i < catalog.length; i++) {
        if (catalog[i] && catalog[i].key === mode && catalog[i].label) {
          label = catalog[i].label;
          break;
        }
      }
    }
    if (!label) label = STRATEGY_LABELS[mode];
    if (!label) {
      label = mode.replace(/_/g, " ");
      label = label.charAt(0).toUpperCase() + label.slice(1);
    }
    if (mode.indexOf("planner_") !== 0) label += " (manual — planner not dispatching)";
    return label;
  }

  // hedgeLine renders the live "what does k actually do" readout under
  // the k input: σ (the live PV-forecast error std from /api/pvmodel)
  // and the resulting hedge k·σ in watts. Returns null when σ is
  // missing/invalid — the caller keeps the line hidden.
  function hedgeLine(k, sigmaW) {
    if (sigmaW == null || typeof sigmaW !== "number" || isNaN(sigmaW) || sigmaW < 0) return null;
    var sigma = Math.round(sigmaW);
    if (sigma < 1) return "σ right now ≈ 0 W — no hedge";
    var kn = parseFloat(k);
    if (isNaN(kn) || kn < 0) kn = 0;
    return "σ right now ≈ " + sigma + " W → hedge = k·σ ≈ " + Math.round(kn * sigma) + " W";
  }

  S.tabs.planner = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField, help = ctx.help, config = ctx.config;
      if (!config.planner) config.planner = {};
      return '<fieldset><legend>MPC Planner</legend>' +
        '<label><input type="checkbox" data-checkbox-path="planner.enabled"' + (config.planner.enabled ? ' checked' : '') + '> Enabled ' +
        help('Enable the MPC planner. When active it overrides manual mode with an optimised schedule.') + '</label>' +
        selectField("Mode", "planner.mode", ["passive_arbitrage", "arbitrage", "self_consumption", "cheap_charge"], "passive_arbitrage",
          "passive_arbitrage = charge from cheapest source (PV or cheap grid), never export from battery. arbitrage = full timing arbitrage including battery export. self_consumption / cheap_charge = legacy (use passive_arbitrage instead).") +
        '<div class="field-row"><div>' +
        field("SoC min (%)", "planner.soc_min_pct", "number", 10,
          "Lowest SoC the planner will discharge to (percent). 10 = 10%.") +
        '</div><div>' +
        field("SoC max (%)", "planner.soc_max_pct", "number", 90,
          "Highest SoC the planner will charge to (percent). 90 = 90%.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("PV forecast safety (k)", "planner.pv_forecast_safety_k", "number", 1.0,
          "How conservative the planner is about solar that might not arrive. It plans against forecast − k×σ, where σ is the live PV-forecast error. Higher k keeps more battery reserve on uncertain/cloudy days; 1.0 is the default; 0 = use the full battery (no hedge). On clear days and in winter the hedge is ~0 automatically.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Base load (W)", "planner.base_load_w", "number", 0,
          "Constant household load estimate used when the load twin has no data yet.") +
        '</div><div>' +
        field("Horizon (hours)", "planner.horizon_hours", "number", 48,
          "Planning horizon in hours. 48 h covers two day-ahead price windows.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Replan interval (min)", "planner.interval_min", "number", 15,
          "How often the planner re-solves. Lower = more responsive but more CPU.") +
        '</div><div>' +
        field("Export value (ore/kWh)", "planner.export_ore_per_kwh", "number", 0,
          "Override export value. 0 = use mean spot price.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Charge efficiency", "planner.charge_efficiency", "number", 0.95,
          "Round-trip charge efficiency (0-1). 0.95 = 5% loss charging.") +
        '</div><div>' +
        field("Discharge efficiency", "planner.discharge_efficiency", "number", 0.95,
          "Round-trip discharge efficiency (0-1). 0.95 = 5% loss discharging.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Min arbitrage spread (öre/kWh)", "planner.min_arbitrage_spread_ore_kwh", "number", 0,
          "The battery won't cycle for grid arbitrage unless the price gain beats this many öre/kWh, on top of round-trip losses. 0 = off. Higher = fewer, deeper cycles. Self-consumption is never affected. Tune empirically.") +
        '</div></div>' +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'The planner requires working price + weather forecasts. When disabled the system runs in the manual mode set on the Control page.' +
        '</p>';
    },
  };

  // Escape hatch for node --test (planner.test.mjs); not a public API.
  S.tabs.planner._pure = { strategyLabel: strategyLabel, hedgeLine: hedgeLine };
})();
