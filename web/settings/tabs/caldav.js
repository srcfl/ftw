// Settings → Calendar tab: CalDAV planner-constraints client config + the
// subscribe URLs to paste into a calendar app (issue #498).
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function ownerFetch(path, opts) {
    if (typeof window.ownerFetch === "function") return window.ownerFetch(path, opts);
    return fetch(path, opts);
  }

  // Calendar apps on the LAN can't reach "localhost" / "127.0.0.1" (that's the
  // 42W↔Radicale link). Rewrite the host to whatever the dashboard is served
  // from so the pasted URL works from a phone on the same network.
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

  S.tabs.caldav = {
    render: function (ctx) {
      var field = ctx.field, config = ctx.config;
      if (!config.caldav) config.caldav = {};
      return '<div id="caldav-status-indicator" class="ha-status-indicator">checking…</div>' +
        '<fieldset><legend>Calendar (CalDAV)</legend>' +
        '<p class="hint">42W reads a calendar served by the bundled Radicale sidecar and turns events into planner constraints. It stays on your local network — nothing here is exposed to the internet.</p>' +
        '<label><input type="checkbox" data-checkbox-path="caldav.enabled"' + (config.caldav.enabled ? ' checked' : '') + '> Enabled</label>' +
        '<div class="field-row"><div>' +
        field("Server URL", "caldav.url", "text", "http://localhost:5232",
          "Base URL of the CalDAV server. Defaults to the bundled Radicale sidecar.") +
        '</div><div>' +
        field("Calendar path", "caldav.calendar_path", "text", "/fortytwowatts/energy/",
          "Collection 42W reads events from. Subscribe your calendar app to this.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Username", "caldav.username", "text", "fortytwowatts") +
        '</div><div>' +
        field("Password", "caldav.password", "password", "",
          "Stored on the Pi, never written to config.yaml. Must match the Radicale account.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Poll interval (s)", "caldav.poll_interval_s", "number", 300,
          "How often 42W re-reads the calendar. 300 s is a good default.") +
        '</div><div>' +
        field("Default EV target SoC (%)", "caldav.ev_default_target_soc_pct", "number", 80,
          "Used when an EV event's title has no explicit percentage.") +
        '</div></div>' +
        '<label><input type="checkbox" data-checkbox-path="caldav.evse_history"' + (config.caldav.evse_history === false ? '' : ' checked') + '> Write EVSE usage as calendar history</label>' +
        '<label><input type="checkbox" data-checkbox-path="caldav.publish_plan"' + (config.caldav.publish_plan === false ? '' : ' checked') + '> Publish the planner\'s upcoming charge/discharge windows</label>' +
        '<p class="hint" style="margin-top:6px">Title an event <b>Away</b> or <b>Vacation</b> to conserve battery while you\'re out. Title one <b>Charge car 80%</b> (with your departure as the event time) so the car is ready in time.</p>' +
        '<div id="caldav-urls"></div>' +
        '</fieldset>';
    },
    after: function () {
      var el = document.getElementById("caldav-status-indicator");
      var urls = document.getElementById("caldav-urls");
      if (!el) return;
      function refresh() {
        ownerFetch("/api/caldav/status").then(function (r) { return r.json(); }).then(function (d) {
          if (!d.enabled) {
            el.className = "ha-status-indicator ha-off";
            el.textContent = "○  disabled in config";
            if (urls) urls.innerHTML = "";
            return;
          }
          if (d.reachable) {
            var age = d.last_sync_ms > 0 ? Math.round((Date.now() - d.last_sync_ms) / 1000) + "s ago" : "no sync yet";
            var bits = (d.event_count || 0) + " intent" + (d.event_count === 1 ? "" : "s");
            if (d.away_active) bits += "  ·  away active";
            if (d.history_written) bits += "  ·  " + d.history_written + " history event" + (d.history_written === 1 ? "" : "s") + " written";
            if (d.plan_enabled) bits += "  ·  " + (d.plan_events || 0) + " plan window" + (d.plan_events === 1 ? "" : "s");
            el.className = "ha-status-indicator ha-ok";
            el.textContent = "● reachable  ·  " + bits + "  ·  last sync " + age;
          } else {
            el.className = "ha-status-indicator ha-warn";
            el.textContent = "⚠  CalDAV server unreachable  —  check URL + credentials  ·  " + (d.last_error || "");
          }
          if (urls) {
            var rows = "";
            if (d.subscribe_url) {
              rows += '<div class="field-row"><label>Subscribe (read + write)</label>' +
                '<code class="subscribe-url">' + lanURL(d.subscribe_url) + '</code></div>';
            }
            if (d.history_enabled && d.history_url) {
              rows += '<div class="field-row"><label>EVSE history (read-only)</label>' +
                '<code class="subscribe-url">' + lanURL(d.history_url) + '</code></div>';
            }
            if (d.plan_enabled && d.plan_url) {
              rows += '<div class="field-row"><label>Planned actions (read-only)</label>' +
                '<code class="subscribe-url">' + lanURL(d.plan_url) + '</code></div>';
            }
            urls.innerHTML = rows;
          }
        }).catch(function () {
          el.className = "ha-status-indicator ha-warn";
          el.textContent = "? status endpoint unreachable";
        });
      }
      refresh();
      if (window._caldavStatusTimer) clearInterval(window._caldavStatusTimer);
      window._caldavStatusTimer = setInterval(refresh, 5000);
    },
  };
})();
