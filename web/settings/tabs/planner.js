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
        '<label>Active strategy ' +
        help("The strategy the planner is running right now. It is chosen with the Strategy buttons on the dashboard Plan card and persists across restarts. The config file's planner.mode is only the first-boot default and is not editable here.") +
        '</label>' +
        '<div id="planner-active-strategy" style="font-family:var(--mono);margin:2px 0 0">—</div>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin:4px 0 12px">Set from the Plan card on the dashboard — not editable here.</p>' +
        '<div class="field-row"><div>' +
        selectField("Engine", "planner.engine", ["python", "dp"], "python",
          "Python runs the CVXPY mathematical optimizer. DP is the emergency rollback engine.") +
        '</div><div>' +
        selectField("Solver", "planner.optimizer_solver", ["HIGHS", "CLARABEL"], "HIGHS",
          "HiGHS handles LP and MILP. CLARABEL is available only for continuous convex formulations.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        selectField("Formulation", "planner.optimizer_formulation", ["auto", "milp", "relaxed"], "auto",
          "Auto introduces integer variables only when physics or discrete asset steps require them.") +
        '</div><div>' +
        field("Solver timeout (s)", "planner.optimizer_timeout_s", "number", 5,
          "Whole worker deadline. A timeout activates the validated Go-DP fallback for that replan.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("MIP relative gap", "planner.optimizer_mip_rel_gap", "number", 0.005,
          "Accepted HiGHS MILP optimality gap. 0.005 means 0.5 percent.") +
        '</div><div>' +
        field("CVaR risk weight", "planner.optimizer_cvar_weight", "number", 0.15,
          "Weight on expensive forecast-tail scenarios. 0 disables tail-risk cost.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("CVaR alpha", "planner.optimizer_cvar_alpha", "number", 0.9,
          "Tail confidence level. 0.9 optimizes the worst ten percent of scenario cost.") +
        '</div><div>' +
        selectField("Shadow policy", "planner.optimizer_challenger_policy", ["recourse", "multistage"], "recourse",
          "Recourse is the two-stage reference. Multistage uses a hierarchical scenario tree, move-blocking, service risk, and scenario reduction.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Shared prefix (slots)", "planner.optimizer_recourse_non_anticipative_slots", "number", 1,
          "Initial slots that use the same action in every scenario. One slot means the next 15-minute action is executable before replanning.") +
        '</div><div>' +
        field("Scenario limit", "planner.optimizer_multistage.scenario_limit", "number", 12,
          "Maximum representative trajectories retained by energy-weighted scenario reduction.") +
        '</div></div>' +
        '<label class="checkbox-row"><input type="checkbox" data-checkbox-path="planner.optimizer_recourse_shadow"' + (config.planner.optimizer_recourse_shadow ? ' checked' : '') + '> Stochastic shadow ' +
        help('Run the stochastic storage challenger and stateful score it against the active champion. It never controls dispatch and pauses while flexible assets are active.') + '</label>' +
        '<div class="field-row"><div>' +
        field("Branch interval (slots)", "planner.optimizer_multistage.branch_interval_slots", "number", 4,
          "How often the near-horizon scenario tree may reveal new information.") +
        '</div><div>' +
        field("Branch horizon (slots)", "planner.optimizer_multistage.branch_horizon_slots", "number", 48,
          "Stop adding new scenario branches after this many slots to bound edge complexity.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Near horizon (slots)", "planner.optimizer_multistage.near_horizon_slots", "number", 16,
          "Slots kept at full 15-minute control resolution.") +
        '</div><div>' +
        field("Far move block (slots)", "planner.optimizer_multistage.far_block_slots", "number", 4,
          "Far-horizon actions tied into blocks. Four slots give hourly decisions.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Service CVaR weight", "planner.optimizer_multistage.service_cvar_weight", "number", 1,
          "Risk weight on target and operating-bound violations, optimized before economic cost.") +
        '</div><div>' +
        field("Decomposition threshold", "planner.optimizer_multistage.decomposition_threshold", "number", 20,
          "Scenario count above which auto mode uses eligible Progressive Hedging or reduces to the exact extensive budget.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("SoC min (%)", "planner.soc_min_pct", "number", 10,
          "Lowest SoC the planner will discharge to (percent). 10 = 10%.") +
        '</div><div>' +
        field("SoC max (%)", "planner.soc_max_pct", "number", 90,
          "Highest SoC the planner will charge to (percent). 90 = 90%.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("PV forecast safety (k)", "planner.pv_forecast_safety_k", "number", 1.0,
          "How much the planner trusts the solar forecast. It plans against forecast − k×σ, where σ is the live PV-forecast error. Higher k = trust the forecast less: the battery holds more reserve and charges earlier, drifting toward self-consumption behaviour. 0 = trust the forecast fully (no hedge). On clear, stable days σ shrinks toward zero and k has little effect — the hedge sizes itself to the real risk.") +
        '<div id="planner-hedge-line" style="display:none;color:var(--text-dim);font-size:0.8rem;margin-top:4px"></div>' +
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
  S.tabs.planner._pure = { strategyLabel: strategyLabel, hedgeLine: hedgeLine };
})();
