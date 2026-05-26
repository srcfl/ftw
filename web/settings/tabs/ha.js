// Settings → Home Assistant tab: MQTT bridge config + live status.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

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
        fetch("/api/ha/status").then(function (r) { return r.json(); }).then(function (d) {
          if (!d.enabled) {
            el.className = "ha-status-indicator ha-off";
            el.textContent = "○  disabled in config";
            return;
          }
          if (d.connected) {
            var age = d.last_publish_ms > 0 ? Math.round((Date.now() - d.last_publish_ms) / 1000) + "s ago" : "no publish yet";
            el.className = "ha-status-indicator ha-ok";
            el.textContent = "● connected to " + d.broker + "  ·  " + (d.sensors_announced || 0) + " sensors  ·  last publish " + age;
          } else {
            el.className = "ha-status-indicator ha-warn";
            el.textContent = "⚠  not connected to " + (d.broker || "?") + "  —  check broker + credentials";
          }
        }).catch(function () {
          el.className = "ha-status-indicator ha-warn";
          el.textContent = "? status endpoint unreachable";
        });
      }
      refresh();
      if (window._haStatusTimer) clearInterval(window._haStatusTimer);
      window._haStatusTimer = setInterval(refresh, 5000);
    },
  };
})();
