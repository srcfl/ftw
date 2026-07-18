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
    if (!Number.isFinite(n) || n < 0) return "—";
    if (n === 0) return "0 B";
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

  function bar(percent) {
    var p = Math.max(0, Math.min(100, Number(percent) || 0));
    return '<div class="sys-bar"><div class="sys-bar-fill" style="width:' + p.toFixed(1) + '%"></div></div>';
  }

  function storageInventoryHTML(d, escHtml) {
    d = d || {};
    var databases = d.databases || {};
    var files = d.files || {};
    var fs = d.filesystem || {};
    var advisor = d.advisor || {};
    var maintenance = d.maintenance || {};
    var incomplete = Array.isArray(d.incomplete_categories) ? d.incomplete_categories : [];

    function physical(db) {
      db = db || {};
      return (Number(db.file_bytes) || 0) + (Number(db.wal_bytes) || 0) + (Number(db.shm_bytes) || 0);
    }
    function row(label, value, detail) {
      return '<div class="sys-row sys-storage-row"><span class="sys-label">' + escHtml(label) + '</span>' +
        '<span>' + escHtml(detail || "") + '</span><span class="sys-value">' + escHtml(value) + '</span></div>';
    }
    function artifactDetail(fp) {
      fp = fp || {};
      var where = fp.on_device === false ? "external" : "on device";
      return (Number(fp.files) || 0) + " files · " + where;
    }
    function when(ms) {
      if (!Number.isFinite(ms) || ms <= 0) return "not observed yet";
      return new Date(ms).toLocaleString();
    }

    var stateDB = databases.state || {};
    var cacheDB = databases.cache || {};
    var parquet = files.parquet || {};
    var recovery = files.recovery_snapshot || {};
    var rollbacks = files.rollback_snapshots || {};
    var backups = files.full_backups || {};
    var other = files.other_data || {};
    var status = String(advisor.status || "incomplete");
    var statusClass = ["ok", "watch", "action_needed", "incomplete"].indexOf(status) >= 0 ? status : "incomplete";
    var statusLabel = status.replace(/_/g, " ");
    var rows = '';
    rows += row("state.db", fmtBytes(physical(stateDB)),
      "live " + fmtBytes(stateDB.live_bytes) + " · free pages " + fmtBytes(stateDB.free_bytes) + " · WAL " + fmtBytes(stateDB.wal_bytes));
    rows += row("cache.db", fmtBytes(physical(cacheDB)),
      "live " + fmtBytes(cacheDB.live_bytes) + " · free pages " + fmtBytes(cacheDB.free_bytes) + " · WAL " + fmtBytes(cacheDB.wal_bytes));
    rows += row("Cold Parquet", fmtBytes(parquet.bytes),
      artifactDetail(parquet) + " · diagnostics " + fmtBytes(parquet.diagnostics_bytes) +
      " · retention " + ((Number(parquet.retention_days) || 0) > 0 ? parquet.retention_days + " days" : "unbounded"));
    rows += row("Recovery copy", fmtBytes(recovery.bytes), artifactDetail(recovery));
    rows += row("Rollback points", fmtBytes(rollbacks.bytes), artifactDetail(rollbacks));
    rows += row("Full backups", fmtBytes(backups.bytes), artifactDetail(backups));
    rows += row("Other data", fmtBytes(other.bytes), "config, models, drivers and uncategorized files");

    var pressure = (Array.isArray(advisor.candidates) ? advisor.candidates : []).filter(function (candidate) {
      return candidate && candidate.would_consider;
    });
    var actions = pressure.length
      ? '<ul class="sys-storage-actions">' + pressure.map(function (candidate) {
          return '<li><b>' + escHtml(candidate.category || "storage") + ':</b> ' + escHtml(candidate.action || "review retention") + '</li>';
        }).join("") + '</ul>'
      : '<div class="sys-meta">No retention action is indicated by this dry run.</div>';
    var observed = 'Last durable outputs: Parquet ' + when(maintenance.last_parquet_success_ms) +
      ' · recovery copy ' + when(maintenance.last_recovery_snapshot_success_ms) +
      ' · verified full backup ' + when(maintenance.last_full_backup_verified_ms);
    var partial = incomplete.length
      ? '<div class="sys-storage-warning">Partial inventory: ' + escHtml(incomplete.join(", ")) + '</div>'
      : '';

    return rows +
      '<div class="sys-storage-summary"><span class="sys-storage-status sys-storage-' + statusClass + '">' +
        escHtml(statusLabel) + '</span><span>Dry-run target ' + fmtBytes(advisor.budget_bytes) +
        ' · managed ' + fmtBytes(advisor.managed_bytes) + ' · filesystem available ' + fmtBytes(fs.available_bytes) +
        ' · reserve ' + fmtBytes(advisor.filesystem_reserve_bytes) + '</span></div>' +
      partial + actions + '<div class="sys-meta sys-storage-observed">' + escHtml(observed) + '</div>' +
      '<div class="sys-meta">Read-only advisor: no files, retention settings or SQLite pages are changed.</div>';
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
        '  .sys-bar-fill { height: 100%; background: var(--accent, #6cf); transition: width 250ms ease-out; }' +
        '  .sys-meta { color: var(--text-dim); font-size: 0.8rem; margin-top: -4px; }' +
        '  .sys-net { font-family: var(--mono, monospace); font-size: 0.85rem; }' +
        '  .sys-net-iface { color: var(--text-dim); margin-right: 8px; }' +
		'  .sys-fleet-preview { grid-column: 1 / -1; max-height: 260px; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font: 0.75rem/1.45 var(--mono, monospace); background: var(--line, rgba(255,255,255,0.06)); border-radius: 6px; padding: 10px; }' +
        '  .sys-storage-row { grid-template-columns: 120px 1fr 90px; }' +
        '  .sys-storage-summary { display:flex; align-items:center; gap:10px; flex-wrap:wrap; margin-top:12px; }' +
        '  .sys-storage-status { border:1px solid var(--line); border-radius:999px; padding:2px 8px; font:0.75rem var(--mono, monospace); text-transform:uppercase; }' +
        '  .sys-storage-action_needed, .sys-storage-warning { color:var(--danger, #f88); }' +
        '  .sys-storage-watch, .sys-storage-incomplete { color:var(--warning, #fc6); }' +
        '  .sys-storage-actions { margin:10px 0; padding-left:20px; font-size:0.85rem; }' +
        '  .sys-storage-observed { margin-top:10px; }' +
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
        '<legend>FTW storage <span class="sys-meta">(read-only)</span></legend>' +
        '<div class="sys-grid" id="sys-storage-inventory">Loading storage inventory…</div>' +
        '<button class="btn-add" id="sys-storage-refresh" type="button" style="margin-top:10px">Refresh inventory</button>' +
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
          var runtime = optimizer.runtime || {};
          var drivers = d.drivers || {};
          var release = (d.updates || {}).release || {};
          var updateStatus = (d.updates || {}).status || {};
          var previousImages = updateStatus.previous_images || {};
          var active = Array.isArray(drivers.active) ? drivers.active.length : 0;
          var optimizerLabel = optimizer.configured
            ? ((runtime.version || "unknown") + " · " + (runtime.transport || "unknown") + (optimizer.healthy === false ? " · degraded" : ""))
            : "Go DP only";
          el.innerHTML =
            '<div class="sys-row"><span class="sys-label">Core</span><span>' + escHtml(core.version || "dev") +
              ' · ' + escHtml(release.channel || "native") + '</span><span class="sys-value">safety</span></div>' +
            '<div class="sys-row"><span class="sys-label">Optimizer</span><span>' + escHtml(optimizerLabel) +
              '</span><span><button class="btn-add" id="sys-update-optimizer" type="button">Update</button>' +
              ((previousImages.optimizer || (updateStatus.previous_image_id && updateStatus.component === "optimizer")) ? ' <button class="btn-add" id="sys-rollback-optimizer" type="button">Rollback</button>' : '') + '</span></div>' +
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

      function refreshStorage() {
        var el = document.getElementById("sys-storage-inventory");
        var button = document.getElementById("sys-storage-refresh");
        if (!el) return;
        if (button) button.disabled = true;
        apiFetch("/api/storage/inventory").then(function (r) {
          return r.json().then(function (body) {
            if (!r.ok) throw new Error(body.error || "storage inventory unavailable");
            return body;
          });
        }).then(function (body) {
          el.innerHTML = storageInventoryHTML(body, escHtml);
        }).catch(function (err) {
          el.textContent = err.message || "Storage inventory unavailable";
        }).finally(function () {
          if (button) button.disabled = false;
        });
      }

      var storageRefresh = document.getElementById("sys-storage-refresh");
      if (storageRefresh) storageRefresh.addEventListener("click", refreshStorage);

      refresh();
      refreshComponents();
      refreshStorage();
      if (window._systemStatusTimer) clearInterval(window._systemStatusTimer);
      window._systemStatusTimer = setInterval(refresh, 5000);
    },

    _pure: { fmtBytes: fmtBytes, storageInventoryHTML: storageInventoryHTML },
  };
})();
