// Settings → Calendar tab: CalDAV planner-constraints client (#498).
// 42W can manage the Radicale credential itself and show it here (with a QR)
// to add to a phone/desktop calendar app. All server-supplied values are
// rendered via textContent / DOM nodes (never innerHTML) so a hostile or
// MITM'd CalDAV server cannot inject markup into this gated settings page.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function ownerFetch(path, opts) {
    if (typeof window.ownerFetch === "function") return window.ownerFetch(path, opts);
    return fetch(path, opts);
  }

  // Calendar apps on the LAN can't reach "localhost" (that's the 42W↔Radicale
  // link); rewrite it to the host serving this dashboard.
  function lanURL(u) {
    if (!u) return u;
    try {
      var url = new URL(u);
      if (url.hostname === "localhost" || url.hostname === "127.0.0.1") {
        url.hostname = window.location.hostname;
      }
      return url.toString();
    } catch (e) {
      return u;
    }
  }

  // el is a tiny safe DOM builder — text is set via textContent, never parsed.
  function el(tag, props, kids) {
    var n = document.createElement(tag);
    if (props) {
      Object.keys(props).forEach(function (k) {
        if (k === "text") n.textContent = props[k];
        else if (k === "class") n.className = props[k];
        else n.setAttribute(k, props[k]);
      });
    }
    (kids || []).forEach(function (c) {
      if (c) n.appendChild(c);
    });
    return n;
  }

  function copyBtn(getText) {
    var b = el("button", { class: "copy-btn", type: "button", text: "Copy" });
    b.addEventListener("click", function () {
      var t = getText();
      if (navigator.clipboard) navigator.clipboard.writeText(t);
      b.textContent = "Copied";
      setTimeout(function () { b.textContent = "Copy"; }, 1200);
    });
    return b;
  }

  function urlRow(label, url) {
    var u = lanURL(url);
    return el("div", { class: "field-row" }, [
      el("label", { text: label }),
      el("code", { class: "subscribe-url", text: u }),
      copyBtn(function () { return u; }),
    ]);
  }

  // drawQR paints qrMatrix(text) into a <canvas> (mirrors owner-access QR).
  function drawQR(qrMatrix, text, target) {
    var matrix = qrMatrix(text);
    var n = matrix.length, quiet = 4, total = n + 2 * quiet;
    var px = Math.max(2, Math.floor((target || 220) / total));
    var size = total * px;
    var canvas = el("canvas");
    canvas.width = size; canvas.height = size;
    canvas.style.width = size + "px"; canvas.style.height = size + "px";
    var ctx = canvas.getContext("2d");
    ctx.fillStyle = "#ffffff"; ctx.fillRect(0, 0, size, size);
    ctx.fillStyle = "#0a0a0a";
    for (var r = 0; r < n; r++) {
      for (var c = 0; c < n; c++) {
        if (matrix[r][c]) ctx.fillRect((c + quiet) * px, (r + quiet) * px, px, px);
      }
    }
    return canvas;
  }

  S.tabs.caldav = {
    render: function (ctx) {
      var field = ctx.field, config = ctx.config;
      if (!config.caldav) config.caldav = {};
      var managed = config.caldav.manage_credentials !== false; // default on
      var html =
        '<div id="caldav-status-indicator" class="ha-status-indicator">checking…</div>' +
        '<fieldset><legend>Calendar (CalDAV)</legend>' +
        '<p class="hint">42W reads a calendar served by the bundled Radicale sidecar and turns events into planner constraints. It stays on your local network — nothing here is exposed to the internet.</p>' +
        '<label><input type="checkbox" data-checkbox-path="caldav.enabled"' + (config.caldav.enabled ? " checked" : "") + "> Enabled</label>" +
        '<label><input type="checkbox" data-checkbox-path="caldav.manage_credentials"' + (managed ? " checked" : "") + "> Let 42W manage the calendar password (recommended)</label>" +
        '<div class="field-row"><div>' +
        field("Server URL", "caldav.url", "text", "http://localhost:5232", "Base URL of the CalDAV server. Defaults to the bundled Radicale sidecar.") +
        "</div><div>" +
        field("Calendar path", "caldav.calendar_path", "text", "/fortytwowatts/energy/", "Collection 42W reads events from.") +
        "</div></div>" +
        field("Username", "caldav.username", "text", "fortytwowatts");
      if (!managed) {
        html += field("Password", "caldav.password", "password", "", "Only used when 42W is not managing the password.");
      }
      html +=
        '<div class="field-row"><div>' +
        field("Poll interval (s)", "caldav.poll_interval_s", "number", 300) +
        "</div><div>" +
        field("Default EV target SoC (%)", "caldav.ev_default_target_soc_pct", "number", 80) +
        "</div></div>" +
        '<label><input type="checkbox" data-checkbox-path="caldav.evse_history"' + (config.caldav.evse_history === false ? "" : " checked") + "> Write EVSE usage as calendar history</label>" +
        '<label><input type="checkbox" data-checkbox-path="caldav.publish_plan"' + (config.caldav.publish_plan === false ? "" : " checked") + "> Publish the planner's upcoming charge/discharge windows</label>" +
        '<p class="hint" style="margin-top:6px">Title an event <b>Away</b> or <b>Vacation</b> to conserve battery while you\'re out. Title one <b>Charge car 80%</b> (with your departure as the event time) so the car is ready in time.</p>' +
        '<div id="caldav-creds"></div>' +
        '<div id="caldav-urls"></div>' +
        '<div id="caldav-qr" class="caldav-qr"></div>' +
        "</fieldset>";
      return html;
    },
    after: function () {
      var ind = document.getElementById("caldav-status-indicator");
      var credsEl = document.getElementById("caldav-creds");
      var urlsEl = document.getElementById("caldav-urls");
      var qrEl = document.getElementById("caldav-qr");

      function refreshStatus() {
        ownerFetch("/api/caldav/status").then(function (r) { return r.json(); }).then(function (d) {
          if (!ind) return;
          if (!d.enabled) {
            ind.className = "ha-status-indicator ha-off";
            ind.textContent = "○  disabled in config";
            return;
          }
          if (d.available === false) {
            ind.className = "ha-status-indicator ha-warn";
            ind.textContent = (d.unavailable_reason === "ha-addon")
              ? "⚠  Not available as a Home Assistant add-on. The calendar needs the Radicale sidecar (GPLv3) running in its own container, which the single-container add-on can't provide. See docs/caldav-integration.md."
              : "⚠  Calendar unavailable in this install.";
            if (credsEl) credsEl.textContent = "";
            if (urlsEl) urlsEl.textContent = "";
            if (qrEl) qrEl.textContent = "";
            return;
          }
          if (d.reachable) {
            var age = d.last_sync_ms > 0 ? Math.round((Date.now() - d.last_sync_ms) / 1000) + "s ago" : "no sync yet";
            var bits = (d.event_count || 0) + " intent" + (d.event_count === 1 ? "" : "s");
            if (d.away_active) bits += "  ·  away active";
            if (d.history_written) bits += "  ·  " + d.history_written + " history written";
            if (d.plan_enabled) bits += "  ·  " + (d.plan_events || 0) + " plan window" + (d.plan_events === 1 ? "" : "s");
            ind.className = "ha-status-indicator ha-ok";
            ind.textContent = "● reachable  ·  " + bits + "  ·  last sync " + age;
          } else {
            ind.className = "ha-status-indicator ha-warn";
            ind.textContent = "⚠  CalDAV server unreachable — check it is running";
          }
        }).catch(function () {
          if (ind) { ind.className = "ha-status-indicator ha-warn"; ind.textContent = "? status endpoint unreachable"; }
        });
      }

      function refreshCreds() {
        ownerFetch("/api/caldav/credentials").then(function (r) { return r.json(); }).then(function (d) {
          if (credsEl) {
            credsEl.textContent = "";
            if (d.managed && d.username && d.password) {
              var box = el("fieldset", { class: "caldav-account" }, [
                el("legend", { text: "Calendar account (managed by 42W)" }),
                el("div", { class: "field-row" }, [el("label", { text: "Username" }), el("code", { text: d.username }), copyBtn(function () { return d.username; })]),
                el("div", { class: "field-row" }, [el("label", { text: "Password" }), el("code", { text: d.password }), copyBtn(function () { return d.password; })]),
              ]);
              credsEl.appendChild(box);
            }
          }
          if (urlsEl) {
            urlsEl.textContent = "";
            if (d.subscribe_url) urlsEl.appendChild(urlRow("Subscribe (read + write)", d.subscribe_url));
            if (d.history_url) urlsEl.appendChild(urlRow("EVSE history (read-only)", d.history_url));
            if (d.plan_url) urlsEl.appendChild(urlRow("Planned actions (read-only)", d.plan_url));
          }
          if (qrEl && d.subscribe_url) {
            qrEl.textContent = "";
            import("/vendor/qrcode.js").then(function (m) {
              qrEl.appendChild(el("div", { class: "hint", text: "Scan to get the calendar URL on your phone:" }));
              qrEl.appendChild(drawQR(m.qrMatrix, lanURL(d.subscribe_url), 220));
            }).catch(function () { /* QR is optional */ });
          }
        }).catch(function () { /* credentials endpoint optional */ });
      }

      refreshStatus();
      refreshCreds();
      if (window._caldavStatusTimer) clearInterval(window._caldavStatusTimer);
      window._caldavStatusTimer = setInterval(refreshStatus, 5000);
    },
  };
})();
