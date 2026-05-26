// Settings modal shell — owns the lifecycle (open/close, fetch, save,
// tab switching) and exposes a registry so each tab can live in its own
// file under /web/settings/tabs/*.js. Tab files register themselves
// into window.FTWSettings.tabs at load time; the shell looks them up
// whenever renderTab is called.
//
// Contract for a tab file:
//
//   (function () {
//     var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
//     S.tabs = S.tabs || {};
//     S.tabs.<name> = {
//       render: function (ctx) { return htmlString; },
//       after:  function (ctx) { /* optional post-render hook */ },
//     };
//   })();
//
// ctx is built fresh on each render and exposes the shell's helpers
// (field, selectField, help, escHtml, getByPath, setByPath,
// captureCurrentTab, renderTab, bodyEl, config).
(function () {
  "use strict";

  var modal = document.getElementById("settings-modal");
  var openBtn = document.getElementById("settings-btn");
  var closeBtn = document.getElementById("settings-close");
  var saveBtn = document.getElementById("settings-save");
  var statusEl = document.getElementById("settings-status");
  var tabsEl = document.getElementById("settings-tabs");
  var bodyEl = document.getElementById("settings-body");

  if (!modal || !openBtn) return;

  // Expose the registry namespace immediately so tab files that load
  // before or after this shell can register idempotently.
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  var currentConfig = null;
  var currentTab = "control";

  openBtn.addEventListener("click", function () {
    fetch("/api/config")
      .then(function (r) { return r.json(); })
      .then(function (cfg) {
        currentConfig = cfg;
        modal.classList.remove("hidden");
        renderTab(currentTab);
        setStatus("");
      })
      .catch(function (e) {
        setStatus("Failed to load config: " + e, "error");
      });
  });

  closeBtn.addEventListener("click", function () {
    modal.classList.add("hidden");
  });
  modal.addEventListener("click", function (e) {
    if (e.target === modal) modal.classList.add("hidden");
  });

  tabsEl.addEventListener("click", function (e) {
    if (e.target.tagName === "BUTTON" && e.target.dataset.tab) {
      tabsEl.querySelectorAll("button").forEach(function (b) {
        b.classList.toggle("active", b === e.target);
      });
      captureCurrentTab();
      currentTab = e.target.dataset.tab;
      renderTab(currentTab);
    }
  });

  saveBtn.addEventListener("click", function () {
    captureCurrentTab();
    setStatus("Saving...");
    fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(currentConfig),
    })
      .then(function (r) {
        if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || ("HTTP " + r.status)); });
        return r.json();
      })
      .then(function (res) {
        setStatus("Saved ✓", "success");
        setTimeout(function () { setStatus(""); }, 2000);
        // Server tells us when the change touched a section that the
        // configreload watcher can't apply in flight (state.path,
        // api.port, …). Surface that as a dialog so the operator
        // doesn't walk away thinking their change took effect when
        // it didn't. Almost every field is hot-reloaded; the dialog
        // is meant to fire rarely.
        if (res && res.restart_required) {
          showRestartModal(res.restart_reasons || []);
        }
      })
      .catch(function (e) {
        setStatus("Save failed: " + e.message, "error");
      });
  });

  // ---- Restart-required modal ----

  function showRestartModal(reasons) {
    var modalEl = document.getElementById("restart-modal");
    var listEl = document.getElementById("restart-reasons");
    var laterBtn = document.getElementById("restart-later");
    var nowBtn = document.getElementById("restart-now");
    var progressEl = document.getElementById("restart-progress");
    var progressTextEl = document.getElementById("restart-progress-text");
    if (!modalEl || !listEl || !laterBtn || !nowBtn) return;

    listEl.innerHTML = "";
    if (reasons.length === 0) {
      var li = document.createElement("li");
      li.textContent = "(no specific reason reported)";
      li.style.color = "var(--text-dim)";
      listEl.appendChild(li);
    } else {
      reasons.forEach(function (reason) {
        var li = document.createElement("li");
        li.textContent = reason;
        listEl.appendChild(li);
      });
    }

    progressEl.classList.add("hidden");
    progressTextEl.textContent = "Restarting…";
    nowBtn.disabled = false;
    laterBtn.disabled = false;
    modalEl.classList.remove("hidden");

    laterBtn.onclick = function () { modalEl.classList.add("hidden"); };
    nowBtn.onclick = function () { triggerRestart(modalEl, nowBtn, laterBtn, progressEl, progressTextEl); };
  }

  function triggerRestart(modalEl, nowBtn, laterBtn, progressEl, progressTextEl) {
    nowBtn.disabled = true;
    laterBtn.disabled = true;
    progressEl.classList.remove("hidden");
    progressTextEl.textContent = "Restarting…";

    fetch("/api/restart", { method: "POST" })
      .then(function (r) {
        if (!r.ok) {
          return r.json()
            .catch(function () { return {}; })
            .then(function (j) { throw new Error(j.error || ("HTTP " + r.status)); });
        }
        // Server is on its way down. Wait a moment for the process to
        // exit, then poll /api/health until it answers again.
        progressTextEl.textContent = "Waiting for service…";
        setTimeout(function () { pollHealth(progressTextEl); }, 1500);
      })
      .catch(function (e) {
        nowBtn.disabled = false;
        laterBtn.disabled = false;
        progressEl.classList.add("hidden");
        alert("Restart failed: " + e.message);
      });
  }

  function pollHealth(progressTextEl) {
    var startedAt = Date.now();
    var TIMEOUT_MS = 90 * 1000; // ~90 s; sidecar pull+up can be slow on Pi.
    function tick() {
      // Cache-bust so an intermediate proxy can't lie about reachability.
      fetch("/api/health?_=" + Date.now(), { cache: "no-store" })
        .then(function (r) {
          if (r.ok) {
            progressTextEl.textContent = "Reloading…";
            // Hard reload so any new static assets (post-update) are
            // picked up — same dance ftw-update-check.js does.
            setTimeout(function () { window.location.reload(); }, 400);
            return;
          }
          throw new Error("HTTP " + r.status);
        })
        .catch(function () {
          if (Date.now() - startedAt > TIMEOUT_MS) {
            progressTextEl.textContent = "Service is taking longer than usual — reload manually when it's back.";
            return;
          }
          setTimeout(tick, 1000);
        });
    }
    tick();
  }

  function setStatus(msg, kind) {
    statusEl.textContent = msg || "";
    statusEl.className = "settings-status" + (kind ? " " + kind : "");
  }

  function captureCurrentTab() {
    var inputs = bodyEl.querySelectorAll("[data-path]");
    inputs.forEach(function (input) {
      var path = input.dataset.path;
      var val = input.type === "number" ? parseFloat(input.value) : input.value;
      if (input.type === "number" && isNaN(val)) val = 0;
      if (input.type === "number" && input.dataset.unitScale) {
        val = val * parseFloat(input.dataset.unitScale);
      }
      // Preserve a stored password when the user hasn't typed over it.
      if (input.type === "password" && val === "" && getByPath(currentConfig, path, "")) return;
      setByPath(currentConfig, path, val);
    });
  }

  function setByPath(obj, path, val) {
    var parts = path.split(".");
    var node = obj;
    for (var i = 0; i < parts.length - 1; i++) {
      if (!node[parts[i]]) node[parts[i]] = {};
      node = node[parts[i]];
    }
    node[parts[parts.length - 1]] = val;
  }

  function getByPath(obj, path, dflt) {
    var parts = path.split(".");
    var node = obj;
    for (var i = 0; i < parts.length; i++) {
      if (node == null) return dflt;
      node = node[parts[i]];
    }
    return node == null ? dflt : node;
  }

  function help(text) {
    return '<span class="help" data-help="' + escHtml(text) + '" title="' + escHtml(text) + '">?</span>';
  }

  function field(label, path, type, dflt, helpText) {
    var val = getByPath(currentConfig, path, dflt);
    return '<label>' + label + (helpText ? ' ' + help(helpText) : '') + '</label>' +
      '<input type="' + type + '" data-path="' + path + '" value="' + escHtml(val == null ? "" : String(val)) + '">';
  }

  function selectField(label, path, options, dflt, helpText) {
    var val = getByPath(currentConfig, path, dflt);
    var opts = options.map(function (o) {
      return '<option value="' + o + '"' + (o === val ? ' selected' : '') + '>' + o + '</option>';
    }).join("");
    return '<label>' + label + (helpText ? ' ' + help(helpText) : '') + '</label>' +
      '<select data-path="' + path + '">' + opts + '</select>';
  }

  function escHtml(s) {
    var div = document.createElement("div");
    div.textContent = s == null ? "" : String(s);
    return div.innerHTML;
  }

  function renderTab(tab) {
    var def = S.tabs[tab];
    if (!def) {
      bodyEl.innerHTML = '<p style="color:var(--text-dim)">Unknown tab: ' + escHtml(tab) + '</p>';
      return;
    }
    var ctx = {
      config: currentConfig,
      bodyEl: bodyEl,
      field: field,
      selectField: selectField,
      help: help,
      escHtml: escHtml,
      getByPath: getByPath,
      setByPath: setByPath,
      captureCurrentTab: captureCurrentTab,
      renderTab: renderTab,
    };
    var html = "";
    try {
      html = (def.render ? def.render(ctx) : "") || "";
    } catch (e) {
      html = '<p style="color:#e57373">Render error: ' + escHtml(e.message) + '</p>';
      console.error("tab render:", tab, e);
    }
    bodyEl.innerHTML = html;

    // Generic handler for data-checkbox-path — shared across every tab.
    bodyEl.querySelectorAll("[data-checkbox-path]").forEach(function (cb) {
      cb.addEventListener("change", function () {
        setByPath(currentConfig, cb.dataset.checkboxPath, cb.checked);
      });
    });

    if (def.after) {
      try { def.after(ctx); } catch (e) { console.error("tab after:", tab, e); }
    }
  }
})();
