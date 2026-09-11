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

  function engineSelect(engine, help) {
    var selected = String(engine == null ? "" : engine).trim().toLowerCase();
    if (selected === "go" || selected === "dp") selected = "core";
    if (selected === "python") selected = "energyplan";
    var options = [["", "Automatic (release default)"], ["energyplan", "Energyplan"],
      ["core", "Core DP"]];
    return '<label for="planner-engine">Engine ' +
      help("Automatic uses Energyplan in supported beta builds and Core DP elsewhere. Energyplan runs Core DP as a shadow and uses it as fallback. Changing the engine requires a restart.") +
      '</label><select id="planner-engine" data-path="planner.engine">' +
      options.map(function (option) {
        return '<option value="' + option[0] + '"' + (selected === option[0] ? ' selected' : '') + '>' + option[1] + '</option>';
      }).join("") + '</select>';
  }

  S.tabs.planner = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField, help = ctx.help, config = ctx.config;
      var planner = config.planner || {};
      if (planner.soc_min == null && planner.soc_min_pct != null) {
        planner.soc_min = planner.soc_min_pct / 100;
      }
      if (planner.soc_max == null && planner.soc_max_pct != null) {
        planner.soc_max = planner.soc_max_pct / 100;
      }
      delete planner.soc_min_pct;
      delete planner.soc_max_pct;
      var kHtml;
      if (planner.pv_forecast_safety_k != null) {
        kHtml = field("PV forecast safety (k)", "planner.pv_forecast_safety_k", "number", 1.0,
          "How much the planner trusts the solar forecast. It plans against forecast − k×σ, where σ is the live PV-forecast error. Higher k = trust the forecast less: the battery holds more reserve and charges earlier, drifting toward self-consumption behaviour. 0 = trust the forecast fully (no hedge). On clear, stable days σ shrinks toward zero and k has little effect.") +
          '<div id="planner-hedge-line" style="display:none;color:var(--text-dim);font-size:0.8rem;margin-top:4px"></div>';
      } else {
        kHtml = '<p style="color:var(--text-dim);font-size:0.8rem;margin:4px 0 8px">PV forecast safety k is not set in YAML. The Plan card slider owns it, anywhere from 0 to 2 in steps of 0.05.</p>' +
          '<div id="planner-hedge-line" style="display:none;color:var(--text-dim);font-size:0.8rem;margin-top:4px"></div>';
      }
      return '<fieldset><legend>MPC Planner</legend>' +
        '<label><input type="checkbox" data-checkbox-path="planner.enabled"' + (planner.enabled ? ' checked' : '') + '> Enabled ' +
        help('Enable the MPC planner. When active it overrides manual mode with an optimised schedule.') + '</label>' +
        '<div class="field-row"><div>' +
        field("House reserve (min SoC, 0–1)", "planner.soc_min", "number", 0.10,
          "Lowest SoC the planner will discharge to, so the house keeps a reserve. 0.10 = 10%.") +
        '</div><div>' +
        field("Max SoC (0–1)", "planner.soc_max", "number", 0.95,
          "Highest SoC the planner will charge to. The default is 0.95 = 95%.") +
        '</div></div>' +
        '</fieldset>' +
        '<details class="engine-details">' +
        '<summary>Engine controls — leave these unless you are debugging.</summary>' +
        '<fieldset><legend>Engine</legend>' +
        '<label>Mapped strategy ' +
        help("The planner mode currently mapped from battery-export permission. Forecast trust and export live on the Plan card.") +
        '</label>' +
        '<div id="planner-active-strategy" style="font-family:var(--mono);margin:2px 0 12px">—</div>' +
        '<div class="field-row"><div>' +
        engineSelect(planner.engine, help) +
        '</div></div>' +
        '<p>Energyplan uses a 500 ms solve limit. Core DP runs in the background for comparison and supplies a fallback if needed.</p>' +
        '<div class="field-row"><div>' + kHtml + '</div></div>' +
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
        '</details>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'The planner requires working price + weather forecasts. When disabled the system runs in the manual mode set on the Control page.' +
        '</p>';
    },
    after: function (ctx) {
      var apiFetch = ctx.apiFetch || window.fetch.bind(window);

      // ---- Active strategy (read-only, from the runtime, not the YAML) ----
      var stratEl = document.getElementById("planner-active-strategy");
      if (stratEl) {
        // /api/modes is the server-side mode catalog from PR #468; older
        // hosts 404 it — treat any failure as "no catalog" and fall back
        // to the local label table.
        var catalogP = apiFetch("/api/modes")
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (d) { return d && d.modes ? d.modes : null; })
          .catch(function () { return null; });
        var modeP = apiFetch("/api/status")
          .then(function (r) { return r.json(); })
          .then(function (d) { return d && d.mode; })
          .catch(function () { return null; });
        Promise.all([modeP, catalogP]).then(function (res) {
          stratEl.textContent = strategyLabel(res[0], res[1]);
        });
      }

      // ---- Live σ/hedge readout under the k field ----
      var hedgeEl = document.getElementById("planner-hedge-line");
      var kInput = document.querySelector('input[data-path="planner.pv_forecast_safety_k"]');
      if (hedgeEl && kInput) {
        apiFetch("/api/pvmodel")
          .then(function (r) { return r.json(); })
          .then(function (d) {
            if (!d || d.enabled === false) return; // pvmodel off → line stays hidden
            var sigma = d.pv_residual_std_w;
            function update() {
              var text = hedgeLine(kInput.value, sigma);
              if (text == null) return;
              hedgeEl.textContent = text;
              hedgeEl.style.display = "";
            }
            update();
            kInput.addEventListener("input", update);
          })
          .catch(function () {}); // unreachable → line stays hidden
      }
    },
  };

  // Escape hatch for node --test (planner.test.mjs); not a public API.
  S.tabs.planner._pure = { strategyLabel: strategyLabel, hedgeLine: hedgeLine, engineSelect: engineSelect };
})();
