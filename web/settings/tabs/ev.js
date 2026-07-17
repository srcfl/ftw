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
      // If ev_charger is empty but an easee driver exists with config,
      // populate the EV tab from the driver's config block so the UI
      // reflects what's actually running.
      if (!config.ev_charger.email && config.drivers) {
        for (var di = 0; di < config.drivers.length; di++) {
          var drv = config.drivers[di];
          if (drv.name === "easee" && drv.config) {
            config.ev_charger.provider = "easee";
            config.ev_charger.email = drv.config.email || "";
            config.ev_charger.password = drv.config.password || "";
            config.ev_charger.serial = drv.config.serial || "";
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
        selectField("Provider", "ev_charger.provider", ["easee"], "easee",
          "Cloud service provider for the EV charger. Currently only Easee is supported.") +
        field("Email", "ev_charger.email", "text", "",
          "Account email for the charger cloud service.") +
        '<label>Password ' + help("Account password for the charger cloud service.") + '</label>' +
        '<input type="password" data-path="ev_charger.password" value="" placeholder="' + (evHasPassword ? '••••••••' : '') + '">' +
        '<div style="margin-top:4px">' + credsBadge + '</div>' +
        field("Charger serial", "ev_charger.serial", "text", "",
          "Serial number of the charger. Leave empty to auto-detect the first charger on the account.") +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'Credentials are used to authenticate with the Easee Cloud API. ' +
        'The charger serial is optional — if left empty the driver will use the first charger found on your account.' +
        '</p>';
    },
    after: function (ctx) {
      var bodyEl = ctx.bodyEl;
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
          var easee = null;
          for (var i = 0; i < drivers.length; i++) {
            if ((drivers[i].name || "").toLowerCase().indexOf("easee") >= 0) {
              easee = drivers[i];
              break;
            }
          }
          if (!easee) {
            el.className = "ha-status-indicator ha-off";
            el.textContent = "○  no Easee driver configured";
            return;
          }
          if (easee.status === "ok" || easee.status === "online") {
            el.className = "ha-status-indicator ha-ok";
            el.textContent = "● charger connected  ·  " + (easee.device_id || easee.name);
          } else {
            el.className = "ha-status-indicator ha-warn";
            el.textContent = "⚠  charger " + (easee.status || "unknown") + "  —  check credentials";
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
