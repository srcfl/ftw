// Settings → EV tab: cloud EV charger credentials + live status.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  S.tabs.ev = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField, help = ctx.help;
      var getByPath = ctx.getByPath, config = ctx.config;
      if (!config.ev_charger) config.ev_charger = {};
      if (!config.ev_charger.http) config.ev_charger.http = {};
      // If ev_charger is empty but an Easee/Zaptec driver exists with
      // config, populate the EV tab from the driver's config block so
      // the UI reflects what's actually running.
      if (!config.ev_charger.provider && config.drivers) {
        for (var di = 0; di < config.drivers.length; di++) {
          var drv = config.drivers[di];
          if ((drv.name === "easee" || drv.name === "zaptec") && drv.config) {
            config.ev_charger.provider = drv.name;
            config.ev_charger.email = drv.config.email || drv.config.username || "";
            config.ev_charger.password = drv.config.password || "";
            config.ev_charger.serial = drv.config.serial || "";
            break;
          }
          if ((drv.name === "tesla-wc" || (drv.lua || "").indexOf("tesla_wall_connector") >= 0) && drv.config) {
            config.ev_charger.provider = "tesla-wc";
            config.ev_charger.http.base_url = drv.config.base_url ||
              (drv.config.host ? ("http://" + drv.config.host) : "");
            break;
          }
        }
      }
      var evHasPassword = !!getByPath(config, "ev_charger.password", "");
      var credsBadge = evHasPassword
        ? '<span id="ev-creds-badge" class="creds-badge creds-saved">✓ Credentials saved</span>'
        : '<span id="ev-creds-badge" class="creds-badge creds-missing">⚠ No credentials saved</span>';
      return '<div id="ev-status-indicator" class="ha-status-indicator">checking…</div>' +
        '<fieldset><legend>EV Charger</legend>' +
        selectField("Provider", "ev_charger.provider", ["easee", "zaptec", "tesla-wc"], "easee",
          "Easee and Zaptec use a cloud account. Tesla Wall Connector is local HTTP on the LAN.") +
        (getByPath(config, "ev_charger.provider", "easee") === "tesla-wc"
          ? field("Host", "ev_charger.http.base_url", "text", "http://192.168.1.50",
              "LAN address of the Wall Connector. No Tesla account. Current is set by the car or the Tesla app.")
          : field("Email", "ev_charger.email", "text", "",
              "Account email for the charger cloud service.") +
            '<label>Password ' + help("Account password for the charger cloud service.") + '</label>' +
            '<input type="password" data-path="ev_charger.password" value="" placeholder="' + (evHasPassword ? '••••••••' : '') + '">' +
            '<div style="margin-top:4px">' + credsBadge + '</div>' +
            field("Charger serial", "ev_charger.serial", "text", "",
              "Serial number of the charger. Leave empty to auto-detect the first charger on the account.")) +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        (getByPath(config, "ev_charger.provider", "easee") === "tesla-wc"
          ? 'Tesla Wall Connector Gen 3 is polled on the LAN. It reports plug state and power; it cannot take a current setpoint. Pair tesla_vehicle to steer a Tesla.'
          : 'Credentials authenticate with the charger cloud API (Easee or Zaptec). ' +
            'The charger serial is optional — if left empty the driver will use the first charger found on your account.') +
        '</p>';
    },
    after: function (ctx) {
      var bodyEl = ctx.bodyEl;
      var providerSel = bodyEl.querySelector('[data-path="ev_charger.provider"]');
      if (providerSel) {
        providerSel.addEventListener("change", function () {
          ctx.captureCurrentTab();
          if (providerSel.value === "tesla-wc") {
            var ev = ctx.config.ev_charger;
            if (ev) {
              ev.username = "";
              ev.email = "";
              ev.password = "";
            }
          }
          ctx.renderTab("ev");
        });
      }
      var evHasPassword = !!ctx.getByPath(ctx.config, "ev_charger.password", "");
      var pwInput = bodyEl.querySelector('[data-path="ev_charger.password"]');
      if (pwInput) {
        pwInput.addEventListener("focus", function () { pwInput.placeholder = ""; });
        pwInput.addEventListener("blur", function () {
          if (!pwInput.value && evHasPassword) pwInput.placeholder = "••••••••";
        });
      }
      var el = document.getElementById("ev-status-indicator");
      if (!el) return;
      function refresh() {
        apiFetch("/api/status").then(function (r) { return r.json(); }).then(function (d) {
          var badge = document.getElementById("ev-creds-badge");
          if (badge) {
            if (d.ev_credentials_saved) {
              badge.textContent = "✓ Credentials saved";
              badge.className = "creds-badge creds-saved";
            } else {
              badge.textContent = "⚠ No credentials saved";
              badge.className = "creds-badge creds-missing";
            }
          }
          var rawDrivers = d.drivers || {};
          var drivers = [];
          if (Array.isArray(rawDrivers)) {
            drivers = rawDrivers;
          } else {
            Object.keys(rawDrivers).forEach(function (k) {
              var entry = rawDrivers[k];
              if (typeof entry === "object" && entry !== null) {
                if (!entry.name) entry.name = k;
                drivers.push(entry);
              }
            });
          }
          var charger = null;
          for (var i = 0; i < drivers.length; i++) {
            var n = (drivers[i].name || "").toLowerCase();
            if (n.indexOf("easee") >= 0 || n.indexOf("zaptec") >= 0 ||
                n.indexOf("tesla-wc") >= 0 || n.indexOf("tesla_wall") >= 0 ||
                n.indexOf("tesla-wall") >= 0) {
              charger = drivers[i];
              break;
            }
          }
          if (!charger) {
            el.className = "ha-status-indicator ha-off";
            el.textContent = "○  no cloud charger driver configured";
            return;
          }
          if (charger.status === "ok" || charger.status === "online") {
            el.className = "ha-status-indicator ha-ok";
            el.textContent = "● charger connected  ·  " + (charger.device_id || charger.name);
          } else {
            el.className = "ha-status-indicator ha-warn";
            el.textContent = "⚠  charger " + (charger.status || "unknown") + "  —  check credentials";
          }
        }).catch(function () {
          el.className = "ha-status-indicator ha-warn";
          el.textContent = "? status endpoint unreachable";
        });
      }
      refresh();
      if (window._evStatusTimer) clearInterval(window._evStatusTimer);
      window._evStatusTimer = setInterval(refresh, 5000);
    },
  };
})();
