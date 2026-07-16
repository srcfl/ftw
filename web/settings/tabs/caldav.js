// Settings → Calendar tab: CalDAV planner-constraints client (#498).
// FTW can manage the calendar credential itself and show it here (with a QR)
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

  // Calendar apps on the LAN can't reach "localhost" (that's FTW's own link to
  // its in-process CalDAV server); rewrite it to the host serving this dashboard.
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

  // webcalFeed turns a collection's http(s) URL into a one-tap webcal:// link to
  // its read-only .ics feed, with the managed credential embedded so the phone
  // subscribes without a manual login. Built by string (the URL API refuses to
  // switch a special http scheme to the non-special webcal scheme).
  function webcalFeed(httpUrl, name, user, pass) {
    try {
      var u = new URL(lanURL(httpUrl));
      var auth = user ? encodeURIComponent(user) + ":" + encodeURIComponent(pass || "") + "@" : "";
      return "webcal://" + auth + u.host + "/feed/" + name + ".ics";
    } catch (e) {
      return null;
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

  // calendarCard renders one calendar as a self-contained block: its name, a
  // one-line description of what it does, the link/URL, a Copy button, and a QR.
  // c.clickable → render the link as a tappable <a> (the read-only webcal://
  // feeds); otherwise a plain <code> URL to add as an account (read + write).
  // The QR is filled in later (after the qrcode.js dynamic import) into c.qrSlot.
  function calendarCard(c) {
    var value = c.clickable
      ? el("a", { class: "caldav-url", href: c.link, target: "_blank", rel: "noopener noreferrer", text: c.displayText || c.link })
      : el("code", { class: "caldav-val", text: c.link });
    c.qrSlot = el("div", { class: "caldav-qr" });
    return el("div", { class: "caldav-cal" }, [
      el("div", { class: "caldav-cal-title", text: c.label }),
      el("p", { class: "caldav-cal-desc", text: c.desc }),
      el("div", { class: "caldav-row" }, [value, copyBtn(function () { return c.link; })]),
      c.qrSlot,
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
    // No fixed inline width/height — .caldav-qr canvas { max-width:100%; height:auto }
    // lets the code scale down inside a narrow phone modal without distortion.
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
        '<p class="hint">FTW hosts its own built-in CalDAV server and turns the events you add into planner constraints. It stays on your local network — nothing here is exposed to the internet.</p>' +
        '<label><input type="checkbox" data-checkbox-path="caldav.enabled"' + (config.caldav.enabled ? " checked" : "") + "> Enabled</label>" +
        '<label><input type="checkbox" data-checkbox-path="caldav.manage_credentials"' + (managed ? " checked" : "") + "> Let FTW manage the calendar password (recommended)</label>" +
        '<div class="field-row"><div>' +
        field("Server URL", "caldav.url", "text", "http://localhost:5232", "Base URL of the CalDAV server. Defaults to FTW's built-in in-process server.") +
        "</div><div>" +
        field("Calendar path", "caldav.calendar_path", "text", "/ftw/energy/", "Collection FTW reads events from.") +
        "</div></div>" +
        field("Username", "caldav.username", "text", "ftw");
      if (!managed) {
        html += field("Password", "caldav.password", "password", "", "Only used when FTW is not managing the password.");
      }
      html +=
        '<div class="field-row"><div>' +
        field("History path", "caldav.history_path", "text", "/ftw/history/", "Read-only EV charging history collection.") +
        "</div><div>" +
        field("Plan path", "caldav.plan_path", "text", "/ftw/plan/", "Read-only planned energy windows collection.") +
        "</div></div>" +
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
                el("legend", { text: "Calendar account (managed by FTW)" }),
                el("div", { class: "caldav-row" }, [el("label", { text: "Username" }), el("code", { class: "caldav-val", text: d.username }), copyBtn(function () { return d.username; })]),
                el("div", { class: "caldav-row" }, [el("label", { text: "Password" }), el("code", { class: "caldav-val", text: d.password }), copyBtn(function () { return d.password; })]),
              ]);
              credsEl.appendChild(box);
            }
          }
          // One card per calendar: name + what it does + link + QR.
          var calendars = [];
          if (d.subscribe_url) calendars.push({
            label: "Subscribe (read + write)",
            desc: "Add this as a calendar account, then create “Away” or “Charge car 80%” events for FTW to act on. Scan the QR to get the URL onto your phone.",
            link: lanURL(d.subscribe_url),
            clickable: false,
          });
          if (d.plan_url) calendars.push({
            label: "Planned actions (read-only)",
            desc: "Subscribe to see FTW’s upcoming battery charge / discharge windows. Tap the link or scan the QR to subscribe in one step.",
            link: webcalFeed(d.plan_url, "plan", d.username, d.password),
            displayText: webcalFeed(d.plan_url, "plan"),
            clickable: true,
          });
          if (d.history_url) calendars.push({
            label: "EVSE history (read-only)",
            desc: "Subscribe to get one event per completed EV charge session. Tap the link or scan the QR to subscribe in one step.",
            link: webcalFeed(d.history_url, "history", d.username, d.password),
            displayText: webcalFeed(d.history_url, "history"),
            clickable: true,
          });

          if (qrEl) qrEl.textContent = "";
          if (urlsEl) {
            urlsEl.textContent = "";
            calendars.forEach(function (c) {
              if (c.link) urlsEl.appendChild(calendarCard(c));
            });
            // One dynamic import paints every card's QR (read + write URL, and
            // the read-only webcal:// feeds).
            import("/vendor/qrcode.js").then(function (m) {
              calendars.forEach(function (c) {
                if (!c.link || !c.qrSlot) return;
                c.qrSlot.appendChild(el("div", { class: "hint", text: "Scan with your phone:" }));
                c.qrSlot.appendChild(drawQR(m.qrMatrix, c.link, 160));
              });
            }).catch(function () { /* QR optional */ });
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
