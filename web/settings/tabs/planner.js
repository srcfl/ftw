// Settings → Planner tab: MPC planner scalars.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

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
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'The planner requires working price + weather forecasts. When disabled the system runs in the manual mode set on the Control page.' +
        '</p>';
    },
  };
})();
