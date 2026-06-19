// Settings → System tab: read-only host OS resources (CPU, memory,
// disk, network IPs). CPU + memory poll every 5 s; network IPs and
// hostname/uptime come along for free in the same response.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function ownerFetch(path, opts) {
    if (typeof window.ownerFetch === "function") return window.ownerFetch(path, opts);
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
        '  .sys-bar-fill { height: 100%; background: var(--accent, #6cf); transition: width 250ms ease-out; }' +
        '  .sys-meta { color: var(--text-dim); font-size: 0.8rem; margin-top: -4px; }' +
        '  .sys-net { font-family: var(--mono, monospace); font-size: 0.85rem; }' +
        '  .sys-net-iface { color: var(--text-dim); margin-right: 8px; }' +
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
        ownerFetch("/api/system/info").then(function (r) { return r.json(); }).then(function (d) {
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

      refresh();
      if (window._systemStatusTimer) clearInterval(window._systemStatusTimer);
      window._systemStatusTimer = setInterval(refresh, 5000);
    },
  };
})();
