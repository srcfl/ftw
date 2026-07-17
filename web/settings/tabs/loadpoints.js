// Settings → Loadpoints tab: connect an EV-capable driver to a loadpoint
// and set its electrical envelope (min/max charging power, allowed steps,
// vehicle capacity). Schedules + surplus_only stay on the dashboard EV
// modal — those are operator-day decisions, not site-setup.
//
// Bound state: config.loadpoints[] — same JSON shape persisted to YAML
// (see go/internal/config/config.go::Loadpoint).
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function ownerFetch(path, opts) {
    return fetch(path, opts);
  }

  // Drivers eligible to back a loadpoint = ones the catalog tags with
  // the "ev" capability. We resolve via the same catalogByLua map the
  // Devices tab populates (loaded once per modal open).
  function evDrivers(config) {
    var out = [];
    var cat = S.catalogByLua || {};
    (config.drivers || []).forEach(function (d) {
      var entry = d.lua ? cat[d.lua] : null;
      var caps = (entry && entry.capabilities) || [];
      if (caps.indexOf("ev") >= 0) out.push(d.name || "");
    });
    return out;
  }

  function fmtStepsW(arr) {
    if (!Array.isArray(arr) || !arr.length) return "";
    return arr.map(function (n) { return String(Math.round(n)); }).join(", ");
  }

  function parseStepsW(s) {
    if (!s || !s.trim()) return [];
    var parts = s.split(/[,\s]+/).filter(Boolean);
    var out = [];
    for (var i = 0; i < parts.length; i++) {
      var n = parseFloat(parts[i]);
      if (!isFinite(n) || n < 0) continue;
      out.push(Math.round(n));
    }
    // 0 is implicit (always allowed). Dedup + sort.
    if (out.indexOf(0) < 0) out.unshift(0);
    out.sort(function (a, b) { return a - b; });
    var seen = {};
    return out.filter(function (n) {
      if (seen[n]) return false;
      seen[n] = true;
      return true;
    });
  }

  S.tabs.loadpoints = {
    render: function (ctx) {
      var help = ctx.help, escHtml = ctx.escHtml, config = ctx.config;
      if (!config.loadpoints) config.loadpoints = [];
      var drivers = evDrivers(config);

      var html =
        '<p style="color:var(--text-dim);font-size:0.8rem;margin:0 0 12px">' +
        'A <b>loadpoint</b> binds a configured EV charger driver to the planner so it can schedule charging against your tariff + PV forecast. ' +
        'Add a driver first under <b>Devices</b>; then pick it here and set the electrical envelope.' +
        '</p>';

      if (!drivers.length) {
        html +=
          '<div class="ha-status-indicator ha-warn" style="margin:0 0 12px">' +
          '⚠ No EV-capable driver configured. Add one under <b>Devices</b> first ' +
          '(e.g. drivers/ctek_hybrid.lua, drivers/easee_cloud.lua).' +
          '</div>';
      }

      html += '<div class="devices-list">';
      config.loadpoints.forEach(function (lp, idx) {
        var prefix = "loadpoints." + idx;
        var stepsStr = fmtStepsW(lp.allowed_steps_w);
        var driverOpts = drivers.map(function (n) {
          return '<option value="' + escHtml(n) + '"' +
            (n === lp.driver_name ? " selected" : "") + ">" + escHtml(n) + "</option>";
        }).join("");
        if (lp.driver_name && drivers.indexOf(lp.driver_name) < 0) {
          // Show the bound driver even if it's not currently EV-tagged
          // (driver was renamed / catalog reload pending). Operator can
          // re-pick from the list to fix it.
          driverOpts = '<option value="' + escHtml(lp.driver_name) + '" selected>' +
            escHtml(lp.driver_name) + ' (not in catalog?)</option>' + driverOpts;
        }

        html +=
          '<fieldset class="device-card" data-lp-idx="' + idx + '">' +
          '<legend>Loadpoint ' + (idx + 1) + ' <span class="dim">·</span> ' + escHtml(lp.id || "(unnamed)") + '</legend>' +

          '<div class="field-row">' +
          '<div>' +
          '<label>ID ' + help("Stable identifier referenced by the planner and the dashboard EV modal. Letters/digits/dashes only.") + '</label>' +
          '<input type="text" data-path="' + prefix + '.id" value="' + escHtml(lp.id || "") + '" placeholder="garage">' +
          '</div>' +
          '<div>' +
          '<label>Charger driver ' + help("Which configured driver delivers power for this loadpoint. The dropdown lists drivers with the `ev` capability.") + '</label>' +
          '<select data-path="' + prefix + '.driver_name">' +
          '<option value="">— select driver —</option>' +
          driverOpts +
          '</select>' +
          '</div>' +
          '</div>' +

          '<div class="field-row">' +
          '<div>' +
          '<label>Min charge (W) ' + help("Lowest sustained power the charger can deliver. IEC 61851 sets a 6 A floor → 1380 W (1φ) / 4140 W (3φ) at 230 V.") + '</label>' +
          '<input type="number" min="0" step="10" data-path="' + prefix + '.min_charge_w" value="' + (lp.min_charge_w || 0) + '">' +
          '</div>' +
          '<div>' +
          '<label>Max charge (W) ' + help("Highest power the planner is allowed to request. Use the fuse limit, not the charger nameplate — leave headroom for household load.") + '</label>' +
          '<input type="number" min="0" step="10" data-path="' + prefix + '.max_charge_w" value="' + (lp.max_charge_w || 0) + '">' +
          '</div>' +
          '</div>' +

          '<div class="field-row">' +
          '<div>' +
          '<label>Vehicle capacity (Wh) ' + help("Usable battery capacity of the connected EV. Used by MPC to size the energy needed to reach the target SoC. 75000 = 75 kWh.") + '</label>' +
          '<input type="number" min="0" step="500" data-path="' + prefix + '.vehicle_capacity_wh" value="' + (lp.vehicle_capacity_wh || 0) + '">' +
          '</div>' +
          '<div>' +
          '<label>Phase mode ' + help('"3p" = always 3-phase, "1p" = always 1-phase, "auto" = controller picks per slot. Most EU installs are 3p.') + '</label>' +
          '<select data-path="' + prefix + '.phase_mode">' +
          ['3p', '1p', 'auto'].map(function (m) {
            return '<option value="' + m + '"' + (m === (lp.phase_mode || '3p') ? ' selected' : '') + '>' + m + '</option>';
          }).join('') +
          '</select>' +
          '</div>' +
          '</div>' +

          '<label>Allowed steps (W) ' + help("Discrete charging-power levels the charger can hold, comma-separated. The planner picks one per slot. Empty = use min/max as a binary on/off. CTEK 6–11 A 3φ: 4140, 4830, 5520, 6210, 6900, 7590.") + '</label>' +
          '<input type="text" data-path="' + prefix + '.allowed_steps_w__str" value="' + escHtml(stepsStr) + '" placeholder="4140, 4830, 5520, 6210, 6900, 7590">' +

          '<div style="margin-top:12px">' +
          '<button class="btn-remove" data-action="remove-lp" data-idx="' + idx + '">Remove loadpoint</button>' +
          '</div>' +
          '</fieldset>';
      });
      html += '</div>';

      html +=
        '<fieldset><legend>Add loadpoint</legend>' +
        '<div class="field-row"><div>' +
        '<label>ID</label><input type="text" id="new-lp-id" placeholder="garage">' +
        '</div><div>' +
        '<label>Charger driver</label>' +
        '<select id="new-lp-driver">' +
        '<option value="">— select driver —</option>' +
        drivers.map(function (n) {
          return '<option value="' + escHtml(n) + '">' + escHtml(n) + '</option>';
        }).join('') +
        '</select>' +
        '</div></div>' +
        '<button class="btn-add" id="new-lp-add">+ Add loadpoint</button>' +
        '</fieldset>';

      return html;
    },

    after: function (ctx) {
      var bodyEl = ctx.bodyEl, config = ctx.config;

      // Ensure catalog is loaded so evDrivers() resolves capability tags.
      // The Devices tab also primes this; calling again is cheap because
      // the response is small and the browser caches it.
      if (!S.catalogByLua) {
        ownerFetch('/api/drivers/catalog')
          .then(function (r) { return r.json(); })
          .then(function (data) {
            var byLua = {};
            (data.entries || []).forEach(function (e) {
              if (e.path) byLua[e.path] = e;
            });
            S.catalogByLua = byLua;
            // Re-render so driver dropdowns populate.
            ctx.captureCurrentTab();
            ctx.renderTab('loadpoints');
          })
          .catch(function () { /* leave dropdowns empty; user can still type */ });
      }

      // Remove handlers.
      bodyEl.querySelectorAll('[data-action="remove-lp"]').forEach(function (btn) {
        btn.addEventListener('click', function () {
          var idx = parseInt(btn.dataset.idx, 10);
          if (!isFinite(idx)) return;
          ctx.captureCurrentTab();
          config.loadpoints.splice(idx, 1);
          ctx.renderTab('loadpoints');
        });
      });

      // Add handler.
      var addBtn = document.getElementById('new-lp-add');
      if (addBtn) {
        addBtn.addEventListener('click', function () {
          var idEl = document.getElementById('new-lp-id');
          var drvEl = document.getElementById('new-lp-driver');
          var id = (idEl && idEl.value || '').trim();
          var drv = (drvEl && drvEl.value || '').trim();
          if (!id) { idEl && idEl.focus(); return; }
          // Reject duplicates — the controller treats id as the join key.
          var exists = (config.loadpoints || []).some(function (lp) { return lp.id === id; });
          if (exists) {
            alert('A loadpoint with id "' + id + '" already exists.');
            return;
          }
          ctx.captureCurrentTab();
          config.loadpoints = config.loadpoints || [];
          config.loadpoints.push({
            id: id,
            driver_name: drv,
            min_charge_w: 4140,
            max_charge_w: 11000,
            vehicle_capacity_wh: 60000,
            phase_mode: '3p',
            allowed_steps_w: [],
          });
          ctx.renderTab('loadpoints');
        });
      }

      // Translate the freeform "allowed steps" text input into the real
      // allowed_steps_w[] array on every change — the Settings shell's
      // generic capture pass writes the literal string into
      // `allowed_steps_w__str`, which isn't a real config field, so we
      // intercept and rewrite to the array.
      bodyEl.querySelectorAll('input[data-path$=".allowed_steps_w__str"]').forEach(function (inp) {
        inp.addEventListener('change', function () {
          var path = inp.dataset.path;
          var idx = parseInt(path.split('.')[1], 10);
          if (!isFinite(idx) || !config.loadpoints[idx]) return;
          config.loadpoints[idx].allowed_steps_w = parseStepsW(inp.value);
        });
        // Fire once on render so a fresh-typed value persists even if
        // the operator hits Save without leaving the field.
        inp.addEventListener('blur', function () {
          inp.dispatchEvent(new Event('change'));
        });
      });
    },
  };
})();
