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
      if (!config.site) config.site = {};
      if (config.site.pv_surplus_absorb_soc_cap == null && config.site.pv_surplus_absorb_soc_cap_pct != null) {
        config.site.pv_surplus_absorb_soc_cap = config.site.pv_surplus_absorb_soc_cap_pct / 100;
      }
      delete config.site.pv_surplus_absorb_soc_cap_pct;
      var setup = config.setup ? '<fieldset><legend>Equipment control</legend>' +
        '<p id="setup-control-status" role="status">Checking the active mode…</p>' +
        '<label class="checkbox-row"><input type="checkbox" data-checkbox-path="setup.measurement_only"' + (config.setup.measurement_only ? ' checked' : '') + '> Measurement mode after the next restart</label>' +
        '<p>Mode changes need a save and restart. Until then the current mode stays active. Measurement mode keeps equipment writes off.</p>' +
        '<p>Before enabling control or changing A, phases or voltage: check the connection below against your network contract, and check the selected equipment and grid-boundary meter.</p>' +
        '<label class="checkbox-row"><input id="setup-confirm-connection" type="checkbox"> I checked this connection and the selected equipment</label>' +
        '<p id="setup-connection-note" role="status"></p>' +
        '<button type="button" id="setup-refresh-status" class="btn-ghost">Refresh active mode</button>' +
        '</fieldset>' : '';
      return setup + '<fieldset><legend>Site</legend>' +
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
        decimalField("Operator SoC cap (0–1)", "site.pv_surplus_absorb_soc_cap", 0,
          "Force surplus absorption until average battery SoC reaches this fraction. 0 leaves the decision to the planner's economic comparison. Suggested override: 0.88, leaving 0.02 below a typical planner soc_max of 0.90.",
          "0.01") +
        '</div><div>' +
        field("Export threshold (W)", "site.pv_surplus_absorb_threshold_w", "number", 100,
          "Trigger when projected grid export exceeds this many watts after the plan's target. Defaults to 100 W whenever the operator or planner enables absorption.") +
        '</div></div>' +
        '</fieldset>' +
        '<fieldset><legend>Electricity connection</legend>' +
        '<label for="fuse-connection">Connection</label>' +
        '<select id="fuse-connection">' +
          '<option value="3">Three-phase</option>' +
          '<option value="1">Single-phase</option>' +
          '<option value="custom">Custom connection</option>' +
        '</select>' +
        '<p id="fuse-summary" role="status"></p>' +
        field("Main fuse (A)", "fuse.max_amps", "number", 16,
          "Use the rating on your electricity network contract, not a fuse for one appliance.") +
        '<details id="fuse-advanced"><summary>Advanced connection settings</summary>' +
        '<p>Standard three-phase uses 230 V from phase to neutral and 400 V between phases. ' +
        'FTW needs the phase-to-neutral value. The standard choices above set it to 230 V. ' +
        'Changing these values does not add support for a different grid type.</p>' +
        '<div class="field-row"><div>' +
        field("Phases", "fuse.phases", "number", 3) +
        '</div><div>' +
        field("Phase-to-neutral voltage (V)", "fuse.voltage", "number", 230) +
        '</div></div>' +
        decimalField("Safety margin (A)", "fuse.safety_margin_a", 0,
          "Optional. Amps to keep below the fuse rating, for an inverter whose own phase limiter trips at the rating. 0 works to the rating.",
          "0.1") +
        '</details></fieldset>';
    },
    after: function (ctx) {
      var root = ctx.bodyEl;
      var connection = root.querySelector("#fuse-connection");
      var phases = root.querySelector('[data-path="fuse.phases"]');
      var voltage = root.querySelector('[data-path="fuse.voltage"]');
      var advanced = root.querySelector("#fuse-advanced");
      var summary = root.querySelector("#fuse-summary");
      function sync() {
        var standard = Number(voltage.value) === 230 && (phases.value === "1" || phases.value === "3");
        connection.value = standard ? phases.value : "custom";
        summary.textContent = standard
          ? (phases.value === "3" ? "Three-phase" : "Single-phase") + " · 230 V per phase"
          : "Custom connection · " + phases.value + " phases · " + voltage.value + " V per phase. Check Advanced before changing this.";
        if (!standard) advanced.open = true;
      }
      connection.addEventListener("change", function () {
        if (connection.value === "custom") {
          advanced.open = true;
          phases.focus();
          return;
        }
        phases.value = connection.value;
        voltage.value = "230";
        sync();
      });
      phases.addEventListener("input", sync);
      voltage.addEventListener("input", sync);
      sync();

      var confirmation = root.querySelector("#setup-confirm-connection");
      if (!confirmation) return;
      var note = root.querySelector("#setup-connection-note");
      var mode = root.querySelector("#setup-control-status");
      function readMode() {
        return ctx.apiFetch("/api/setup/draft").then(function (r) { if (!r.ok) throw new Error("Could not read active mode."); return r.json(); }).then(function (body) {
          var c = body.commissioning;
          if (!c) throw new Error("Active mode is unavailable on this box version.");
          if (c.measurement_only && !c.configured_measurement_only) mode.textContent = "Measurement mode is active. Control is saved and waits for a restart.";
          else if (!c.measurement_only && c.configured_measurement_only) mode.textContent = "Control is still active. Measurement mode is saved and waits for a restart.";
          else mode.textContent = c.measurement_only ? "Measurement mode is active. Equipment writes are off." : "Control mode is active. Hardware tests remain separate.";
          if (c.control_blocked_reason && !c.measurement_only) mode.textContent += " Dispatch is blocked: " + c.control_blocked_reason + ".";
        }).catch(function (e) { mode.textContent = e.message; });
      }
      root.querySelector("#setup-refresh-status").addEventListener("click", readMode);
      readMode();
      confirmation.addEventListener("change", function () {
        if (!confirmation.checked) return;
        ctx.captureCurrentTab();
        if (!(ctx.config.fuse.max_amps > 0)) {
          confirmation.checked = false;
          note.textContent = "Enter the main fuse rating from your network contract below before confirming. Keep measurement mode on if it is unknown.";
          return;
        }
        var snapshot = JSON.stringify(ctx.config.fuse);
        confirmation.disabled = true;
        note.textContent = "Checking this connection and the live meter…";
        ctx.apiFetch("/api/setup/connection", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({fuse: ctx.config.fuse})})
          .then(function (r) { return r.json().then(function (body) { if (!r.ok) throw new Error(body.error || "Connection check failed."); return body; }); })
          .then(function (review) {
            ctx.captureCurrentTab();
            if (JSON.stringify(ctx.config.fuse) !== snapshot) throw new Error("The connection changed. Check the new values and confirm again.");
            ctx.config.setup.connection_digest = review.connection_digest;
            ctx.config.setup.connection_confirmed = true;
            ctx.config.setup.equipment_reviewed = true;
            if (review.meter_identity) ctx.config.setup.meter_identity = review.meter_identity;
            note.textContent = review.can_enable_control ? "Connection confirmed for this save. Save and restart to change mode." : "Connection confirmed. Keep measurement mode on: " + review.blocked_reason;
          }).catch(function (e) { confirmation.checked = false; note.textContent = e.message; })
          .finally(function () { confirmation.disabled = false; });
      });
    },
  };
})();
