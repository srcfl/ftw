// Settings → Notifications tab: operator-facing notifications settings.
// Transport runtime lives in internal/notifications; this file renders
// the editor and embeds the <ftw-notif-*> Web Components for live
// status + the test-button.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function ownerFetch(path, opts) {
    return fetch(path, opts);
  }

  function defaultEvents() {
    return [
      { type: "driver_offline", enabled: false, threshold_s: 600, priority: 4, cooldown_s: 3600, tags: "", title_template: "", body_template: "" },
      { type: "driver_recovered", enabled: false, priority: 3, cooldown_s: 0, tags: "", title_template: "", body_template: "" },
    ];
  }

  S.tabs.notifications = {
    render: function (ctx) {
      var field = ctx.field, help = ctx.help, config = ctx.config;

      // Fetch backend-defined template defaults once so the operator
      // sees exactly what will be sent and can tweak in place. Failure
      // is non-fatal — fields just render empty (blank falls back to
      // default at render time on the server anyway).
      if (!window._notifDefaults) {
        ownerFetch("/api/notifications/defaults")
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (d) {
            if (!d) return;
            window._notifDefaults = d;
            var active = document.querySelector("#settings-tabs .active");
            if (active && active.dataset.tab === "notifications") {
              ctx.renderTab("notifications");
            }
          }).catch(function () {});
      }
      var defaults = window._notifDefaults || {};

      if (!config.notifications) {
        config.notifications = {
          enabled: false,
          provider: "ntfy",
          default_priority: 3,
          ntfy: { server: "https://ntfy.sh", topic: "" },
          events: defaultEvents(),
        };
      }
      var nc = config.notifications;
      if (!nc.provider) nc.provider = "ntfy";
      if (!nc.ntfy) nc.ntfy = { server: "https://ntfy.sh", topic: "" };
      if (!nc.events || !nc.events.length) nc.events = defaultEvents();

      // Defaults from the backend are shown as placeholders, NOT written
      // into ev.title_template / ev.body_template — otherwise a Save
      // without edits would persist the current default into YAML and
      // freeze this site on today's text. Keeping the field blank lets
      // the backend render the live default on every fire; if defaults
      // change in a future release, unchanged rules pick them up.

      var html = '<ftw-notif-status interval-ms="5000" style="margin-bottom:10px"></ftw-notif-status>' +
        '<fieldset><legend>Transport</legend>' +
        '<label><input type="checkbox" data-checkbox-path="notifications.enabled"' + (nc.enabled ? ' checked' : '') + '> Enabled</label>' +
        '<div class="field-row"><div>' +
        '<label>Provider' +
        '<select data-path="notifications.provider">' +
        '<option value="ntfy"' + (nc.provider === "ntfy" ? ' selected' : '') + '>ntfy</option>' +
        '</select></label>' +
        '</div><div>' +
        field("Default priority (1–5)", "notifications.default_priority", "number", 3,
          "Used when a rule leaves priority=0. 5 = urgent, 1 = low.") +
        '</div></div>' +
        '</fieldset>' +
        '<fieldset><legend>ntfy</legend>' +
        '<div class="field-row"><div>' +
        field("Server", "notifications.ntfy.server", "text", "https://ntfy.sh",
          "Public ntfy.sh or your self-hosted server URL.") +
        '</div><div>' +
        field("Topic", "notifications.ntfy.topic", "text", "",
          "The ntfy topic (subscribe to it in the ntfy app).") +
        '</div></div>' +
        // Access-token uses the masked-placeholder pattern: typing replaces
        // the saved value, leaving blank preserves it via
        // PreserveMaskedSecrets on the backend.
        '<label>Access token ' + help("Bearer token issued by your ntfy server (or ntfy.sh reserved topic).") + '</label>' +
        '<input type="password" data-path="notifications.ntfy.access_token" value="" ' +
        'placeholder="' + (nc.ntfy && nc.ntfy.has_access_token
          ? "configured — hidden, type to replace"
          : "paste token") + '">' +
        '</fieldset>';

      var escAttr = function (s) {
        return (s == null ? "" : String(s))
          .replace(/&/g, "&amp;").replace(/"/g, "&quot;")
          .replace(/</g, "&lt;").replace(/>/g, "&gt;");
      };

      for (var ei = 0; ei < nc.events.length; ei++) {
        var rule = nc.events[ei];
        var def = defaults[rule.type] || {};
        // Types that use threshold_s (sustained-over window):
        // driver_offline and fuse_over_limit. Others (driver_recovered,
        // update_available) would render a dead field — skip with a
        // clarifying note.
        var usesThreshold = rule.type === "driver_offline" || rule.type === "fuse_over_limit";
        var noThresholdNote = rule.type === "driver_recovered"
          ? "Fires within 30 s of telemetry resuming — no threshold configurable."
          : rule.type === "update_available"
            ? "Fires when a new release is discovered — no threshold configurable."
            : "This event type has no threshold.";
        html += '<fieldset><legend>' + (rule.type || "event #" + ei) + '</legend>' +
          '<label><input type="checkbox" data-checkbox-path="notifications.events.' + ei + '.enabled"' + (rule.enabled ? ' checked' : '') + '> Enabled</label>';
        if (usesThreshold) {
          html += '<div class="field-row"><div>' +
            field("Threshold (s)", "notifications.events." + ei + ".threshold_s", "number", 600,
              "How long the condition must hold before firing. Default 600 s (10 min). Independent of the control-loop watchdog.") +
            '</div><div>' +
            field("Priority (0–5)", "notifications.events." + ei + ".priority", "number", 3,
              "0 uses the default priority above.") +
            '</div></div>';
        } else {
          html += '<p style="color:var(--text-dim);font-size:0.75rem;margin:6px 0">' +
            noThresholdNote +
            '</p>' +
            field("Priority (0–5)", "notifications.events." + ei + ".priority", "number", 3,
              "0 uses the default priority above.");
        }
        html += '<div class="field-row"><div>' +
          field("Cooldown (s)", "notifications.events." + ei + ".cooldown_s", "number", 3600,
            "Minimum seconds between fires per driver.") +
          '</div><div>' +
          field("Tags (comma-separated)", "notifications.events." + ei + ".tags", "text", "") +
          '</div></div>' +
          // Templates: input value = operator's custom text (if any),
          // placeholder = backend default for visibility. Leaving blank
          // preserves "use server defaults" semantics through save.
          '<label>Title template ' + help("Go text/template. Leave blank to use the server default.") + '</label>' +
          '<input type="text" data-path="notifications.events.' + ei + '.title_template" ' +
          'value="' + escAttr(rule.title_template || "") + '" ' +
          'placeholder="' + escAttr(def.title || "") + '">' +
          '<label>Body template ' + help("Go text/template. Leave blank to use the server default.") + '</label>' +
          '<input type="text" data-path="notifications.events.' + ei + '.body_template" ' +
          'value="' + escAttr(rule.body_template || "") + '" ' +
          'placeholder="' + escAttr(def.body || "") + '">' +
          '</fieldset>';
      }

      html += '<fieldset><legend>Test</legend>' +
        '<ftw-notif-test-button label="Send test notification"></ftw-notif-test-button>' +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'Template fields — driver events: {{.Device}}, {{.Make}}, {{.Serial}}, {{.Duration}}, {{.DurationS}}. ' +
        'update_available: {{.Version}}, {{.PreviousVersion}}, {{.ReleaseURL}}. ' +
        'fuse_over_limit: {{.Phase}}, {{.Amps}}, {{.LimitA}}, {{.Duration}}. ' +
        'Always: {{.EventType}}, {{.Timestamp}}.' +
        '</p>';
      return html;
    },
  };
})();
