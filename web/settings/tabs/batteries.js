// Settings → Batteries tab: per-battery limit overrides.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  S.tabs.batteries = {
    render: function (ctx) {
      var field = ctx.field, escHtml = ctx.escHtml, config = ctx.config;
      if (!config.batteries) config.batteries = {};
      var html = '<p style="color:var(--text-dim);font-size:0.8rem">Per-battery limits override the defaults. Leave blank to use BMS defaults.</p>';
      (config.drivers || []).forEach(function (d) {
        if (d.battery_capacity_wh > 0 && !d.observe_only) {
          if (!config.batteries[d.name]) config.batteries[d.name] = {};
          html += '<fieldset><legend>' + escHtml(d.name) + ' &mdash; ' + (d.battery_capacity_wh / 1000).toFixed(1) + ' kWh</legend>' +
            '<div class="field-row"><div>' +
            field("Min SoC (fraction 0–1)", "batteries." + d.name + ".soc_min", "number", "",
              "Lower SoC bound the planner is allowed to discharge to. 0.10 = 10%. Leave blank to use the battery BMS default.") +
            '</div><div>' +
            field("Max SoC (fraction 0–1)", "batteries." + d.name + ".soc_max", "number", "",
              "Upper SoC bound the planner is allowed to charge to. 0.95 = 95%. Avoid 1.0 to extend battery life.") +
            '</div></div>' +
            '<div class="field-row"><div>' +
            field("Max charge (W)", "batteries." + d.name + ".max_charge_w", "number", "",
              "Peak charge rate the driver will command. Defaults to 0.5C (half capacity).") +
            '</div><div>' +
            field("Max discharge (W)", "batteries." + d.name + ".max_discharge_w", "number", "",
              "Peak discharge rate the driver will command. Defaults to 0.5C.") +
            '</div></div>' +
            field("Weight (for weighted mode)", "batteries." + d.name + ".weight", "number", 1,
              "Share of correction this battery takes when control mode is 'weighted'. 1.0 = equal with other batteries.") +
            '</fieldset>';
        }
      });
      return html;
    },
  };
})();
