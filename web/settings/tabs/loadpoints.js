// Settings → Chargers tab: connect an EV charger to the planner and set its
// electrical envelope (min/max charging power, allowed steps, vehicle
// capacity). Internally a charger binding is a "loadpoint" — the config
// field names keep that spelling. Schedules + surplus_only stay on the
// dashboard EV modal — those are operator-day decisions, not site-setup.
//
// Bound state: config.loadpoints[] — same JSON shape persisted to YAML
// (see go/internal/config/config.go::Loadpoint).
//
// Chargers come from two places: Lua drivers the catalog tags "ev", and
// OCPP charge points, which have no driver at all — they dial FTW's
// built-in central system and self-register. GET /api/ocpp/chargers is
// the live view of the latter.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  // Drivers eligible to back a charger binding = ones the catalog tags with
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

  // Set of charge-point ids seen by the OCPP server. An OCPP charger is
  // bound to a loadpoint by its identity (the last URL segment it dialed),
  // so these ids belong in the same driver dropdown as Lua EV drivers.
  function ocppChargerIds(ocppStatus) {
    var out = [];
    ((ocppStatus && ocppStatus.chargers) || []).forEach(function (c) {
      if (c.id) out.push(c.id);
    });
    return out;
  }

  // Everything a loadpoint may name as its power source, OCPP ids included.
  function evDriverNames(config, ocppStatus) {
    var out = evDrivers(config);
    ocppChargerIds(ocppStatus).forEach(function (id) {
      if (out.indexOf(id) < 0) out.push(id);
    });
    return out;
  }

  // One human-readable state per charger. Online is the WebSocket session;
  // connected is a vehicle on the connector — a charger is usually online
  // long before anything is plugged into it.
  function ocppStateLabel(c) {
    if (!c.online) return "offline";
    if (c.charging) return "charging";
    if (c.connected) return "vehicle plugged";
    return "online, no vehicle";
  }

  function fmtPowerW(w) {
    if (!w || !isFinite(w)) return "0 W";
    if (Math.abs(w) >= 1000) return (w / 1000).toFixed(1) + " kW";
    return Math.round(w) + " W";
  }

  // The OCPP block of the Chargers tab: what the server is, where a charger
  // should dial, and the live list of charge points it has seen.
  function ocppSection(status, host, escHtml) {
    var html = '<fieldset><legend>OCPP chargers</legend>';

    if (!status) {
      html += '<p style="color:var(--text-dim);font-size:0.8rem;margin:0">Checking the OCPP server…</p></fieldset>';
      return html;
    }

    if (!status.enabled) {
      html +=
        '<p style="color:var(--text-dim);font-size:0.8rem;margin:0 0 8px">' +
        'An EV charger that speaks <b>OCPP</b> needs no driver: FTW has a built-in OCPP 1.6J + 2.0.1 server, ' +
        'and the charger connects to it and registers itself. The server is currently <b>off</b>. ' +
        'Enable it in <code>config.yaml</code>:' +
        '</p>' +
        '<pre style="font-size:0.75rem;margin:0 0 8px">ocpp:\n' +
        '    enabled: true\n' +
        '    port: 8887          # OCPP 1.6J\n' +
        '    port_v201: 8888     # OCPP 2.0.1, omit to disable\n' +
        '    username: ftw\n' +
        '    password: &lt;a long random string&gt;</pre>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin:0">' +
        'Then point the charger at <code>ws://' + escHtml(host) + ':8887/&lt;charger-name&gt;</code> ' +
        'and it appears here. See <b>docs/ocpp.md</b> for per-vendor steps.' +
        '</p>';
      html += '</fieldset>';
      return html;
    }

    var port = status.port || 8887;
    var path = status.path || "/";
    html +=
      '<p style="color:var(--text-dim);font-size:0.8rem;margin:0 0 8px">' +
      'The OCPP server is <b>on</b>. A charger that speaks OCPP needs no driver — set its backend URL to ' +
      '<code>ws://' + escHtml(host) + ':' + port + escHtml(path) + '&lt;charger-name&gt;</code>' +
      (status.port_v201 ? ' (OCPP 2.0.1: port ' + status.port_v201 + ')' : '') +
      ' and it appears below. The <code>&lt;charger-name&gt;</code> you choose is the id to pick ' +
      'as the charger driver when adding it as a charger binding on this tab. Until a charger entry ' +
      'names it, a charge point is <b>pending</b>: visible here, but FTW ignores its data and never ' +
      'commands it.' +
      '</p>' +
      '<p style="color:var(--text-dim);font-size:0.8rem;margin:0 0 8px">' +
      'Give this machine a <b>DHCP reservation</b> (fixed IP) in your router first: chargers store the URL ' +
      'at commissioning time, and some also whitelist which addresses may talk to them, so an FTW host that ' +
      'changes address silently orphans every charger pointed at it. A plain DNS hostname works on most ' +
      'chargers too, but <code>.local</code> (mDNS) names usually do not — charger firmware rarely resolves them.' +
      '</p>';

    var chargers = status.chargers || [];
    if (!chargers.length) {
      html +=
        '<div class="ha-status-indicator ha-off" style="margin:0">' +
        '○ No OCPP charger has connected yet.' +
        '</div>';
    } else {
      html += '<table class="settings-table" style="width:100%;font-size:0.8rem"><thead><tr>' +
        '<th style="text-align:left">Charger</th>' +
        '<th style="text-align:left">Hardware</th>' +
        '<th style="text-align:left">OCPP</th>' +
        '<th style="text-align:left">State</th>' +
        '<th style="text-align:right">Power</th>' +
        '<th style="text-align:right">Session</th>' +
        '</tr></thead><tbody>';
      chargers.forEach(function (c) {
        var hw = [c.vendor, c.model].filter(Boolean).join(" ") || "—";
        var state = ocppStateLabel(c);
        if (c.pending) state += " · pending";
        html += '<tr' + (c.pending ? ' style="opacity:.65"' : '') + '>' +
          '<td><code>' + escHtml(c.id || "") + '</code></td>' +
          '<td>' + escHtml(hw) + '</td>' +
          '<td>' + escHtml(c.version || "?") + '</td>' +
          '<td>' + escHtml(state) + '</td>' +
          '<td style="text-align:right">' + fmtPowerW(c.power_w) + '</td>' +
          '<td style="text-align:right">' + ((c.session_wh || 0) / 1000).toFixed(2) + ' kWh</td>' +
          '</tr>';
      });
      html += '</tbody></table>';
      if (chargers.some(function (c) { return c.pending; })) {
        html +=
          '<p style="color:var(--text-dim);font-size:0.8rem;margin:8px 0 0">' +
          '<b>Pending</b> chargers are connected but not part of the site: FTW ignores their ' +
          'telemetry and never commands them, so an unknown device cannot influence dispatch. ' +
          'To adopt one, add a charger entry below with its id as the charger driver and save ' +
          '— it joins the site on that save.' +
          '</p>';
      }
    }
    html += '</fieldset>';
    return html;
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
      var ocppIds = ocppChargerIds(S.ocppStatus);
      var drivers = evDriverNames(config, S.ocppStatus);

      var html =
        '<p style="color:var(--text-dim);font-size:0.8rem;margin:0 0 12px">' +
        'A <b>charger</b> entry binds an EV charger to the planner so it can schedule charging against your tariff + PV forecast. ' +
        'The power source is either a driver added under <b>Devices</b>, or an OCPP charge point from the list below — ' +
        'pick it here and set the electrical envelope. (Config files call this binding a <code>loadpoint</code>.)' +
        '</p>';

      html += ocppSection(S.ocppStatus, (window.location && window.location.hostname) || "<ftw-host>", escHtml);

      if (!drivers.length) {
        html +=
          '<div class="ha-status-indicator ha-warn" style="margin:12px 0">' +
          '⚠ No EV-capable driver configured and no OCPP charger connected. Add a driver under <b>Devices</b> ' +
          '(e.g. drivers/ctek_hybrid.lua), or point an OCPP charger at the URL above.' +
          '</div>';
      }

      html += '<div class="devices-list">';
      config.loadpoints.forEach(function (lp, idx) {
        var prefix = "loadpoints." + idx;
        var stepsStr = fmtStepsW(lp.allowed_steps_w);
        var driverOpts = drivers.map(function (n) {
          var label = n + (ocppIds.indexOf(n) >= 0 ? " (OCPP)" : "");
          return '<option value="' + escHtml(n) + '"' +
            (n === lp.driver_name ? " selected" : "") + ">" + escHtml(label) + "</option>";
        }).join("");
        if (lp.driver_name && drivers.indexOf(lp.driver_name) < 0) {
          // Show the bound driver even if it's not currently EV-tagged
          // (driver was renamed / catalog reload pending / an OCPP charger
          // that is not connected right now). Operator can re-pick from
          // the list to fix it.
          driverOpts = '<option value="' + escHtml(lp.driver_name) + '" selected>' +
            escHtml(lp.driver_name) + ' (not connected?)</option>' + driverOpts;
        }

        html +=
          '<fieldset class="device-card" data-lp-idx="' + idx + '">' +
          '<legend>Charger ' + (idx + 1) + ' <span class="dim">·</span> ' + escHtml(lp.id || "(unnamed)") + '</legend>' +

          '<div class="field-row">' +
          '<div>' +
          '<label>ID ' + help("Stable identifier referenced by the planner and the dashboard EV modal. Letters/digits/dashes only.") + '</label>' +
          '<input type="text" data-path="' + prefix + '.id" value="' + escHtml(lp.id || "") + '" placeholder="garage">' +
          '</div>' +
          '<div>' +
          '<label>Charger driver ' + help("Which power source this charger entry commands. The dropdown lists drivers with the `ev` capability plus every OCPP charge point the built-in server has seen.") + '</label>' +
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
          '<button class="btn-remove" data-action="remove-lp" data-idx="' + idx + '">Remove charger</button>' +
          '</div>' +
          '</fieldset>';
      });
      html += '</div>';

      html +=
        '<fieldset><legend>Add charger</legend>' +
        '<div class="field-row"><div>' +
        '<label>ID</label><input type="text" id="new-lp-id" placeholder="garage">' +
        '</div><div>' +
        '<label>Charger driver</label>' +
        '<select id="new-lp-driver">' +
        '<option value="">— select driver —</option>' +
        drivers.map(function (n) {
          var label = n + (ocppIds.indexOf(n) >= 0 ? " (OCPP)" : "");
          return '<option value="' + escHtml(n) + '">' + escHtml(label) + '</option>';
        }).join('') +
        '</select>' +
        '</div></div>' +
        '<button class="btn-add" id="new-lp-add">+ Add charger</button>' +
        '</fieldset>';

      return html;
    },

    after: function (ctx) {
      var bodyEl = ctx.bodyEl, config = ctx.config;

      // Live OCPP view. Re-render only when the answer actually changed,
      // so the refetch on every tab open cannot loop.
      apiFetch('/api/ocpp/chargers')
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (data) {
          if (!data) return;
          var raw = JSON.stringify(data);
          if (raw === S._ocppStatusRaw) return;
          S._ocppStatusRaw = raw;
          S.ocppStatus = data;
          ctx.captureCurrentTab();
          ctx.renderTab('loadpoints');
        })
        .catch(function () { /* section keeps its last known state */ });

      // Ensure catalog is loaded so evDrivers() resolves capability tags.
      // The Devices tab also primes this; calling again is cheap because
      // the response is small and the browser caches it.
      if (!S.catalogByLua) {
        apiFetch('/api/drivers/catalog')
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
            alert('A charger with id "' + id + '" already exists.');
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

    // Pure helpers exposed for node tests.
    _pure: {
      evDriverNames: evDriverNames,
      ocppChargerIds: ocppChargerIds,
      ocppStateLabel: ocppStateLabel,
      ocppSection: ocppSection,
    },
  };
})();
