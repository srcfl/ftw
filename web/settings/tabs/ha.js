// Settings → Home Assistant tab: MQTT bridge config + live status.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  function statusView(status, desiredEnabled, nowMs) {
    if (typeof desiredEnabled === "boolean" && desiredEnabled !== !!status.enabled) {
      return {
        className: "ha-status-indicator ha-warn",
        text: desiredEnabled ? "○  unsaved change — Save to enable" : "○  unsaved change — Save to disable",
      };
    }
    if (!status.enabled) {
      return {
        className: "ha-status-indicator ha-off",
        text: "○  disabled in config",
      };
    }
    if (status.connected) {
      var age = status.last_publish_ms > 0
        ? Math.round(((nowMs == null ? Date.now() : nowMs) - status.last_publish_ms) / 1000) + "s ago"
        : "no publish yet";
      return {
        className: "ha-status-indicator ha-ok",
        text: "● connected to " + status.broker + "  ·  " + (status.sensors_announced || 0) + " sensors  ·  last publish " + age,
      };
    }
    return {
      className: "ha-status-indicator ha-warn",
      text: "⚠  enabled but not connected to " + (status.broker || "?") + "  —  check broker + credentials",
    };
  }

  S.tabs.ha = {
    render: function (ctx) {
      var field = ctx.field, config = ctx.config;
      if (!config.homeassistant) config.homeassistant = {};
      return '<div id="ha-status-indicator" class="ha-status-indicator">checking…</div>' +
        '<fieldset><legend>Home Assistant MQTT</legend>' +
        '<label><input type="checkbox" data-checkbox-path="homeassistant.enabled"' + (config.homeassistant.enabled ? ' checked' : '') + '> Enabled</label>' +
        '<div class="field-row"><div>' +
        field("Broker host", "homeassistant.broker", "text", "192.168.1.1",
          "IP or hostname of the MQTT broker Home Assistant uses. Typically the HA server itself (Mosquitto addon).") +
        '</div><div>' +
        field("Port", "homeassistant.port", "number", 1883) +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Username", "homeassistant.username", "text", "") +
        '</div><div>' +
        field("Password", "homeassistant.password", "password", "") +
        '</div></div>' +
        field("Publish interval (s)", "homeassistant.publish_interval_s", "number", 5,
          "How often state topics are pushed to HA. 5 s is a good default.") +
        '</fieldset>';
    },
    after: function () {
      var el = document.getElementById("ha-status-indicator");
      if (!el) return;
      function refresh() {
        apiFetch("/api/ha/status").then(function (r) { return r.json(); }).then(function (d) {
          var checkbox = document.querySelector('[data-checkbox-path="homeassistant.enabled"]');
          var view = statusView(d, checkbox ? checkbox.checked : null);
          el.className = view.className;
          el.textContent = view.text;
        }).catch(function () {
          el.className = "ha-status-indicator ha-warn";
          el.textContent = "? status endpoint unreachable";
        });
      }
      refresh();
      if (window._haStatusTimer) clearInterval(window._haStatusTimer);
      window._haStatusTimer = setInterval(refresh, 5000);
    },
    _pure: {
      statusView: statusView,
    },
  };
})();
