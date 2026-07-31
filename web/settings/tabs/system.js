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
      return { label: "Go DP only", degraded: false, warning: "", lastPlanAtMs: 0 };
    }
    var runtimeLabel = (runtime.version || "unknown") + " · " + (runtime.transport || "unknown");
    var solverLabel = [solver.engine, solver.backend].filter(Boolean).join(" / ");
    var reason = optimizer.fallback_reason || solver.fallback_reason || optimizer.health_error || optimizer.error || "";
    var warning = "";
    if (solver.fallback || solver.engine === "go-dp") {
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

  function bar(percent) {
    var p = Math.max(0, Math.min(100, Number(percent) || 0));
    return '<div class="sys-bar"><div class="sys-bar-fill" style="width:' + p.toFixed(1) + '%"></div></div>';
  }

  S.tabs.system = {
    render: function () {
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
        '<legend>Help</legend>' +
        '<div class="sys-help-actions">' +
        '  <a class="btn-add" href="https://github.com/srcfl/ftw/issues/new?template=bug_report.yml" target="_blank" rel="noopener">Report FTW bug</a>' +
        '  <a class="btn-add" href="https://github.com/srcfl/ftw/issues/new?template=feature_request.yml" target="_blank" rel="noopener">Request FTW feature</a>' +
        '</div>' +
        '<p class="sys-help-secondary">GitHub tracks bugs and features. For setup help, <a href="https://discord.gg/25xcBzQaux" target="_blank" rel="noopener">ask on Discord</a>.</p>' +
        '</fieldset>';
    },

    after: function (ctx) {
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
          el.innerHTML =
            '<div class="sys-row"><span class="sys-label">Core</span><span>' + escHtml(core.version || "dev") +
              ' · ' + escHtml(release.channel || "native") + '</span><span class="sys-value">safety</span></div>' +
            '<div class="sys-row"><span class="sys-label">Optimizer</span><span>' + escHtml(optimizerState.label) +
              '</span><span><button class="btn-add" id="sys-update-optimizer" type="button">Update</button>' +
              ((previousImages.optimizer || (updateStatus.previous_image_id && updateStatus.component === "optimizer")) ? ' <button class="btn-add" id="sys-rollback-optimizer" type="button">Rollback</button>' : '') + '</span></div>' +
            (optimizerState.warning ? '<div class="sys-alert" role="alert"><strong>' + escHtml(optimizerState.warning) + '</strong>' + escHtml(planTime) + '</div>' : '') +
            '<div class="sys-row"><span class="sys-label">Drivers</span><span>host API ' +
              escHtml(drivers.driver_host_api || drivers.host_api || 1) + ' · ' + active +
              ' managed</span><button class="btn-add" id="sys-refresh-drivers" type="button">Refresh</button></div>' +
            '<div class="sys-meta" id="sys-component-action" style="grid-column:1/-1"></div>';
          var status = document.getElementById("sys-component-action");
          var optimizerBtn = document.getElementById("sys-update-optimizer");
          if (optimizerBtn) optimizerBtn.onclick = function () {
            optimizerBtn.disabled = true;
            if (status) status.textContent = "Starting optimizer update…";
            apiFetch("/api/components/optimizer/update", {method:"POST", headers:{"Content-Type":"application/json"}, body:"{}"})
              .then(function (r) { return r.json().then(function (body) { if (!r.ok) throw new Error(body.error || "update failed"); return body; }); })
              .then(function () { if (status) status.textContent = "Optimizer update started; core remains online."; })
              .catch(function (err) { if (status) status.textContent = err.message; optimizerBtn.disabled = false; });
          };
          var rollbackBtn = document.getElementById("sys-rollback-optimizer");
          if (rollbackBtn) rollbackBtn.onclick = function () {
            rollbackBtn.disabled = true;
            if (status) status.textContent = "Restoring previous optimizer image…";
            apiFetch("/api/components/optimizer/rollback", {method:"POST", headers:{"Content-Type":"application/json"}, body:"{}"})
              .then(function (r) { return r.json().then(function (body) { if (!r.ok) throw new Error(body.error || "rollback failed"); return body; }); })
              .then(function () { if (status) status.textContent = "Optimizer rollback started; core remains online."; })
              .catch(function (err) { if (status) status.textContent = err.message; rollbackBtn.disabled = false; });
          };
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

      refresh();
      refreshComponents();
      if (window._systemStatusTimer) clearInterval(window._systemStatusTimer);
      window._systemStatusTimer = setInterval(refresh, 5000);
    },
  };
  S.tabs.system._pure = { optimizerStatus: optimizerStatus };
})();
