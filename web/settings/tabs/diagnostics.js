// Settings → Diagnostics tab. Per-driver health, last readings, live
// metric snapshots, and the most recent log lines from the in-memory
// ring buffer. "Download support bundle" pulls a redacted gzipped tar
// the operator can attach to a developer-support email.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function fmtAge(ms) {
    if (!Number.isFinite(ms) || ms <= 0) return "—";
    var s = Math.floor(ms / 1000);
    if (s < 60) return s + "s ago";
    if (s < 3600) return Math.floor(s / 60) + "m ago";
    if (s < 86400) return Math.floor(s / 3600) + "h ago";
    return Math.floor(s / 86400) + "d ago";
  }

  function fmtW(v) {
    if (!Number.isFinite(v)) return "—";
    if (Math.abs(v) >= 1000) return (v / 1000).toFixed(2) + " kW";
    return v.toFixed(0) + " W";
  }

  function fmtNum(v, digits) {
    if (!Number.isFinite(v)) return "—";
    return v.toFixed(digits == null ? 2 : digits);
  }

  S.tabs.diagnostics = {
    render: function () {
      return '' +
        '<style>' +
        '  .diag-actions { display:flex; gap:8px; flex-wrap:wrap; margin-bottom:12px; }' +
        '  .diag-btn { font-family: var(--mono); font-size: 0.78rem; letter-spacing: 0.06em; text-transform: uppercase; padding: 8px 14px; border:1px solid var(--line); border-radius:4px; background:transparent; color:var(--fg); cursor:pointer; }' +
        '  .diag-btn:hover { border-color: var(--accent-e); color: var(--accent-e); }' +
        '  .diag-btn-primary { background: var(--accent-e); color: #0a0a0a; border-color: var(--accent-e); }' +
        '  .diag-btn-primary:hover { color: #0a0a0a; opacity: 0.9; }' +
        '  .diag-list { display:grid; gap:8px; margin-bottom:14px; }' +
        '  .diag-row { display:grid; grid-template-columns: 10px 1fr auto auto; gap:10px; align-items:center; padding:10px 12px; border:1px solid var(--line); border-radius:4px; background:var(--ink-raised); cursor:pointer; transition:border-color 120ms ease; }' +
        '  .diag-row:hover { border-color: var(--accent-e); }' +
        '  .diag-row.active { border-color: var(--accent-e); }' +
        '  .diag-dot { width:6px; height:6px; border-radius:50%; }' +
        '  .diag-dot.ok       { background: oklch(0.78 0.18 145); box-shadow: 0 0 6px oklch(0.78 0.18 145 / 0.6); }' +
        '  .diag-dot.degraded { background: oklch(0.82 0.16 80);  box-shadow: 0 0 6px oklch(0.82 0.16 80 / 0.6); }' +
        '  .diag-dot.offline  { background: oklch(0.65 0.18 25);  box-shadow: 0 0 6px oklch(0.65 0.18 25 / 0.6); }' +
        '  .diag-dot.unknown  { background: var(--fg-muted); }' +
        '  .diag-name { font-family: var(--mono); font-size: 0.92rem; }' +
        '  .diag-meta { font-family: var(--mono); font-size: 0.78rem; color: var(--fg-muted); font-variant-numeric: tabular-nums; }' +
        '  .diag-status { font-family: var(--mono); font-size: 0.72rem; letter-spacing: 0.12em; text-transform: uppercase; color: var(--fg-dim); }' +
        '  .diag-detail { border:1px solid var(--line); border-radius:4px; padding:14px 16px; background:var(--ink-raised); }' +
        '  .diag-detail h4 { margin: 0 0 8px; font-family: var(--mono); font-size: 0.72rem; letter-spacing: 0.18em; text-transform: uppercase; color: var(--fg-label); font-weight:500; }' +
        '  .diag-section + .diag-section { margin-top: 14px; padding-top: 14px; border-top: 1px solid var(--line-soft); }' +
        '  .diag-kv { display:grid; grid-template-columns: 130px 1fr; gap: 4px 12px; font-family: var(--mono); font-size: 0.85rem; font-variant-numeric: tabular-nums; }' +
        '  .diag-kv .k { color: var(--fg-muted); }' +
        '  .diag-kv .v { color: var(--fg); word-break: break-all; }' +
        '  .diag-readings { display:grid; gap: 4px; }' +
        '  .diag-reading { display:grid; grid-template-columns: 80px 1fr 1fr 100px; gap:10px; font-family:var(--mono); font-size:0.85rem; font-variant-numeric: tabular-nums; padding: 4px 0; }' +
        '  .diag-reading .label { color: var(--fg-muted); text-transform: uppercase; letter-spacing: 0.1em; font-size: 0.72rem; align-self:center; }' +
        '  .diag-reading .stale { color: oklch(0.65 0.18 25); }' +
        '  .diag-metrics { display:grid; grid-template-columns: 1fr 1fr; gap:4px 18px; font-family: var(--mono); font-size: 0.82rem; font-variant-numeric: tabular-nums; }' +
        '  .diag-metric-name { color: var(--fg-muted); }' +
        '  .diag-logs { background: var(--bg); border:1px solid var(--line-soft); border-radius:3px; padding:8px 10px; font-family: var(--mono); font-size: 0.78rem; line-height: 1.4; max-height: 280px; overflow:auto; white-space:pre; }' +
        '  .diag-log-line { display:block; }' +
        '  .diag-log-DEBUG { color: var(--fg-muted); }' +
        '  .diag-log-INFO  { color: var(--fg); }' +
        '  .diag-log-WARN  { color: oklch(0.82 0.16 80); }' +
        '  .diag-log-ERROR { color: oklch(0.7 0.18 25); }' +
        '  .diag-empty { color: var(--fg-muted); font-family: var(--mono); font-size: 0.85rem; padding: 16px 0; text-align:center; }' +
        '  .diag-error { color: oklch(0.7 0.18 25); font-family: var(--mono); font-size: 0.82rem; }' +
        '</style>' +
        '<div class="diag-actions">' +
        '  <button class="diag-btn diag-btn-primary" id="diag-dump-btn" title="Download a gzipped tarball with redacted config, driver health, recent logs, and 1 h of telemetry — small enough to email a developer.">Download support bundle</button>' +
        '  <button class="diag-btn" id="diag-refresh-btn">Refresh</button>' +
        '</div>' +
        '<h4 style="margin: 0 0 8px; font-family: var(--mono); font-size: 0.72rem; letter-spacing: 0.18em; text-transform: uppercase; color: var(--fg-label); font-weight:500;">Drivers</h4>' +
        '<div class="diag-list" id="diag-list"><div class="diag-empty">Loading…</div></div>' +
        '<div id="diag-detail-host"></div>';
    },

    after: function (ctx) {
      var escHtml = ctx.escHtml;
      var selected = null;
      var listEl = document.getElementById("diag-list");
      var detailEl = document.getElementById("diag-detail-host");
      var refreshBtn = document.getElementById("diag-refresh-btn");
      var dumpBtn = document.getElementById("diag-dump-btn");

      function statusClass(s) {
        if (s === "ok" || s === "degraded" || s === "offline") return s;
        return "unknown";
      }

      function renderList(health) {
        var names = Object.keys(health || {}).sort();
        if (!names.length) {
          listEl.innerHTML = '<div class="diag-empty">No drivers running.</div>';
          return;
        }
        var now = Date.now();
        listEl.innerHTML = names.map(function (n) {
          var h = health[n] || {};
          var st = (h.Status === 0) ? "ok"
            : (h.Status === 1) ? "degraded"
            : (h.Status === 2) ? "offline"
            : (typeof h.Status === "string") ? h.Status
            : "unknown";
          var lastSuccess = h.LastSuccess ? new Date(h.LastSuccess).getTime() : 0;
          var ageMs = lastSuccess ? (now - lastSuccess) : -1;
          var ageStr = ageMs >= 0 ? fmtAge(ageMs) : "never";
          var errs = h.ConsecutiveErrors || 0;
          var meta = "ticks " + (h.TickCount || 0) + " · " + ageStr +
            (errs ? " · " + errs + " err" : "");
          var active = (selected === n) ? " active" : "";
          return '<div class="diag-row' + active + '" data-name="' + escHtml(n) + '">' +
            '<span class="diag-dot ' + statusClass(st) + '" title="' + escHtml(st) + '"></span>' +
            '<span class="diag-name">' + escHtml(n) + '</span>' +
            '<span class="diag-meta">' + escHtml(meta) + '</span>' +
            '<span class="diag-status">' + escHtml(st) + '</span>' +
            '</div>';
        }).join("");
        listEl.querySelectorAll(".diag-row").forEach(function (row) {
          row.addEventListener("click", function () {
            selected = row.dataset.name;
            listEl.querySelectorAll(".diag-row").forEach(function (r) { r.classList.toggle("active", r.dataset.name === selected); });
            renderDetail(selected);
          });
        });
      }

      function renderDetail(name) {
        if (!name) { detailEl.innerHTML = ""; return; }
        detailEl.innerHTML = '<div class="diag-detail"><div class="diag-empty">Loading ' + escHtml(name) + '…</div></div>';
        Promise.all([
          fetch("/api/drivers/" + encodeURIComponent(name)).then(function (r) { return r.json(); }),
          fetch("/api/drivers/" + encodeURIComponent(name) + "/logs?limit=200").then(function (r) { return r.json(); }).catch(function () { return { entries: [] }; }),
        ]).then(function (results) {
          var d = results[0] || {};
          var l = results[1] || {};
          var h = d.Health || d.health || {};
          var readings = d.Readings || d.readings || [];
          var metrics = d.Metrics || d.metrics || [];
          var ident = d.Identity || d.identity || {};
          var entries = l.entries || [];

          var html = '<div class="diag-detail">';
          html += '<div class="diag-section">';
          html += '<h4>Identity</h4>';
          html += '<div class="diag-kv">';
          html += '<span class="k">make</span><span class="v">' + escHtml(ident.make || "—") + '</span>';
          html += '<span class="k">serial</span><span class="v">' + escHtml(ident.sn || "—") + '</span>';
          html += '<span class="k">mac</span><span class="v">' + escHtml(ident.mac || "—") + '</span>';
          html += '<span class="k">endpoint</span><span class="v">' + escHtml(ident.endpoint || "—") + '</span>';
          html += '</div></div>';

          html += '<div class="diag-section"><h4>Health</h4><div class="diag-kv">';
          html += '<span class="k">status</span><span class="v">' + escHtml(statusName(h.Status)) + '</span>';
          html += '<span class="k">last success</span><span class="v">' + escHtml(h.LastSuccess ? new Date(h.LastSuccess).toLocaleString() : "never") + '</span>';
          html += '<span class="k">consecutive errors</span><span class="v">' + escHtml(String(h.ConsecutiveErrors || 0)) + '</span>';
          html += '<span class="k">tick count</span><span class="v">' + escHtml(String(h.TickCount || 0)) + '</span>';
          if (h.LastError) {
            html += '<span class="k">last error</span><span class="v" style="color: oklch(0.7 0.18 25)">' + escHtml(h.LastError) + '</span>';
          }
          html += '</div></div>';

          if (readings.length) {
            html += '<div class="diag-section"><h4>Latest readings</h4><div class="diag-readings">';
            readings.forEach(function (r) {
              var staleCls = r.stale ? " stale" : "";
              var soc = (r.soc != null) ? "soc " + (r.soc * 100).toFixed(1) + "%" : "";
              html += '<div class="diag-reading' + staleCls + '">' +
                '<span class="label">' + escHtml(r.type) + '</span>' +
                '<span>raw ' + escHtml(fmtW(r.raw_w)) + '</span>' +
                '<span>smooth ' + escHtml(fmtW(r.smoothed_w)) + '</span>' +
                '<span>' + escHtml(soc || (r.updated_at_ms ? fmtAge(Date.now() - r.updated_at_ms) : "")) + '</span>' +
                '</div>';
            });
            html += '</div></div>';
          }

          if (metrics.length) {
            html += '<div class="diag-section"><h4>Live metrics</h4><div class="diag-metrics">';
            metrics.forEach(function (m) {
              html += '<span class="diag-metric-name">' + escHtml(m.name) + '</span>' +
                '<span>' + escHtml(fmtNum(m.value, 3)) + '</span>';
            });
            html += '</div></div>';
          }

          html += '<div class="diag-section"><h4>Recent log lines (' + entries.length + ')</h4>';
          if (!entries.length) {
            html += '<div class="diag-empty">No captured log lines for this driver yet.</div>';
          } else {
            html += '<div class="diag-logs">';
            // Render newest at the bottom (entries already arrive oldest-first).
            entries.forEach(function (e) {
              var ts = e.ts ? new Date(e.ts).toISOString().slice(11, 23) : "";
              var lvl = (e.level || "").toUpperCase();
              var line = ts + " " + lvl.padEnd(5) + " " + e.msg + (e.attrs ? " " + e.attrs : "");
              html += '<span class="diag-log-line diag-log-' + escHtml(lvl) + '">' + escHtml(line) + '</span>';
            });
            html += '</div>';
          }
          html += '</div>';

          html += '</div>';
          detailEl.innerHTML = html;
        }).catch(function (e) {
          detailEl.innerHTML = '<div class="diag-detail"><div class="diag-error">Failed to load: ' + escHtml(e.message) + '</div></div>';
        });
      }

      function statusName(s) {
        if (s === 0) return "ok";
        if (s === 1) return "degraded";
        if (s === 2) return "offline";
        if (typeof s === "string") return s;
        return "unknown";
      }

      function refresh() {
        fetch("/api/drivers").then(function (r) { return r.json(); }).then(function (h) {
          renderList(h);
          if (selected && h[selected]) renderDetail(selected);
        }).catch(function () {
          listEl.innerHTML = '<div class="diag-error">Failed to load /api/drivers</div>';
        });
      }

      if (refreshBtn) refreshBtn.addEventListener("click", refresh);
      if (dumpBtn) dumpBtn.addEventListener("click", function () {
        // Plain navigation triggers the browser's download manager; the
        // server sets Content-Disposition: attachment on the response.
        window.location.href = "/api/support/dump";
      });

      refresh();
      if (window._diagTimer) clearInterval(window._diagTimer);
      window._diagTimer = setInterval(refresh, 5000);
    },
  };

  // Stop the polling timer if the operator switches away from this tab.
  // settings.js calls captureCurrentTab() on tab switches so we hook the
  // global blur path: tabs without an `after` rebind don't leave the
  // interval alive.
  document.addEventListener("visibilitychange", function () {
    if (document.hidden && window._diagTimer) {
      clearInterval(window._diagTimer);
      window._diagTimer = null;
    }
  });
})();
