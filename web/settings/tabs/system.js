// Settings → System tab: read-only host OS resources (CPU, memory,
// disk, network IPs). CPU + memory poll every 5 s; network IPs and
// hostname/uptime come along for free in the same response.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  function fmtBytes(n) {
    if (!Number.isFinite(n) || n <= 0) return "—";
    var units = ["B", "KB", "MB", "GB", "TB"];
    var i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return n.toFixed(n < 10 && i > 0 ? 1 : 0) + " " + units[i];
  }

  function fmtUptime(s) {
    if (!Number.isFinite(s) || s <= 0) return "—";
    var d = Math.floor(s / 86400); s -= d * 86400;
    var h = Math.floor(s / 3600);  s -= h * 3600;
    var m = Math.floor(s / 60);
    if (d > 0) return d + "d " + h + "h";
    if (h > 0) return h + "h " + m + "m";
    return m + "m";
  }

  function optimizerStatus(optimizer) {
    optimizer = optimizer || {};
    var runtime = optimizer.runtime || {};
    var solver = optimizer.active_solver || {};
    var degraded = optimizer.degraded === true || optimizer.healthy === false || solver.fallback === true;
    if (!optimizer.configured) {
      return { label: "Core planner", degraded: false, warning: "", lastPlanAtMs: 0 };
    }
    var runtimeLabel = (optimizer.bundled_with_core ? "Energyplan " : "") + (runtime.version || "unknown") + " · " + (runtime.transport || "unknown");
    if (optimizer.role === "shadow") runtimeLabel += " · shadow";
    var solverLabel = [solver.engine, solver.backend].filter(Boolean).join(" / ");
    var reason = optimizer.fallback_reason || solver.fallback_reason || optimizer.health_error || optimizer.error || "";
    var warning = "";
    // Only the fallback flag means a failure: under the default engine the
    // sidecar is attached as a shadow and Core plans on purpose.
    if (solver.fallback) {
      warning = "Planner fallback active" + (solverLabel ? " — " + solverLabel : "") + (reason ? ". " + reason : "");
    } else if (degraded) {
      warning = "Optimizer unavailable" + (reason ? " — " + reason : "");
    }
    return {
      label: runtimeLabel + (degraded ? " · degraded" : ""),
      degraded: degraded,
      warning: warning,
      lastPlanAtMs: Number(optimizer.last_plan_at_ms) || 0,
    };
  }

  // In a single-image bundle (the Home Assistant add-on) Core and the
  // Optimizer ship and update together under one bundled FTW release, and
  // the host platform owns update and rollback. Showing per-container
  // versions and update buttons there would only mislead.
  function bundleDisplay(d) {
    d = d || {};
    var bundle = d.bundle || {};
    // Deliberately literal: /api/components' Bundle is generic over Kind, but
    // the collapsed row is not — a new bundle kind needs its own branch here,
    // and falls back to the per-component view until it gets one.
    if (bundle.kind !== "home_assistant_addon") return null;
    var ftwVersion = (d.core || {}).version || "dev";
    return {
      ftwVersion: ftwVersion,
      bundleVersion: bundle.version || "",
      note: "FTW " + ftwVersion + " is bundled with the Home Assistant add-on" +
        (bundle.version ? " " + bundle.version : "") +
        ". Home Assistant manages updates and rollback.",
    };
  }

  function bar(percent) {
    var p = Math.max(0, Math.min(100, Number(percent) || 0));
    return '<div class="sys-bar"><div class="sys-bar-fill" style="width:' + p.toFixed(1) + '%"></div></div>';
  }

  S.tabs.system = {
    render: function (ctx) {
      ctx = ctx || {};
      var escHtml = ctx.escHtml || function (s) {
        var div = document.createElement("div");
        div.textContent = s == null ? "" : String(s);
        return div.innerHTML;
      };
      function escAttr(s) {
        return String(s == null ? "" : s)
          .replace(/&/g, "&amp;")
          .replace(/"/g, "&quot;")
          .replace(/</g, "&lt;");
      }
      var a = (ctx.config && ctx.config.assistant) || {};
      var keyPlaceholder = a.has_api_key
        ? "configured — hidden, type to replace"
        : "sk-or-v1-…";
      return '' +
        '<style>' +
        '  .sys-grid { display: grid; gap: 12px; }' +
        '  .sys-row { display: grid; grid-template-columns: 110px 1fr 80px; gap: 10px; align-items: center; }' +
        '  .sys-row .sys-label { color: var(--text-dim); font-family: var(--mono, monospace); font-size: 0.85rem; }' +
        '  .sys-row .sys-value { font-family: var(--mono, monospace); font-variant-numeric: tabular-nums; text-align: right; }' +
        '  .sys-bar { width: 100%; height: 10px; border-radius: 5px; background: var(--line, rgba(255,255,255,0.08)); overflow: hidden; }' +
        '  .sys-bar-fill { height: 100%; background: var(--accent-e, #f5b942); transition: width 250ms ease-out; }' +
        '  .sys-meta { color: var(--text-dim); font-size: 0.8rem; margin-top: -4px; }' +
        '  .sys-alert { grid-column: 1 / -1; padding: 9px 11px; border: 1px solid #f59e0b; border-radius: 6px; background: rgba(245,158,11,0.12); color: var(--text, #e8e8e8); font-size: 0.82rem; line-height: 1.4; overflow-wrap: anywhere; }' +
        '  .sys-net { font-family: var(--mono, monospace); font-size: 0.85rem; }' +
        '  .sys-net-iface { color: var(--text-dim); margin-right: 8px; }' +
		'  .sys-fleet-preview { grid-column: 1 / -1; max-height: 260px; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font: 0.75rem/1.45 var(--mono, monospace); background: var(--line, rgba(255,255,255,0.06)); border-radius: 6px; padding: 10px; }' +
		'  .sys-help-actions { display:flex; gap:8px; flex-wrap:wrap; align-items:center; }' +
		'  .sys-help-secondary { margin:10px 0 0; color:var(--text-dim); font-size:0.8rem; }' +
        '</style>' +
        '<fieldset>' +
        '<legend>LAN password</legend>' +
        '<p class="sys-meta" id="sys-lan-auth-status">…</p>' +
        '<label for="sys-lan-auth-password">Password</label>' +
        '<input type="password" id="sys-lan-auth-password" autocomplete="new-password">' +
        '<label for="sys-lan-auth-confirm">Confirm</label>' +
        '<input type="password" id="sys-lan-auth-confirm" autocomplete="new-password">' +
        '<div class="sys-help-actions" style="margin-top:10px">' +
        '  <button class="btn-add" type="button" id="sys-lan-auth-enable">Enable</button>' +
        '  <button class="btn-add" type="button" id="sys-lan-auth-disable">Disable</button>' +
        '</div>' +
        '<p class="sys-help-secondary" id="sys-lan-auth-msg"></p>' +
        '<p class="sys-help-secondary">Asks for this password on the LAN before settings and writes. Live status stays visible. curl still uses Bearer.</p>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Host</legend>' +
        '<div class="sys-grid">' +
        '  <div class="sys-row">' +
        '    <span class="sys-label">Hostname</span>' +
        '    <span class="sys-value" id="sys-hostname" style="text-align:left">—</span>' +
        '    <span></span>' +
        '  </div>' +
        '  <div class="sys-row">' +
        '    <span class="sys-label">Uptime</span>' +
        '    <span class="sys-value" id="sys-uptime" style="text-align:left">—</span>' +
        '    <span></span>' +
        '  </div>' +
        '</div>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>CPU &amp; memory <span class="sys-meta">(updates every 5 s)</span></legend>' +
        '<div class="sys-grid">' +
        '  <div class="sys-row">' +
        '    <span class="sys-label">CPU</span>' +
        '    <span id="sys-cpu-bar">' + bar(0) + '</span>' +
        '    <span class="sys-value" id="sys-cpu-pct">—</span>' +
        '  </div>' +
        '  <div class="sys-meta" id="sys-cpu-cores" style="grid-column: 1 / -1; margin-left: 110px">—</div>' +
        '  <div class="sys-row">' +
        '    <span class="sys-label">Memory</span>' +
        '    <span id="sys-mem-bar">' + bar(0) + '</span>' +
        '    <span class="sys-value" id="sys-mem-pct">—</span>' +
        '  </div>' +
        '  <div class="sys-meta" id="sys-mem-detail" style="grid-column: 1 / -1; margin-left: 110px">—</div>' +
        '</div>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Disk</legend>' +
        '<div class="sys-grid">' +
        '  <div class="sys-row">' +
        '    <span class="sys-label" id="sys-disk-path">/</span>' +
        '    <span id="sys-disk-bar">' + bar(0) + '</span>' +
        '    <span class="sys-value" id="sys-disk-pct">—</span>' +
        '  </div>' +
        '  <div class="sys-meta" id="sys-disk-detail" style="grid-column: 1 / -1; margin-left: 110px">—</div>' +
        '</div>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Network</legend>' +
        '<label style="display:inline-flex;align-items:center;gap:6px;font-size:0.85rem;margin-bottom:8px">' +
        '  <input type="checkbox" id="sys-net-show-v6"> Show IPv6' +
        '</label>' +
        '<div class="sys-net" id="sys-network">—</div>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Components</legend>' +
        '<div class="sys-grid" id="sys-components">Loading component status…</div>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Ask why</legend>' +
        '<p class="sys-meta">Explains what this box is doing, including why the plan looks like it does. Paste an OpenRouter key to turn it on. You can uncheck Enable later without deleting the key. The key stays on this box. Ask why never issues driver commands.</p>' +
        '<label class="sys-check" style="display:flex;align-items:center;gap:8px;margin:8px 0">' +
        '  <input type="checkbox" data-checkbox-path="assistant.enabled"' + (a.enabled ? ' checked' : '') + '>' +
        '  Enable' +
        '</label>' +
        '<label for="sys-assistant-key">OpenRouter API key</label>' +
        '<input type="password" id="sys-assistant-key" data-path="assistant.api_key" value="" autocomplete="off" placeholder="' + escAttr(keyPlaceholder) + '">' +
        '<label for="sys-assistant-model">Model</label>' +
        '<input type="text" id="sys-assistant-model" data-path="assistant.model" value="' + escAttr(a.model || "openrouter/free") + '">' +
        '<p class="sys-help-secondary">Free key at <a href="https://openrouter.ai/keys" target="_blank" rel="noopener">openrouter.ai/keys</a>. Default <code>openrouter/free</code> stays on free models.</p>' +
        '</fieldset>' +
        '<fieldset>' +
        '<legend>Help</legend>' +
        '<div class="sys-help-actions">' +
        '  <a class="btn-add" href="https://github.com/srcfl/ftw/issues/new?template=bug_report.yml" target="_blank" rel="noopener">Report FTW bug</a>' +
        '  <a class="btn-add" href="https://github.com/srcfl/ftw/issues/new?template=feature_request.yml" target="_blank" rel="noopener">Request FTW feature</a>' +
        '</div>' +
        '<p class="sys-help-secondary">GitHub tracks bugs and features. For setup help, <a href="https://discord.gg/25xcBzQaux" target="_blank" rel="noopener">ask on Discord</a>.</p>' +
        '</fieldset>';
    },

    after: function (ctx) {
      var keyEl = document.getElementById("sys-assistant-key");
      if (keyEl) {
        keyEl.addEventListener("input", function () {
          if (!String(keyEl.value || "").trim()) return;
          var enable = document.querySelector('[data-checkbox-path="assistant.enabled"]');
          if (enable) enable.checked = true;
        });
      }
      var escHtml = (ctx && ctx.escHtml) || function (s) {
        var div = document.createElement("div");
        div.textContent = s == null ? "" : String(s);
        return div.innerHTML;
      };
      function setBar(id, pct) {
        var el = document.getElementById(id);
        if (el) el.innerHTML = bar(pct);
      }
      function setText(id, txt) {
        var el = document.getElementById(id);
        if (el) el.textContent = txt;
      }

      // IPv4 detection — anything containing ":" is IPv6 (covers fe80::,
      // ::1, full ::ffff: forms). Cheaper and more reliable than parsing.
      function isV4(ip) { return ip.indexOf(":") < 0; }

      var lastNetwork = [];
      function renderNetwork() {
        var netEl = document.getElementById("sys-network");
        if (!netEl) return;
        var showV6 = !!(document.getElementById("sys-net-show-v6") || {}).checked;
        var filtered = lastNetwork.map(function (n) {
          var ips = (n.ips || []).filter(function (ip) { return showV6 || isV4(ip); });
          return { iface: n.iface, ips: ips };
        }).filter(function (n) { return n.ips.length > 0; });
        if (!filtered.length) {
          netEl.textContent = showV6
            ? "no non-loopback interfaces"
            : "no IPv4 addresses (toggle Show IPv6 to see IPv6-only interfaces)";
          return;
        }
        netEl.innerHTML = filtered.map(function (n) {
          return '<div><span class="sys-net-iface">' + escHtml(n.iface) + '</span>' + escHtml(n.ips.join(", ")) + '</div>';
        }).join("");
      }

      var v6cb = document.getElementById("sys-net-show-v6");
      if (v6cb) v6cb.addEventListener("change", renderNetwork);

      function refresh() {
        apiFetch("/api/system/info").then(function (r) { return r.json(); }).then(function (d) {
          setText("sys-hostname", d.hostname || "—");
          setText("sys-uptime", fmtUptime(d.uptime_s));

          var cpu = d.cpu || {};
          setBar("sys-cpu-bar", cpu.percent);
          setText("sys-cpu-pct", Number.isFinite(cpu.percent) ? cpu.percent.toFixed(1) + " %" : "—");
          setText("sys-cpu-cores", (cpu.cores || "?") + " cores");

          var mem = d.memory || {};
          setBar("sys-mem-bar", mem.percent);
          setText("sys-mem-pct", Number.isFinite(mem.percent) ? mem.percent.toFixed(1) + " %" : "—");
          setText("sys-mem-detail", fmtBytes(mem.used_bytes) + " used of " + fmtBytes(mem.total_bytes));

          var disk = d.disk || {};
          setText("sys-disk-path", disk.path || "/");
          setBar("sys-disk-bar", disk.percent);
          setText("sys-disk-pct", Number.isFinite(disk.percent) ? disk.percent.toFixed(1) + " %" : "—");
          setText("sys-disk-detail", fmtBytes(disk.used_bytes) + " used of " + fmtBytes(disk.total_bytes));

          lastNetwork = Array.isArray(d.network) ? d.network : [];
          renderNetwork();
        }).catch(function () {
          setText("sys-cpu-pct", "?");
          setText("sys-mem-pct", "?");
        });
      }

      function refreshComponents() {
        apiFetch("/api/components").then(function (r) { return r.json(); }).then(function (d) {
          var el = document.getElementById("sys-components");
          if (!el) return;
          var core = d.core || {};
          var optimizer = d.optimizer || {};
          var optimizerState = optimizerStatus(optimizer);
          var drivers = d.drivers || {};
          var release = (d.updates || {}).release || {};
          var updateStatus = (d.updates || {}).status || {};
          var previousImages = updateStatus.previous_images || {};
          var active = Array.isArray(drivers.active) ? drivers.active.length : 0;
          var planTime = optimizerState.lastPlanAtMs
            ? " Last plan: " + new Date(optimizerState.lastPlanAtMs).toLocaleString() + "."
            : "";
          var bundled = bundleDisplay(d);
          var warningHTML = optimizerState.warning
            ? '<div class="sys-alert" role="alert"><strong>' + escHtml(optimizerState.warning) + '</strong>' + escHtml(planTime) + '</div>'
            : '';
          var driversHTML =
            '<div class="sys-row"><span class="sys-label">Drivers</span><span>host API ' +
              escHtml(drivers.driver_host_api || drivers.host_api || 1) + ' · ' + active +
              ' managed</span><button class="btn-add" id="sys-refresh-drivers" type="button">Refresh</button></div>';
          var actionHTML = '<div class="sys-meta" id="sys-component-action" style="grid-column:1/-1"></div>';
          if (bundled) {
            el.innerHTML =
              '<div class="sys-row"><span class="sys-label">FTW</span><span>' + escHtml(bundled.ftwVersion) +
                '</span><span class="sys-value">bundled</span></div>' +
              warningHTML +
              driversHTML +
              '<div class="sys-meta" style="grid-column:1/-1">' + escHtml(bundled.note) + '</div>' +
              actionHTML;
          } else {
            el.innerHTML =
              '<div class="sys-row"><span class="sys-label">Core</span><span>' + escHtml(core.version || "dev") +
                ' · ' + escHtml(release.channel || "native") + '</span><span class="sys-value">safety</span></div>' +
              warningHTML +
              driversHTML +
              actionHTML;
          }
          var status = document.getElementById("sys-component-action");
          var driverBtn = document.getElementById("sys-refresh-drivers");
          if (driverBtn) driverBtn.onclick = function () {
            driverBtn.disabled = true;
            if (status) status.textContent = "Refreshing signed driver manifests…";
            apiFetch("/api/device_repository/refresh", {method:"POST", headers:{"Content-Type":"application/json"}, body:"{}"})
              .then(function (r) { return r.json().then(function (body) { if (!r.ok) throw new Error(body.error || "refresh failed"); return body; }); })
              .then(function () { if (status) status.textContent = "Driver catalog refreshed; no driver was activated."; driverBtn.disabled = false; })
              .catch(function (err) { if (status) status.textContent = err.message; driverBtn.disabled = false; });
          };
        }).catch(function () {
          setText("sys-components", "Component status unavailable");
        });
      }

      function setLanMsg(txt) {
        var el = document.getElementById("sys-lan-auth-msg");
        if (el) el.textContent = txt || "";
      }

      function lanStatusText(d) {
        if (!d || typeof d !== "object") return "Status unavailable";
        if (d.lan_auth && d.configured) return "On — password is set";
        if (d.lan_auth && !d.configured) return "On — no password stored";
        if (!d.lan_auth && d.configured) return "Off — password is stored";
        return "Off";
      }

      function refreshLanAuth() {
        apiFetch("/api/auth/status").then(function (r) { return r.json(); }).then(function (d) {
          var statusEl = document.getElementById("sys-lan-auth-status");
          if (statusEl) statusEl.textContent = lanStatusText(d);
          var disableBtn = document.getElementById("sys-lan-auth-disable");
          if (disableBtn) disableBtn.disabled = !d.lan_auth;
        }).catch(function () {
          var statusEl = document.getElementById("sys-lan-auth-status");
          if (statusEl) statusEl.textContent = "Status unavailable";
        });
      }

      var enableBtn = document.getElementById("sys-lan-auth-enable");
      if (enableBtn) enableBtn.onclick = function () {
        var pwEl = document.getElementById("sys-lan-auth-password");
        var cfEl = document.getElementById("sys-lan-auth-confirm");
        var pw = pwEl ? pwEl.value : "";
        var cf = cfEl ? cfEl.value : "";
        if (pw.length < 10) {
          setLanMsg("Password must be at least 10 characters");
          return;
        }
        if (pw !== cf) {
          setLanMsg("Password and confirm do not match");
          return;
        }
        enableBtn.disabled = true;
        setLanMsg("");
        apiFetch("/api/auth/password", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ password: pw, enabled: true }),
        }).then(function (r) {
          return r.json().then(function (body) {
            if (!r.ok) throw new Error(body.error || "enable failed");
            if (pwEl) pwEl.value = "";
            if (cfEl) cfEl.value = "";
            setLanMsg("LAN password is on");
            refreshLanAuth();
          });
        }).catch(function (err) {
          setLanMsg(err.message || "enable failed");
        }).then(function () {
          enableBtn.disabled = false;
        });
      };

      var disableBtn = document.getElementById("sys-lan-auth-disable");
      if (disableBtn) disableBtn.onclick = function () {
        disableBtn.disabled = true;
        setLanMsg("");
        apiFetch("/api/auth/password", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ enabled: false }),
        }).then(function (r) {
          return r.json().then(function (body) {
            if (!r.ok) throw new Error(body.error || "disable failed");
            setLanMsg("LAN password is off");
            refreshLanAuth();
          });
        }).catch(function (err) {
          setLanMsg(err.message || "disable failed");
          disableBtn.disabled = false;
        });
      };

      refresh();
      refreshComponents();
      refreshLanAuth();
      if (window._systemStatusTimer) clearInterval(window._systemStatusTimer);
      window._systemStatusTimer = setInterval(refresh, 5000);
    },
  };
  S.tabs.system._pure = { optimizerStatus: optimizerStatus, bundleDisplay: bundleDisplay };
})();
