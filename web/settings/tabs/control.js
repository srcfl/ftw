// Settings → Control tab: site + fuse scalars that feed the PI loop.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  S.tabs.control = {
    render: function (ctx) {
      var field = ctx.field, help = ctx.help, getByPath = ctx.getByPath, escHtml = ctx.escHtml, config = ctx.config;
      // Local helper for fractional-amp fields — the central field()
      // helper emits no step attribute, which most browsers treat as
      // step=1 and refuse 0.5 entries on validation.
      function decimalField(label, path, dflt, helpText, step) {
        var val = getByPath(config, path, dflt);
        return '<label>' + label + (helpText ? ' ' + help(helpText) : '') + '</label>' +
          '<input type="number" step="' + step + '" data-path="' + path +
          '" value="' + escHtml(val == null ? "" : String(val)) + '">';
      }
      return '<fieldset><legend>Site</legend>' +
        field("Name", "site.name", "text", "Home") +
        '<div class="field-row"><div>' +
        field("Grid target (W)", "site.grid_target_w", "number", 0) +
        '</div><div>' +
        field("Grid tolerance (W)", "site.grid_tolerance_w", "number", 42) +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Slew rate (W/cycle)", "site.slew_rate_w", "number", 500) +
        '</div><div>' +
        field("Min dispatch interval (s)", "site.min_dispatch_interval_s", "number", 5) +
        '</div></div>' +
        '<label class="checkbox-row"><input type="checkbox" data-checkbox-path="site.troubleshooting_mode"' +
          (getByPath(config, "site.troubleshooting_mode", false) ? ' checked' : '') +
          '> Troubleshooting mode ' +
          help("Incident diagnostics only. Adds dispatch-decision logs and driver readback metrics without changing control behavior.") +
        '</label>' +
        '<div class="field-row"><div>' +
        field("Smoothing alpha", "site.smoothing_alpha", "number", 0.3,
          "EMA smoothing factor for the grid reading (0-1). Lower = smoother but slower response.") +
        '</div><div>' +
        field("PI gain", "site.gain", "number", 0.5,
          "Proportional gain of the PI controller. Higher = more aggressive correction.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Control interval (s)", "site.control_interval_s", "number", 5) +
        '</div><div>' +
        field("Watchdog timeout (s)", "site.watchdog_timeout_s", "number", 60) +
        '</div></div>' +
        '</fieldset>' +
        '<fieldset><legend>PV surplus absorber</legend>' +
        '<p style="color:var(--fg-dim);font-size:0.85rem;margin:0 0 8px">' +
        'Live underlay for planner_cheap / planner_arbitrage. The planner enables it automatically ' +
        'when surplus now can replace a more expensive future grid charge. ' +
        'When the plan would still leave the grid exporting beyond the threshold AND ' +
        'average battery SoC is below the cap, the leftover export is redirected into the battery ' +
        'instead of crossing the meter. Never reverses a discharge plan. ' +
        'Set a SoC cap below to force the policy for every eligible non-discharge slot.' +
        '</p>' +
        '<div class="field-row"><div>' +
        field("Operator SoC cap (%)", "site.pv_surplus_absorb_soc_cap_pct", "number", 0,
          "Force surplus absorption until average battery SoC reaches this percentage. 0 leaves the decision to the planner's economic comparison. Suggested override: 88, leaving 2 pp below a typical planner soc_max_pct of 90.") +
        '</div><div>' +
        field("Export threshold (W)", "site.pv_surplus_absorb_threshold_w", "number", 100,
          "Trigger when projected grid export exceeds this many watts after the plan's target. Defaults to 100 W whenever the operator or planner enables absorption.") +
        '</div></div>' +
        '</fieldset>' +
        '<fieldset><legend>Fuse</legend>' +
        '<div class="field-row"><div>' +
        field("Max amps (A)", "fuse.max_amps", "number", 16) +
        '</div><div>' +
        field("Phases", "fuse.phases", "number", 3) +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Voltage (V)", "fuse.voltage", "number", 230) +
        '</div><div>' +
        decimalField("Safety margin (A)", "fuse.safety_margin_a", 0.5,
          "Headroom below max amps so the inverter's own per-phase limiter doesn't trip first. Defaults to 0.5 A.",
          "0.1") +
        '</div></div>' +
        '</fieldset>';
    },
  };
})();
