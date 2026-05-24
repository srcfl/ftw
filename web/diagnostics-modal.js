// Per-driver Diagnose modal.
//
// Triggered from the Diagnose button on each driver card; can be opened
// programmatically via window.FTWDiagnostics.open("driverName").
//
// Surfaces:
//   - identity (make/sn/mac/endpoint)
//   - health (status, last success, ticks, last error)
//   - latest readings (meter/pv/battery/ev/vehicle, raw + smoothed)
//   - live metric snapshots (whatever the driver emits via host.emit_metric)
//   - last 200 log lines from the in-memory ring buffer
//   - a "Download support bundle" button that pulls the global dump
//
// Auto-refreshes the open driver every 5 s. Background refreshes swap
// content silently and preserve modal/log scroll positions so the
// operator's reading place isn't yanked away.
(function () {
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
  function escHtml(s) {
    var div = document.createElement("div");
    div.textContent = s == null ? "" : String(s);
    return div.innerHTML;
  }
  function statusName(s) {
    if (s === 0) return "ok";
    if (s === 1) return "degraded";
    if (s === 2) return "offline";
    if (typeof s === "string") return s;
    return "unknown";
  }
  function statusClass(s) {
    if (s === "ok" || s === "degraded" || s === "offline") return s;
    return "unknown";
  }

  // Inject modal CSS once. Tokens come from theme.css (DESIGN.md).
  function ensureStyles() {
    if (document.getElementById("ftw-diag-modal-styles")) return;
    var style = document.createElement("style");
    style.id = "ftw-diag-modal-styles";
    style.textContent = [
      // Backdrop + shell — flat, hairline border, ink-raised surface.
      // No drop shadow on the modal itself per DESIGN.md.
      ".ftw-diag-backdrop{position:fixed;inset:0;background:rgba(0,0,0,0.55);z-index:9000;display:flex;align-items:center;justify-content:center;}",
      ".ftw-diag-shell{width:min(740px,94vw);max-height:90vh;display:flex;flex-direction:column;background:var(--ink-raised);border:1px solid var(--line);border-radius:10px;overflow:hidden;}",
      // Header: eyebrow + driver name + status pill on the left,
      // ghost actions on the right.
      ".ftw-diag-head{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:14px 18px;border-bottom:1px solid var(--line);background:var(--ink);}",
      ".ftw-diag-title{display:flex;align-items:center;gap:12px;flex-wrap:wrap;}",
      // Eyebrow label per DESIGN.md: mono, 0.18em tracking, accent amber.
      ".ftw-diag-title-eyebrow{font-family:var(--mono);font-size:0.7rem;letter-spacing:0.18em;text-transform:uppercase;color:var(--accent-e);font-weight:500;}",
      ".ftw-diag-title-name{font-family:var(--mono);font-size:1rem;color:var(--fg);font-weight:500;}",
      // Status pill: 999px per DESIGN.md, status dot uses the sanctioned
      // accent glow on a 6 px dot. Pill text colour follows status.
      ".ftw-diag-status-pill{display:inline-flex;align-items:center;gap:6px;font-family:var(--mono);font-size:0.7rem;letter-spacing:0.12em;text-transform:uppercase;padding:3px 10px;border:1px solid var(--line);border-radius:999px;color:var(--fg-dim);}",
      ".ftw-diag-status-pill::before{content:'';width:6px;height:6px;border-radius:999px;background:var(--line);}",
      ".ftw-diag-status-pill.ftw-diag-status-ok{color:var(--accent-e);border-color:var(--accent-e);}",
      ".ftw-diag-status-pill.ftw-diag-status-ok::before{background:var(--green-e);box-shadow:0 0 10px var(--green-e);}",
      ".ftw-diag-status-pill.ftw-diag-status-degraded{color:var(--accent-e);border-color:var(--accent-e);}",
      ".ftw-diag-status-pill.ftw-diag-status-degraded::before{background:var(--accent-e);box-shadow:0 0 10px var(--accent-e);}",
      ".ftw-diag-status-pill.ftw-diag-status-offline{color:var(--red-e);border-color:var(--red-e);}",
      ".ftw-diag-status-pill.ftw-diag-status-offline::before{background:var(--red-e);box-shadow:0 0 10px var(--red-e);}",
      ".ftw-diag-actions{display:flex;gap:8px;align-items:center;}",
      // Ghost button per DESIGN.md: transparent bg, hover changes ONLY
      // the border colour (no bg or text-colour shift).
      ".ftw-diag-btn{font-family:var(--sans);font-size:0.78rem;font-weight:500;letter-spacing:0.02em;padding:8px 14px;border:1px solid var(--line);border-radius:8px;background:transparent;color:var(--fg);cursor:pointer;transition:border-color 120ms ease;}",
      ".ftw-diag-btn:hover{border-color:var(--fg-dim);}",
      // Primary CTA: amber bg, near-black text, lift on hover (no colour shift).
      ".ftw-diag-btn-primary{background:var(--accent-e);color:#0a0a0a;border-color:var(--accent-e);}",
      ".ftw-diag-btn-primary:hover{border-color:var(--accent-e);transform:translateY(-1px);}",
      // Close glyph: bare ghost, hover lifts to fg.
      ".ftw-diag-close{font-size:1.4rem;line-height:1;background:transparent;border:none;color:var(--fg-muted);cursor:pointer;padding:4px 8px;}",
      ".ftw-diag-close:hover{color:var(--fg);}",
      ".ftw-diag-body{padding:16px 18px;overflow:auto;flex:1;background:var(--ink-raised);}",
      // Sections: hairline divider, eyebrow heading in amber.
      ".ftw-diag-section + .ftw-diag-section{margin-top:16px;padding-top:16px;border-top:1px solid var(--line-soft);}",
      ".ftw-diag-section h4{margin:0 0 10px;font-family:var(--mono);font-size:0.7rem;letter-spacing:0.18em;text-transform:uppercase;color:var(--accent-e);font-weight:500;}",
      // Key/value grid for identity + health.
      ".ftw-diag-kv{display:grid;grid-template-columns:140px 1fr;gap:6px 14px;font-family:var(--mono);font-size:0.84rem;font-variant-numeric:tabular-nums;}",
      ".ftw-diag-kv .k{color:var(--fg-muted);}",
      ".ftw-diag-kv .v{color:var(--fg);word-break:break-all;}",
      ".ftw-diag-kv .v.err{color:var(--red-e);}",
      // Reading rows: type label, raw, smoothed, soc/age.
      ".ftw-diag-readings{display:grid;gap:2px;}",
      ".ftw-diag-reading{display:grid;grid-template-columns:80px 1fr 1fr 110px;gap:10px;font-family:var(--mono);font-size:0.84rem;font-variant-numeric:tabular-nums;padding:5px 0;border-bottom:1px solid var(--line-soft);}",
      ".ftw-diag-reading:last-child{border-bottom:none;}",
      ".ftw-diag-reading .label{color:var(--accent-e);text-transform:uppercase;letter-spacing:0.14em;font-size:0.68rem;align-self:center;}",
      ".ftw-diag-reading .num{color:var(--fg);}",
      ".ftw-diag-reading .num-dim{color:var(--fg-dim);}",
      ".ftw-diag-reading.stale .num,.ftw-diag-reading.stale .num-dim,.ftw-diag-reading.stale .meta{color:var(--red-e);}",
      ".ftw-diag-reading .meta{color:var(--fg-muted);text-align:right;}",
      // Metrics list: name dim, value strong.
      ".ftw-diag-metrics{display:grid;grid-template-columns:1fr auto 1fr auto;gap:4px 18px;font-family:var(--mono);font-size:0.82rem;font-variant-numeric:tabular-nums;}",
      ".ftw-diag-metric-name{color:var(--fg-muted);}",
      ".ftw-diag-metric-val{color:var(--fg);text-align:right;}",
      // Log tail: ink-sunken (recessed), accent-tinted hairline border.
      ".ftw-diag-logs{background:var(--ink-sunken);border:1px solid var(--line);border-radius:8px;padding:10px 12px;font-family:var(--mono);font-size:0.78rem;line-height:1.5;max-height:300px;overflow:auto;white-space:pre;}",
      ".ftw-diag-log-line{display:block;}",
      ".ftw-diag-log-DEBUG{color:var(--fg-muted);}",
      ".ftw-diag-log-INFO{color:var(--fg);}",
      ".ftw-diag-log-WARN{color:var(--accent-e);}",
      ".ftw-diag-log-ERROR{color:var(--red-e);}",
      ".ftw-diag-empty{color:var(--fg-muted);font-family:var(--mono);font-size:0.84rem;padding:14px 0;text-align:center;}",
      ".ftw-diag-error{color:var(--red-e);font-family:var(--mono);font-size:0.82rem;}",
      ".ftw-diag-job{position:fixed;inset:0;background:rgba(0,0,0,0.62);z-index:9100;display:flex;align-items:center;justify-content:center;}",
      ".ftw-diag-job-card{width:min(420px,90vw);background:var(--ink-raised);border:1px solid var(--line);border-radius:10px;padding:18px;text-align:center;color:var(--fg);}",
      ".ftw-diag-job-spinner{display:inline-block;width:22px;height:22px;border:2px solid var(--line);border-top-color:var(--accent-e);border-radius:50%;animation:ftwDiagSpin 0.9s linear infinite;margin-bottom:10px;}",
      "@keyframes ftwDiagSpin{to{transform:rotate(360deg);}}",
      ".ftw-diag-job-title{font-family:var(--mono);font-size:0.85rem;letter-spacing:0.12em;text-transform:uppercase;color:var(--accent-e);margin-bottom:8px;}",
      ".ftw-diag-job-msg{font-family:var(--sans);font-size:0.9rem;color:var(--fg);margin-bottom:6px;}",
      ".ftw-diag-job-meta{font-family:var(--mono);font-size:0.78rem;color:var(--fg-dim);}",
      ".ftw-diag-job-error{color:var(--red-e);font-family:var(--mono);font-size:0.82rem;margin-top:10px;}",
      ".ftw-diag-job-actions{display:flex;justify-content:center;gap:8px;margin-top:14px;}",
    ].join("");
    document.head.appendChild(style);
  }

  var state = {
    name: null,
    backdrop: null,
    bodyEl: null,
    statusPillEl: null,
    timer: null,
    keyHandler: null,
    jobOverlay: null,
    jobTimer: null,
  };

  function close() {
    if (state.timer) { clearInterval(state.timer); state.timer = null; }
    if (state.keyHandler) {
      document.removeEventListener("keydown", state.keyHandler);
      state.keyHandler = null;
    }
    if (state.backdrop && state.backdrop.parentNode) {
      state.backdrop.parentNode.removeChild(state.backdrop);
    }
    state.backdrop = null;
    state.bodyEl = null;
    state.statusPillEl = null;
    state.name = null;
  }

  function startJobOverlay(title, message) {
    ensureStyles();
    stopJobOverlay();
    var started = Date.now();
    var overlay = document.createElement("div");
    overlay.className = "ftw-diag-job";
    overlay.innerHTML =
      '<div class="ftw-diag-job-card" role="dialog" aria-modal="true" aria-live="polite">' +
      '  <span class="ftw-diag-job-spinner"></span>' +
      '  <div class="ftw-diag-job-title">' + escHtml(title) + '</div>' +
      '  <div class="ftw-diag-job-msg">' + escHtml(message) + '</div>' +
      '  <div class="ftw-diag-job-meta" data-role="meta">Elapsed: 0s</div>' +
      '</div>';
    document.body.appendChild(overlay);
    state.jobOverlay = overlay;
    state.jobTimer = setInterval(function () {
      var meta = overlay.querySelector('[data-role="meta"]');
      if (meta) meta.textContent = "Elapsed: " + Math.round((Date.now() - started) / 1000) + "s";
    }, 1000);
  }

  function stopJobOverlay() {
    if (state.jobTimer) {
      clearInterval(state.jobTimer);
      state.jobTimer = null;
    }
    if (state.jobOverlay && state.jobOverlay.parentNode) {
      state.jobOverlay.parentNode.removeChild(state.jobOverlay);
    }
    state.jobOverlay = null;
  }

  function failJobOverlay(message) {
    if (!state.jobOverlay) return;
    if (state.jobTimer) {
      clearInterval(state.jobTimer);
      state.jobTimer = null;
    }
    var card = state.jobOverlay.querySelector(".ftw-diag-job-card");
    if (!card) return;
    card.innerHTML =
      '<div class="ftw-diag-job-title">Download failed</div>' +
      '<div class="ftw-diag-job-error">' + escHtml(message || "Request failed") + '</div>' +
      '<div class="ftw-diag-job-actions"><button class="ftw-diag-btn ftw-diag-btn-primary" data-role="close-job">Close</button></div>';
    card.querySelector('[data-role="close-job"]').addEventListener("click", stopJobOverlay);
  }

  function filenameFromResponse(resp, fallback) {
    var cd = resp.headers.get("Content-Disposition") || resp.headers.get("content-disposition") || "";
    var m = cd.match(/filename\*=UTF-8''([^;]+)|filename="?([^";]+)"?/i);
    if (m) {
      try { return decodeURIComponent(m[1] || m[2]); } catch (_) { return m[1] || m[2]; }
    }
    return fallback;
  }

  function downloadWithFeedback(url, title, message, fallbackName) {
    startJobOverlay(title, message);
    fetch(url)
      .then(function (resp) {
        if (!resp.ok) {
          return resp.text().then(function (txt) {
            throw new Error(txt || ("HTTP " + resp.status));
          });
        }
        var filename = filenameFromResponse(resp, fallbackName);
        return resp.blob().then(function (blob) {
          var objectURL = URL.createObjectURL(blob);
          var a = document.createElement("a");
          a.href = objectURL;
          a.download = filename;
          document.body.appendChild(a);
          a.click();
          a.remove();
          setTimeout(function () { URL.revokeObjectURL(objectURL); }, 30000);
          stopJobOverlay();
        });
      })
      .catch(function (err) {
        failJobOverlay(err && err.message ? err.message : String(err));
      });
  }

  function renderBody(d, logs, isFirstPaint) {
    var h = d.Health || d.health || {};
    var readings = d.Readings || d.readings || [];
    var metrics = d.Metrics || d.metrics || [];
    var ident = d.Identity || d.identity || {};
    var entries = (logs && logs.entries) || [];

    if (state.statusPillEl) {
      var st = statusName(h.Status);
      state.statusPillEl.textContent = st;
      state.statusPillEl.className = "ftw-diag-status-pill ftw-diag-status-" + statusClass(st);
    }

    var html = "";
    html += '<div class="ftw-diag-section"><h4>Identity</h4><div class="ftw-diag-kv">';
    html += '<span class="k">make</span><span class="v">' + escHtml(ident.make || "—") + '</span>';
    html += '<span class="k">serial</span><span class="v">' + escHtml(ident.sn || "—") + '</span>';
    html += '<span class="k">mac</span><span class="v">' + escHtml(ident.mac || "—") + '</span>';
    html += '<span class="k">endpoint</span><span class="v">' + escHtml(ident.endpoint || "—") + '</span>';
    html += '</div></div>';

    html += '<div class="ftw-diag-section"><h4>Health</h4><div class="ftw-diag-kv">';
    html += '<span class="k">status</span><span class="v">' + escHtml(statusName(h.Status)) + '</span>';
    html += '<span class="k">last success</span><span class="v">' + escHtml(h.LastSuccess ? new Date(h.LastSuccess).toLocaleString() : "never") + '</span>';
    html += '<span class="k">consecutive errors</span><span class="v">' + escHtml(String(h.ConsecutiveErrors || 0)) + '</span>';
    html += '<span class="k">tick count</span><span class="v">' + escHtml(String(h.TickCount || 0)) + '</span>';
    if (h.LastError) {
      html += '<span class="k">last error</span><span class="v err">' + escHtml(h.LastError) + '</span>';
    }
    html += '</div></div>';

    if (readings.length) {
      html += '<div class="ftw-diag-section"><h4>Latest readings</h4><div class="ftw-diag-readings">';
      readings.forEach(function (r) {
        var staleCls = r.stale ? " stale" : "";
        var soc = (r.soc != null) ? "soc " + (r.soc * 100).toFixed(1) + "%" : "";
        html += '<div class="ftw-diag-reading' + staleCls + '">' +
          '<span class="label">' + escHtml(r.type) + '</span>' +
          '<span class="num">raw ' + escHtml(fmtW(r.raw_w)) + '</span>' +
          '<span class="num-dim">smooth ' + escHtml(fmtW(r.smoothed_w)) + '</span>' +
          '<span class="meta">' + escHtml(soc || (r.updated_at_ms ? fmtAge(Date.now() - r.updated_at_ms) : "")) + '</span>' +
          '</div>';
      });
      html += '</div></div>';
    }

    if (metrics.length) {
      html += '<div class="ftw-diag-section"><h4>Live metrics</h4><div class="ftw-diag-metrics">';
      metrics.forEach(function (m) {
        html += '<span class="ftw-diag-metric-name">' + escHtml(m.name) + '</span>' +
          '<span class="ftw-diag-metric-val">' + escHtml(fmtNum(m.value, 3)) + '</span>';
      });
      html += '</div></div>';
    }

    html += '<div class="ftw-diag-section"><h4>Recent log lines (' + entries.length + ')</h4>';
    if (!entries.length) {
      html += '<div class="ftw-diag-empty">No captured log lines for this driver yet.</div>';
    } else {
      html += '<div class="ftw-diag-logs">';
      entries.forEach(function (e) {
        var ts = e.ts ? new Date(e.ts).toISOString().slice(11, 23) : "";
        var lvl = (e.level || "").toUpperCase();
        var line = ts + " " + lvl.padEnd(5) + " " + e.msg + (e.attrs ? " " + e.attrs : "");
        html += '<span class="ftw-diag-log-line ftw-diag-log-' + escHtml(lvl) + '">' + escHtml(line) + '</span>';
      });
      html += '</div>';
    }
    html += '</div>';

    // Preserve scroll positions across the silent re-render so the
    // 5 s auto-refresh doesn't yank the operator off what they're
    // reading. On first paint there's nothing to preserve.
    var prevBodyTop = state.bodyEl.scrollTop;
    var prevLogs = state.bodyEl.querySelector(".ftw-diag-logs");
    var prevLogsTop = prevLogs ? prevLogs.scrollTop : 0;
    var prevPinned = prevLogs
      ? Math.abs((prevLogs.scrollTop + prevLogs.clientHeight) - prevLogs.scrollHeight) < 4
      : true;

    state.bodyEl.innerHTML = html;

    if (!isFirstPaint) state.bodyEl.scrollTop = prevBodyTop;
    var newLogs = state.bodyEl.querySelector(".ftw-diag-logs");
    if (newLogs) {
      // Pin to bottom on first paint OR if the operator was already
      // reading the freshest line; otherwise hold their exact spot.
      newLogs.scrollTop = (isFirstPaint || prevPinned) ? newLogs.scrollHeight : prevLogsTop;
    }
  }

  function fetchAndRender(isFirstPaint) {
    var n = state.name;
    if (!n) return;
    Promise.all([
      fetch("/api/drivers/" + encodeURIComponent(n)).then(function (r) { return r.json(); }),
      fetch("/api/drivers/" + encodeURIComponent(n) + "/logs?limit=200").then(function (r) { return r.json(); }).catch(function () { return { entries: [] }; }),
    ]).then(function (results) {
      if (state.name !== n) return; // user closed or switched
      renderBody(results[0] || {}, results[1] || {}, isFirstPaint);
    }).catch(function (e) {
      if (state.name !== n) return;
      state.bodyEl.innerHTML = '<div class="ftw-diag-error">Failed to load: ' + escHtml(e.message) + '</div>';
    });
  }

  function open(name) {
    if (!name) return;
    ensureStyles();
    if (state.backdrop) close();

    var backdrop = document.createElement("div");
    backdrop.className = "ftw-diag-backdrop";
    backdrop.innerHTML =
      '<div class="ftw-diag-shell" role="dialog" aria-modal="true">' +
      '  <div class="ftw-diag-head">' +
      '    <div class="ftw-diag-title">' +
      '      <span class="ftw-diag-title-eyebrow">Diagnose</span>' +
      '      <span class="ftw-diag-title-name">' + escHtml(name) + '</span>' +
      '      <span class="ftw-diag-status-pill" data-role="status">…</span>' +
      '    </div>' +
      '    <div class="ftw-diag-actions">' +
      '      <button class="ftw-diag-btn" data-role="dump" title="Download a gzipped tarball with recent logs, redacted config, driver health, and 1 h of telemetry — small enough to email a developer.">Download recent logs</button>' +
      '      <button class="ftw-diag-btn" data-role="research" title="Download an anonymized 120-day load research bundle with 15-minute site aggregates, EV split out, weather, prices, and load-model state.">Download load research</button>' +
      '      <button class="ftw-diag-close" data-role="close" aria-label="Close">×</button>' +
      '    </div>' +
      '  </div>' +
      '  <div class="ftw-diag-body" data-role="body"><div class="ftw-diag-empty">Loading ' + escHtml(name) + '…</div></div>' +
      '</div>';
    document.body.appendChild(backdrop);

    state.backdrop = backdrop;
    state.name = name;
    state.bodyEl = backdrop.querySelector('[data-role="body"]');
    state.statusPillEl = backdrop.querySelector('[data-role="status"]');

    backdrop.addEventListener("click", function (ev) {
      if (ev.target === backdrop) close();
    });
    backdrop.querySelector('[data-role="close"]').addEventListener("click", close);
    backdrop.querySelector('[data-role="dump"]').addEventListener("click", function () {
      downloadWithFeedback(
        "/api/support/dump",
        "Preparing support bundle",
        "Collecting logs, redacted config, driver health, and recent telemetry.",
        "forty-two-watts-support.tar.gz"
      );
    });
    backdrop.querySelector('[data-role="research"]').addEventListener("click", function () {
      downloadWithFeedback(
        "/api/research/load/dump?days=120",
        "Preparing load research",
        "Building the anonymized 120-day research bundle. This can take a while on large databases.",
        "forty-two-watts-load-research.tar.gz"
      );
    });

    state.keyHandler = function (ev) { if (ev.key === "Escape") close(); };
    document.addEventListener("keydown", state.keyHandler);

    fetchAndRender(true);
    if (state.timer) clearInterval(state.timer);
    state.timer = setInterval(function () { fetchAndRender(false); }, 5000);
  }

  window.FTWDiagnostics = { open: open, close: close };
})();
