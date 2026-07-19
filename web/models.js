// Battery calibration flow. Fetches /api/battery_models so self-tune has
// current data, but keeps model internals out of the normal driver cards.

(function () {
  "use strict";

  const POLL_INTERVAL = 3000;
  const TUNE_POLL = 1000;
  let modelPollHandle = null;

  const grid = document.getElementById("models-grid");
  const openBtn = document.getElementById("self-tune-btn");
  const modal = document.getElementById("self-tune-modal");
  const closeBtn = document.getElementById("self-tune-close");
  const startBtn = document.getElementById("self-tune-start");
  const cancelBtn = document.getElementById("self-tune-cancel");
  const body = document.getElementById("self-tune-body");
  const statusEl = document.getElementById("self-tune-status");

  if (!grid) return;

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  let lastModels = {};
  let observeOnlyDrivers = {};
  let tunePollHandle = null;

  // Keep the latest models payload available for advanced diagnostics and
  // self-tune, but do not render model internals on normal driver cards.
  window._lastBatteryModels = lastModels;

  // ---- Model cache: refreshed once per /api/battery_models poll ----

  function fetchModels() {
    apiFetch("/api/battery_models")
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data) return;
        lastModels = data;
        window._lastBatteryModels = data;
        renderModels(data);
      })
      .catch(function () { /* silent */ });
  }

  function renderModels(models) {
    // Keep #models-grid empty. The visible surface here is only the
    // Self-tune entry point; raw model internals stay out of the dashboard.
    if (grid) grid.innerHTML = '';
  }

  // Driver cards stay operator-focused. Battery model internals are still
  // fetched for calibration and API diagnostics, but hidden from the main UI.
  window.renderInlineBatteryModel = function (name) {
    return "";
  };

  // ---- Self-tune modal ----

  function controllableTuneBatteries() {
    return Object.keys(lastModels).filter(function (n) {
      return !observeOnlyDrivers[n];
    }).sort();
  }

  function openModal() {
    modal.classList.remove("hidden");
    setStatus("");
    fetchModels();
    // Decide what to render: idle (checklist), active (progress), or done (diff)
    Promise.all([
      apiFetch("/api/self_tune/status").then(function (r) { return r.json(); }),
      apiFetch("/api/status").then(function (r) { return r.ok ? r.json() : {}; }),
    ])
      .then(function (results) {
        var s = results[0];
        var status = results[1] || {};
        observeOnlyDrivers = {};
        var drvs = status.drivers || {};
        Object.keys(drvs).forEach(function (name) {
          if (drvs[name] && drvs[name].observe_only) observeOnlyDrivers[name] = true;
        });
        return s;
      })
      .then(function (s) {
        if (s.active) {
          startBtn.style.display = "none";
          cancelBtn.style.display = "inline-block";
          renderProgress(s);
          startTunePolling();
        } else if (Object.keys(s.after || {}).length > 0) {
          startBtn.textContent = "Run again";
          startBtn.style.display = "inline-block";
          cancelBtn.style.display = "none";
          renderDiff(s);
        } else {
          startBtn.textContent = "Start calibration";
          startBtn.style.display = "inline-block";
          cancelBtn.style.display = "none";
          renderChecklist();
        }
      });
  }

  function renderChecklist() {
    var names = controllableTuneBatteries();
    var checklistHtml = names.length
      ? names.map(function (n) {
          return '<label><input type="checkbox" data-tune-battery="' + esc(n) + '" checked> ' + esc(n) + '</label>';
        }).join("")
      : '<p style="color:var(--text-dim);font-size:0.85rem;margin:0">No controllable batteries available for self-tune.</p>';
    body.innerHTML =
      '<p style="color:var(--text-dim);font-size:0.9rem;margin:0 0 8px 0">' +
      'Pause grid balancing for ~3 minutes per battery. Drives each through a known step pattern, ' +
      'fits an ARX(1) model from the response, and writes the result as the baseline for hardware-health drift detection.' +
      '</p>' +
      '<div class="self-tune-warning">' +
      '⚠ Recommended only when:<br>' +
      '&nbsp;&nbsp;• Low PV generation (cloudy or evening)<br>' +
      '&nbsp;&nbsp;• House load stable (no major appliances cycling)<br>' +
      '&nbsp;&nbsp;• Battery SoC between 30–70%' +
      '</div>' +
      '<div class="self-tune-checklist">' +
      checklistHtml +
      '</div>';
  }

  function renderProgress(s) {
    var stepNames = {
      stabilize: "Stabilize at 0W",
      step_up_small: "Small step UP (+1000W)",
      settle_up: "Settle",
      step_down_small: "Small step DOWN (-1000W)",
      settle_down: "Settle",
      step_up_large: "Large step UP (+3000W)",
      settle_high_up: "Settle",
      step_down_large: "Large step DOWN (-3000W)",
      settle_high_down: "Settle",
      fit: "Fitting model parameters...",
      done: "Done",
    };
    var stepDur = {
      stabilize: 15, step_up_small: 15, settle_up: 15,
      step_down_small: 15, settle_down: 15,
      step_up_large: 20, settle_high_up: 10,
      step_down_large: 20, settle_high_down: 10,
      fit: 1, done: 0,
    };
    var stepLabel = stepNames[s.current_step] || s.current_step;
    var stepProg = stepDur[s.current_step]
      ? Math.min(100, (s.step_elapsed_s / stepDur[s.current_step]) * 100)
      : 0;
    var totalSteps = 9; // active steps before fit
    var perBatterySec = 135; // approx total per battery
    var totalSec = perBatterySec * s.battery_total;
    var overallPct = Math.min(100, (s.total_elapsed_s / totalSec) * 100);

    body.innerHTML =
      '<div class="self-tune-progress">' +
      '<div class="self-tune-step">' +
      'Battery <strong>' + esc(s.current_battery) + '</strong> ' +
      '(' + (s.battery_index + 1) + '/' + s.battery_total + ')' +
      '</div>' +
      '<div class="self-tune-step" style="font-size:0.85rem;color:var(--text-dim)">' + esc(stepLabel) + '</div>' +
      '<div class="self-tune-bar"><div class="self-tune-bar-fill" style="width:' + stepProg + '%"></div></div>' +
      '<div class="self-tune-meta">' +
      '<span>Step: ' + s.step_elapsed_s.toFixed(0) + 's</span>' +
      '<span>Total: ' + s.total_elapsed_s.toFixed(0) + 's / ~' + totalSec + 's</span>' +
      '</div>' +
      '<div class="self-tune-bar-overall"><div class="self-tune-bar-overall-fill" style="width:' + overallPct + '%"></div></div>' +
      '</div>';
  }

  function renderDiff(s) {
    var rows = '';
    var names = Object.keys(s.after);
    names.forEach(function (n) {
      var b = s.before[n] || {};
      var a = s.after[n];
      rows += diffRow(n, "τ (s)", b.tau_s, a.tau_s, 2);
      rows += diffRow(n, "gain", b.gain, a.gain, 3);
    });
    body.innerHTML =
      '<p style="color:var(--text-dim);font-size:0.85rem;margin:0 0 8px 0">Calibration complete.</p>' +
      '<table class="tune-diff-table">' +
      '<tr><th>Battery</th><th>Param</th><th>Before</th><th>After</th><th>Δ</th></tr>' +
      rows +
      '</table>';
  }

  function diffRow(name, label, before, after, decimals) {
    if (after == null) return '';
    var b = before == null ? '–' : before.toFixed(decimals);
    var a = after.toFixed(decimals);
    var delta = (before != null) ? (after - before) : 0;
    var deltaStr = (delta > 0 ? '+' : '') + delta.toFixed(decimals);
    var cls = Math.abs(delta) < 1e-6 ? "tune-delta-neutral"
      : delta > 0 ? "tune-delta-positive" : "tune-delta-negative";
    return '<tr>' +
      '<td>' + esc(name) + '</td>' +
      '<td>' + esc(label) + '</td>' +
      '<td>' + b + '</td>' +
      '<td>' + a + '</td>' +
      '<td class="' + cls + '">' + deltaStr + '</td>' +
      '</tr>';
  }

  function startTunePolling() {
    if (tunePollHandle) return;
    tunePollHandle = setInterval(function () {
      apiFetch("/api/self_tune/status")
        .then(function (r) { return r.json(); })
        .then(function (s) {
          if (s.active) {
            renderProgress(s);
          } else {
            stopTunePolling();
            startBtn.style.display = "inline-block";
            cancelBtn.style.display = "none";
            startBtn.textContent = "Run again";
            renderDiff(s);
            // Refresh model cards once tune is done
            fetchModels();
            if (s.last_error) setStatus(s.last_error, "error");
          }
        });
    }, TUNE_POLL);
  }

  function stopTunePolling() {
    if (tunePollHandle) {
      clearInterval(tunePollHandle);
      tunePollHandle = null;
    }
  }

  function setStatus(msg, kind) {
    statusEl.textContent = msg || "";
    statusEl.className = "settings-status" + (kind ? " " + kind : "");
  }

  function closeModal() {
    modal.classList.add("hidden");
    stopTunePolling();
  }

  // Wire events
  if (openBtn) openBtn.addEventListener("click", openModal);
  if (closeBtn) closeBtn.addEventListener("click", closeModal);
  if (modal) modal.addEventListener("click", function (e) { if (e.target === modal) closeModal(); });

  if (startBtn) startBtn.addEventListener("click", function () {
    var checks = body.querySelectorAll("[data-tune-battery]");
    var batteries = [];
    checks.forEach(function (c) { if (c.checked) batteries.push(c.dataset.tuneBattery); });
    if (batteries.length === 0) {
      setStatus("Select at least one battery", "error");
      return;
    }
    setStatus("Starting...");
    apiFetch("/api/self_tune/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ batteries: batteries }),
    })
      .then(function (r) { return r.ok ? r.json() : r.json().then(function (e) { throw new Error(e.error || ("HTTP " + r.status)); }); })
      .then(function () {
        setStatus("");
        startBtn.style.display = "none";
        cancelBtn.style.display = "inline-block";
        startTunePolling();
      })
      .catch(function (e) { setStatus("Failed: " + e.message, "error"); });
  });

  if (cancelBtn) cancelBtn.addEventListener("click", function () {
    setStatus("Cancelling...");
    apiFetch("/api/self_tune/cancel", { method: "POST" })
      .then(function () {
        stopTunePolling();
        setStatus("Cancelled");
        startBtn.style.display = "inline-block";
        cancelBtn.style.display = "none";
        startBtn.textContent = "Start calibration";
        renderChecklist();
      });
  });

  function esc(s) {
    var d = document.createElement("div");
    d.textContent = s == null ? "" : String(s);
    return d.innerHTML;
  }

  function advancedVisible() {
    return !!(document.body && document.body.classList.contains("advanced"));
  }

  function startModelPolling() {
    if (modelPollHandle) return;
    fetchModels();
    modelPollHandle = setInterval(fetchModels, POLL_INTERVAL);
  }

  function stopModelPolling() {
    if (!modelPollHandle) return;
    clearInterval(modelPollHandle);
    modelPollHandle = null;
  }

  function syncModelPolling() {
    if (advancedVisible()) startModelPolling();
    else stopModelPolling();
  }

  // ---- Init ----
  document.addEventListener("ftw-ui-mode-change", syncModelPolling);
  syncModelPolling();
})();
