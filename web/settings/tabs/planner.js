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

  function formatK(k) {
    return String(Math.round(Number(k) * 100) / 100);
  }

  // styleNote names the Plan card style a k belongs to. The style table
  // comes from plan.js (window.FTWPlanPrefs); without it the note is plain.
  function styleNote(k, lib) {
    if (!lib || typeof lib.styleForK !== "function") return "Changes apply at once.";
    var found = lib.styleForK(k);
    return (found.exact ? found.style.name + "." : "Between two styles, nearest " + found.style.name + ".") +
      " Changes apply at once.";
  }

  // marginModel holds what the margin slider shows, for the whole page: the
  // value the box last confirmed, from any control, unless a save from here
  // is on its way. A read that started before a confirmed write is older
  // than it and changes nothing.
  function marginModel() {
    var confirmed = null;
    var rev = 0;
    var pending = 0;
    var requested = null;
    var outcome = ""; // "saved" or "failed" after this page's last save
    return {
      confirm: function (k) { confirmed = k; rev++; outcome = ""; },
      beginRead: function () { return rev; },
      endRead: function (readRev, k) { if (readRev === rev && pending === 0) confirmed = k; },
      request: function (k) { pending++; requested = k; outcome = ""; },
      settle: function (ok) { pending--; if (pending === 0) outcome = ok ? "saved" : "failed"; },
      view: function () {
        if (pending > 0) return { k: requested, state: "saving" };
        return { k: confirmed, state: outcome };
      },
    };
  }

  var margin = marginModel(); // outlives a redraw of the tab
  var paintMargin = null; // the open tab's painter
  var marginListening = false;

  // savePlannerMargin sends only the margin, through the Plan card's queue so
  // writes from the card and from here reach the box in the order they were
  // made. The queue announces the box's answer; without the card, so does
  // this.
  function savePlannerMargin(apiFetch, k) {
    var lib = window.FTWPlanPrefs;
    if (lib && typeof lib.savePlannerPrefs === "function") return lib.savePlannerPrefs({ safety_k: k }, "settings");
    return apiFetch("/api/planner/prefs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ safety_k: k }),
    })
      .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); })
      .then(function (p) {
        window.dispatchEvent(new CustomEvent("ftw-planner-prefs", { detail: Object.assign({}, p, { source: "settings" }) }));
        return p;
      });
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
      // The live forecast margin. It saves at once through the same
      // endpoint as the Plan card's styles, not through config Save.
      var fineHtml = '<div class="planner-style-fine">' +
        '<label for="planner-style-k">Forecast margin (k) ' +
        help("The Plan card's five planning styles set this number: Very careful 1, Careful 0.6, Balanced 0.3, Bold 0.15, Very bold 0. Higher plans for less sun and more use than forecast. Changes apply at once and do not wait for Save.") +
        '</label>' +
        '<div style="display:flex;gap:10px;align-items:center">' +
        '<input type="range" id="planner-style-k" min="0" max="2" step="0.05" value="0.3" disabled style="flex:1">' +
        '<output id="planner-style-k-value" for="planner-style-k" style="font-family:var(--mono);min-width:4.5em">…</output>' +
        '</div>' +
        '<p id="planner-style-k-note" style="color:var(--text-dim);font-size:0.8rem;margin:4px 0 0">Loading…</p>' +
        '<p id="planner-margin-line" style="color:var(--text-dim);font-size:0.8rem;margin:4px 0 0" hidden></p>' +
        '</div>';
      var seedHtml = planner.pv_forecast_safety_k != null
        ? '<p style="color:var(--text-dim);font-size:0.8rem;margin:4px 0 8px">config.yaml sets pv_forecast_safety_k to ' +
          (ctx.escHtml || String)(formatK(planner.pv_forecast_safety_k)) +
          '. It only seeds the first start; the forecast margin above is what the planner uses.</p>'
        : "";
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
        fineHtml +
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
        seedHtml +
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
        'The planner plans as far ahead as electricity prices are published and uses the weather forecast for solar. When disabled the system runs in the manual mode set on the Control page.' +
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

      // ---- Forecast margin: read, fine-tune, save at once ----
      var kInput = document.getElementById("planner-style-k");
      var kValue = document.getElementById("planner-style-k-value");
      var kNote = document.getElementById("planner-style-k-note");
      var marginEl = document.getElementById("planner-margin-line");
      if (kInput && kValue && kNote) {
        var lib = window.FTWPlanPrefs || null;
        var show = function (k, prefix) {
          kValue.textContent = "k " + formatK(k);
          kNote.textContent = (prefix || "") + styleNote(k, lib);
        };
        var dragging = false;
        // Paints the page's margin state, unless a drag is under way.
        paintMargin = function () {
          if (!kInput.isConnected || dragging) return;
          var v = margin.view();
          if (v.k == null) return;
          kInput.value = String(v.k);
          show(v.k, v.state === "saving" ? "Saving… " : v.state === "saved" ? "Saved. " : "");
          if (v.state === "failed") kNote.textContent = "Not saved: the box did not answer. Try again.";
        };
        if (!marginListening) {
          marginListening = true;
          // Every write the box confirms, from the Plan card or from here.
          window.addEventListener("ftw-planner-prefs", function (e) {
            var p = e.detail || {};
            var k = typeof p.safety_k === "number" ? p.safety_k : p.mapped_k;
            if (typeof k !== "number") return;
            margin.confirm(k);
            if (paintMargin) paintMargin();
          });
        }
        paintMargin(); // what the page already knows, before the read answers
        var readRev = margin.beginRead();
        apiFetch("/api/planner/prefs")
          .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); })
          .then(function (p) {
            kInput.disabled = false;
            margin.endRead(readRev, typeof p.safety_k === "number" ? p.safety_k : p.mapped_k);
            paintMargin();
          })
          .catch(function () { kNote.textContent = "The box did not answer. Reopen Settings to try again."; });
        // A drag is a pointer held on the slider. Chrome sends no change when
        // a drag ends where it began, so the release ends it too. The repaint
        // waits a turn, after the change a moved thumb sends.
        var endDrag = function () {
          if (!dragging) return;
          dragging = false;
          setTimeout(function () { if (paintMargin) paintMargin(); }, 0);
        };
        kInput.addEventListener("pointerdown", function () { dragging = true; });
        ["pointerup", "pointercancel", "lostpointercapture", "blur"].forEach(function (type) {
          kInput.addEventListener(type, endDrag);
        });
        kInput.addEventListener("input", function () { show(kInput.value); });
        kInput.addEventListener("change", function () {
          dragging = false;
          var k = Number(kInput.value);
          margin.request(k);
          paintMargin();
          savePlannerMargin(apiFetch, k)
            .then(function () { return true; }, function () { return false; })
            .then(function (ok) {
              margin.settle(ok);
              if (paintMargin) paintMargin();
            });
        });
        // The same margin as the Plan card, split into sun and use. It
        // follows each plan the Plan card fetches, so a save shows up here.
        if (marginEl && lib) {
          var showMargin = function (actions) {
            if (!actions || !actions.length) {
              // No plan now: an old margin must not read as the current one.
              marginEl.textContent = "";
              marginEl.hidden = true;
              return;
            }
            var last = actions[actions.length - 1];
            var text = lib.marginSplitLine(lib.forecastMargins(actions, Date.now() - 30 * 60 * 1000,
              last.slot_start_ms + last.slot_len_min * 60 * 1000));
            marginEl.textContent = text || "";
            marginEl.hidden = !text;
          };
          var tab = S.tabs.planner;
          if (tab._onPlan) window.removeEventListener("ftw-plan-data", tab._onPlan);
          tab._onPlan = function (e) {
            if (marginEl.isConnected) showMargin(e.detail && e.detail.plan && e.detail.plan.actions);
          };
          window.addEventListener("ftw-plan-data", tab._onPlan);
          apiFetch("/api/mpc/plan")
            .then(function (r) { return r.json(); })
            .then(function (m) { showMargin(m && m.plan && m.plan.actions); })
            .catch(function () {});
        }
      }
    },
  };

  // Escape hatch for node --test (planner.test.mjs); not a public API.
  S.tabs.planner._pure = { strategyLabel: strategyLabel, styleNote: styleNote, formatK: formatK, engineSelect: engineSelect,
    marginModel: marginModel };
})();
