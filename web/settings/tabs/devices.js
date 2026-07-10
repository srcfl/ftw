// Settings → Devices tab: driver catalog picker + per-driver editor.
// Owns its own add/remove/connect button wiring; the Settings shell
// stays driver-agnostic.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // ownerFetch routes state-changing owner/CONTROL probes (EV-charger probe with
  // email+password, Tesla verify with IP+VIN, driver test with the full driver
  // config) over the STRICT P2P transport so those SENSITIVE bodies never traverse
  // the untrusted relay on the public home route. Wired in p2p.js to the shared
  // fail-closed strict function; falls back to plain fetch only where p2p.js never
  // loaded (genuine LAN / tests).
  function ownerFetch(path, opts) {
    if (typeof window.ownerFetch === "function") return window.ownerFetch(path, opts);
    return fetch(path, opts);
  }

  function catalogEntryForLua(lua) {
    return lua ? (S.catalogByLua || {})[lua] : null;
  }

  function catalogHasCapability(lua, capability) {
    var entry = catalogEntryForLua(lua);
    var caps = (entry && entry.capabilities) || [];
    return caps.indexOf(capability) >= 0;
  }

  S.tabs.devices = {
    render: function (ctx) {
      var help = ctx.help, escHtml = ctx.escHtml, config = ctx.config;
      if (!config.drivers) config.drivers = [];
      var html = '<fieldset><legend>Add from catalog</legend>' +
        '<div class="field-row"><div>' +
        '<label>Driver <span class="help" data-help="Pick a Lua driver from the drivers/ directory. Each driver declares its capabilities (MQTT/Modbus) + which manufacturer/model it supports.">?</span></label>' +
        '<select id="driver-catalog-picker"><option value="">Loading catalog…</option></select>' +
        '</div><div>' +
        '<label>Friendly name</label><input type="text" id="driver-catalog-name" placeholder="e.g. ferroamp-house">' +
        '</div></div>' +
        '<button class="btn-add" id="driver-catalog-add">+ Add selected</button>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:8px 0 0">' +
        '🟢 production — verified on real hardware at ≥1 site · ' +
        '🟡 beta — working on a single site, awaiting a second · ' +
        '🔴 experimental — ported from reference, not yet proven against live hardware. ' +
        'Hover a driver for site + date notes.' +
        '</p>' +
        '</fieldset>';
      html += '<div class="devices-list">';
      config.drivers.forEach(function (d, idx) {
        var cap = d.capabilities || {};
        var mqtt = cap.mqtt || d.mqtt;
        var modbus = cap.modbus || d.modbus;
        var protocol = mqtt ? "mqtt" : (modbus ? "modbus" : (cap.http ? "http" : "?"));
        var driverFile = d.lua || "(none)";
        var supportsBattery = catalogHasCapability(d.lua, "battery");
        html += '<div class="device-item">' +
          '<div class="device-item-header">' +
          '<strong>' + escHtml(d.name) + '</strong>' +
          '<span class="device-meta">lua · ' + protocol + ' · ' + escHtml(driverFile) + '</span>' +
          '<button class="btn-remove" data-remove-idx="' + idx + '">Remove</button>' +
          '</div>' +
          '<div class="field-row device-core-row' + (supportsBattery ? '' : ' field-row-single') + '"><div>' +
          '<label>Driver file ' + help('Path to the .lua driver. Absolute or relative to the config file directory.') + '</label>' +
          '<input type="text" data-path="drivers.' + idx + '.lua" value="' + escHtml(driverFile) + '">' +
          '</div><div class="driver-battery-capacity" data-drv-lua="' + escHtml(d.lua || '') + '"' + (supportsBattery ? '' : ' hidden style="display:none"') + '>' +
          '<label>Battery capacity (kWh) ' + help('Nameplate storage capacity in kilowatt-hours. Stored internally as Wh.') + '</label>' +
          '<input type="number" step="0.1" data-path="drivers.' + idx + '.battery_capacity_wh" data-unit-scale="1000" value="' + ((d.battery_capacity_wh || 0) / 1000) + '">' +
          '</div></div>' +
          '<label><input type="checkbox" data-checkbox-path="drivers.' + idx + '.is_site_meter"' + (d.is_site_meter ? ' checked' : '') + '> Site meter ' + help('Exactly one driver should be the site meter — its grid reading defines the point-of-measurement the PI loop balances.') + '</label>';
        if (mqtt) {
          html += '<fieldset><legend>MQTT</legend>' +
            '<div class="field-row"><div>' +
            '<label>Host ' + help('IP or hostname of the MQTT broker exposing the device data (e.g. the Ferroamp EnergyHub).') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.capabilities.mqtt.host" value="' + escHtml(mqtt.host) + '">' +
            '</div><div>' +
            '<label>Port</label><input type="number" data-path="drivers.' + idx + '.capabilities.mqtt.port" value="' + (mqtt.port || 1883) + '">' +
            '</div></div>' +
            '<div class="field-row"><div>' +
            '<label>Username</label><input type="text" data-path="drivers.' + idx + '.capabilities.mqtt.username" value="' + escHtml(mqtt.username || "") + '">' +
            '</div><div>' +
            '<label>Password</label><input type="password" data-path="drivers.' + idx + '.capabilities.mqtt.password" value="' + escHtml(mqtt.password || "") + '">' +
            '</div></div></fieldset>';
        }
        if (modbus) {
          html += '<fieldset><legend>Modbus TCP</legend>' +
            '<div class="field-row"><div>' +
            '<label>Host ' + help('IP of the Modbus-TCP device (e.g. Sungrow inverter LAN port).') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.capabilities.modbus.host" value="' + escHtml(modbus.host) + '">' +
            '</div><div>' +
            '<label>Port</label><input type="number" data-path="drivers.' + idx + '.capabilities.modbus.port" value="' + (modbus.port || 502) + '">' +
            '</div></div>' +
            '<label>Unit ID ' + help('Slave address. Usually 1 for a single-device setup.') + '</label>' +
            '<input type="number" data-path="drivers.' + idx + '.capabilities.modbus.unit_id" value="' + (modbus.unit_id || 1) + '">' +
            '</fieldset>';
        }
        // Local-HTTP vs cloud-HTTP vs vehicle-over-proxy detection by
        // declared config shape + catalog capabilities. Vehicle drivers
        // (e.g. tesla_vehicle against a TeslaBLEProxy) expose only
        // {ip, vin} and read no power channel.
        var dcfg = d.config || {};
        var hasHostField = Object.prototype.hasOwnProperty.call(dcfg, 'host');
        var hasAuthField = Object.prototype.hasOwnProperty.call(dcfg, 'email') ||
                           Object.prototype.hasOwnProperty.call(dcfg, 'password');
        var hasApiCredsField = Object.prototype.hasOwnProperty.call(dcfg, 'client_id') ||
                               Object.prototype.hasOwnProperty.call(dcfg, 'client_secret');
        var catalogEntry = (S.catalogByLua || {})[d.lua];
        var caps = (catalogEntry && catalogEntry.capabilities) || [];
        var isVehicleDriver = cap.http != null &&
          (caps.indexOf("vehicle") >= 0 ||
           Object.prototype.hasOwnProperty.call(dcfg, 'vin') ||
           Object.prototype.hasOwnProperty.call(dcfg, 'ip'));
        var isLocalHTTP = !isVehicleDriver && cap.http != null && hasHostField;
        // OAuth2 client_credentials drivers (e.g. MyUplink): identify via client_id/client_secret keys.
        var isApiCredsDriver = !isVehicleDriver && cap.http != null && !hasHostField && hasApiCredsField;
        var isCloudDriver = !isVehicleDriver && !isApiCredsDriver && cap.http != null && !hasHostField &&
          (hasAuthField || Object.keys(dcfg).length === 0);
        if (isVehicleDriver) {
          // TeslaBLEProxy-style drivers only need the LAN IP of the
          // proxy and the VIN it's paired to. "Verify connection"
          // makes the backend issue a one-shot vehicle_data poll so
          // the operator can confirm pairing before saving.
          var vcfg = d.config || {};
          html += '<fieldset><legend>Vehicle</legend>' +
            '<div class="field-row"><div>' +
            '<label>Proxy IP ' + help('LAN address of the TeslaBLEProxy. Bare IP uses port 8080; append ":port" to override (e.g. 192.168.1.50:1234).') + '</label>' +
            '<input type="text" class="tesla-ip-input" data-driver-idx="' + idx + '" data-path="drivers.' + idx + '.config.ip" value="' + escHtml(vcfg.ip || '') + '" placeholder="192.168.1.50 (or 192.168.1.50:1234)">' +
            '</div><div>' +
            '<label>VIN ' + help('Vehicle Identification Number the proxy is paired to.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.vin" value="' + escHtml(vcfg.vin || '') + '" placeholder="5YJ3E1EA1KF000000">' +
            '</div></div>' +
            '<div style="margin-top:8px;display:flex;gap:10px;align-items:center">' +
            '<button class="btn-add tesla-verify-btn" type="button" data-driver-idx="' + idx + '">Verify connection</button>' +
            '<span class="tesla-verify-status" data-driver-idx="' + idx + '" style="font-size:0.82rem;color:var(--text-dim)"></span>' +
            '</div>' +
            '</fieldset>';
        }
        if (isLocalHTTP) {
          var lcfg = d.config || {};
          // NIBE-style local-API drivers (catalog apicreds + a connection port)
          // also need a username + an optional self-signed cert pin; plain
          // local-HTTP gateways (e.g. a P1 meter) keep just the Host field.
          // Backend auto-derives capabilities.http.allowed_hosts from config.host.
          var localCreds = caps.indexOf('apicreds') >= 0;
          var pin = (cap.http && cap.http.tls_pin_sha256) || '';
          // Render the Disable-PV checkbox for every HTTP driver; the
          // post-fetch pass in `after` hides it for drivers whose
          // catalog doesn't advertise BOTH meter + pv capabilities
          // (only those can double-count generation). Hiding via a
          // post-render DOM edit mirrors the site-meter pattern above
          // and avoids a re-render race with the async catalog fetch.
          html += '<fieldset><legend>HTTP</legend>' +
            '<label>Host / IP ' + help('Hostname (e.g. zap.local) or IP address of the device. mDNS names work when your OS resolver supports them; otherwise use the LAN IP.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.host" value="' + escHtml(lcfg.host || '') + '" placeholder="zap.local">' +
            '<div class="drv-local-creds" data-drv-lua="' + escHtml(d.lua || '') + '"' + (localCreds ? '' : ' hidden') + '>' +
              '<label style="margin-top:8px">Username ' + help('Username for the device\'s local API (HTTP Basic auth). For NIBE this is the local-API account you set up in the myUplink app.') + '</label>' +
              '<input type="text" autocomplete="off" data-path="drivers.' + idx + '.config.username" value="' + escHtml(lcfg.username || '') + '" placeholder="local-api-user">' +
            '</div>' +
            '<label class="drv-disable-pv" data-drv-lua="' + escHtml(d.lua || '') + '" style="margin-top:8px;display:none;align-items:center;gap:6px;font-weight:normal">' +
              '<input type="checkbox" data-checkbox-path="drivers.' + idx + '.config.disable_pv"' +
              (lcfg.disable_pv ? ' checked' : '') + '>' +
              'Disable PV readings ' +
              help('Use this gateway for the P1 meter only. When another driver already owns PV aggregation, set this so the two drivers don\'t double-count generation.') +
            '</label>' +
            '<div class="drv-local-creds" data-drv-lua="' + escHtml(d.lua || '') + '"' + (localCreds ? '' : ' hidden') + '>' +
              '<label style="margin-top:8px">Certificate fingerprint (SHA-256) ' + help('Pin the device\'s self-signed HTTPS certificate by its SHA-256 fingerprint (the "fingeravtryck" in the myUplink app, or from "openssl x509 -fingerprint -sha256"). 64 hex chars; colons and case are ignored. Leave empty for normal certificate verification.') + '</label>' +
              '<input type="text" autocomplete="off" data-path="drivers.' + idx + '.capabilities.http.tls_pin_sha256" value="' + escHtml(pin) + '" placeholder="73d1ac81…bd9bf4eb (64 hex)" style="font-family:var(--mono);font-size:0.78rem">' +
            '</div>' +
            '</fieldset>';
        }
        if (isApiCredsDriver) {
          // OAuth2 authorization-code drivers (e.g. MyUplink). Numbered
          // setup steps + the exact Callback URL, then Client Identifier +
          // Client Secret rendered together with the SAME labels as the
          // MyUplink portal (so the two values don't get swapped). The
          // secret is masked (never echoed into the DOM); Connect completes
          // the one-time browser consent and the refresh_token is stored
          // server-side, flipping the badge to Connected.
          var acfg = d.config || {};
          // refresh_token is a config_secret: when saved it round-trips as a
          // masked placeholder (non-empty string), so a non-empty value here
          // means "consent completed".
          var connected = typeof acfg.refresh_token === 'string' && acfg.refresh_token !== '';
          var connBadge = connected
            ? '<span class="creds-badge creds-saved">✓ Connected</span>'
            : '<span class="creds-badge creds-missing">⚠ Not connected</span>';
          var secretSaved = typeof acfg.client_secret === 'string' && acfg.client_secret !== '';
          var secretBadge = secretSaved
            ? '<span class="creds-badge creds-saved">✓ Saved</span>'
            : '<span class="creds-badge creds-missing">⚠ Not saved</span>';
          var callbackURL = location.origin + '/api/oauth/myuplink/callback';
          // Field labels mirror the MyUplink portal exactly ("Client
          // Identifier" / "Client Secret") and sit together — same as the
          // portal's Credentials box — so the two values don't get swapped.
          html += '<fieldset><legend>MyUplink connection</legend>' +
            '<ol style="color:var(--fg-muted);font-size:0.78rem;line-height:1.6;margin:0 0 12px;padding-left:1.2em">' +
            '<li>Open the <a href="https://dev.myuplink.com/apps" target="_blank" rel="noopener" style="color:var(--accent-e)">MyUplink developer portal</a> → <b>Apps</b> → <b>Create new app</b>.</li>' +
            '<li>Set <b>Callback Url</b> to the address shown below (copy it exactly).</li>' +
            '<li>Copy the app\'s <b>Client Identifier</b> and <b>Client Secret</b> into the matching fields below.</li>' +
            '<li><b>Save</b> these settings, then click <b>Connect to MyUplink</b> and sign in.</li>' +
            '</ol>' +
            '<label>Callback URL ' + help('Paste this exact string into the "Callback Url" field of your MyUplink app. It must match the address you use to reach 42-watts.') + '</label>' +
            '<input type="text" class="myuplink-callback-url" value="' + escHtml(callbackURL) + '" readonly onclick="this.select()" style="font-family:var(--mono);font-size:0.8rem">' +
            '<label style="margin-top:8px">Client Identifier ' + help('The "Client Identifier" from your MyUplink app (a UUID like xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx). NOT the secret.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.client_id" value="' + escHtml(acfg.client_id || '') + '" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx">' +
            '<label style="margin-top:8px">Client Secret ' + secretBadge + ' ' + help('The "Client Secret" from your MyUplink app. Stored masked; leave empty to keep the saved value.') + '</label>' +
            '<input type="password" autocomplete="off" data-path="drivers.' + idx + '.config.client_secret" value="" placeholder="' + (secretSaved ? '•••••••• (leave empty to keep)' : 'paste Client Secret') + '">' +
            '<div style="margin-top:12px;display:flex;gap:10px;align-items:center;flex-wrap:wrap">' +
            '<button class="btn-add myuplink-connect-btn" type="button" data-driver-idx="' + idx + '" data-driver-name="' + escHtml(d.name || '') + '">Connect to MyUplink</button>' +
            connBadge +
            '<span class="myuplink-connect-status" data-driver-idx="' + idx + '" style="font-size:0.82rem;color:var(--text-dim)"></span>' +
            '</div>' +
            // Manual fallback: when the automatic redirect can't reach this
            // device (remote/relay access, or the portal rejected an http LAN
            // callback), the operator copies the redirected URL from the
            // address bar and pastes it here. The Pi exchanges the code over
            // its own outbound HTTPS, so no inbound callback is needed.
            '<details style="margin-top:10px">' +
            '<summary style="cursor:pointer;font-size:0.8rem;color:var(--text-dim)">Not redirected back? Paste the URL instead</summary>' +
            '<p style="color:var(--text-dim);font-size:0.72rem;margin:6px 0">After signing in at MyUplink, copy the full address from your browser\'s address bar and paste it here. Use this for remote/relay access or if the page didn\'t return to 42-watts.</p>' +
            '<input type="text" class="myuplink-manual-url" data-driver-idx="' + idx + '" placeholder=".../api/oauth/myuplink/callback?code=...&amp;state=..." style="font-family:var(--mono);font-size:0.78rem">' +
            '<button class="btn-add myuplink-manual-btn" type="button" data-driver-idx="' + idx + '" style="margin-top:6px">Complete connection</button>' +
            '</details>' +
            '</fieldset>';
        }
        // Slot for catalog-declared config_secrets (e.g. sonnen Auth-Token).
        // Filled by the after() pass once /api/drivers/catalog has resolved.
        html += '<div class="drv-secrets-slot" data-driver-idx="' + idx + '"></div>';
        if (isCloudDriver) {
          var cfg = d.config || {};
          var hasPw = d.has_password === true;
          var pwBadge = hasPw
            ? '<span class="creds-badge creds-saved">✓ Saved</span>'
            : '<span class="creds-badge creds-missing">⚠ Not saved</span>';
          html += '<fieldset><legend>Cloud credentials</legend>' +
            '<div class="field-row"><div>' +
            '<label>Email ' + help('Account email for the cloud service.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.email" value="' + escHtml(cfg.email || '') + '">' +
            '</div><div>' +
            '<label>Password ' + pwBadge + '</label>' +
            '<input type="password" data-path="drivers.' + idx + '.config.password" value="" ' +
              'placeholder="' + (hasPw ? '•••••••• (leave empty to keep)' : 'enter password') + '">' +
            '</div></div>' +
            '<div class="field-row" style="align-items:flex-end"><div style="flex:1">' +
            '<label>Charger ' + help('Click Connect to load chargers from your account.') + '</label>' +
            '<select id="ev-charger-select-' + idx + '" data-path="drivers.' + idx + '.config.serial">' +
            (cfg.serial
              ? '<option value="' + escHtml(cfg.serial) + '" selected>' + escHtml(cfg.serial) + '</option>'
              : '<option value="">(not connected)</option>') +
            '</select>' +
            '</div><div>' +
            '<button class="btn-add ev-connect-btn" type="button" data-driver-idx="' + idx + '">Connect</button>' +
            '</div></div>' +
            '<span id="ev-connect-status-' + idx + '" style="font-size:0.8rem;color:var(--text-dim)"></span>' +
            '</fieldset>';
        }
        html += '<div class="driver-test-panel">' +
          '<button class="btn-add driver-test-btn" type="button" data-driver-idx="' + idx + '">Test connection</button>' +
          '<span class="driver-test-status" data-driver-idx="' + idx + '"></span>' +
          '<div class="driver-test-output" data-driver-idx="' + idx + '" hidden></div>' +
          '</div>';
        html += '</div>';
      });
      html += '</div>' +
        '<a href="/setup?step=3" class="btn-add" style="display:block;text-align:center;text-decoration:none">Add new device&hellip;</a>' +
        '<button class="btn-add" id="add-mqtt">+ Add MQTT device</button>' +
        '<button class="btn-add" id="add-modbus">+ Add Modbus device</button>';
      return html;
    },
    after: function (ctx) {
      var config = ctx.config;
      var bodyEl = ctx.bodyEl;
      var escHtml = ctx.escHtml;

      function fmtW(v) {
        if (!Number.isFinite(v)) return "—";
        return Math.abs(v) >= 1000 ? (v / 1000).toFixed(2) + " kW" : v.toFixed(0) + " W";
      }

      function fmtNum(v) {
        if (!Number.isFinite(v)) return "—";
        return Math.abs(v) >= 100 ? v.toFixed(0) : v.toFixed(2);
      }

      function fmtAge(ms) {
        if (!Number.isFinite(ms) || ms < 0) return "—";
        var s = Math.floor(ms / 1000);
        return s < 60 ? s + "s ago" : Math.floor(s / 60) + "m ago";
      }

      function renderProbeOutput(res) {
        var readings = res.readings || res.Readings || [];
        var metrics = res.metrics || res.Metrics || [];
        var health = res.health || res.Health || {};
        var identity = res.identity || res.Identity || {};
        var html = '<div class="driver-test-kv">';
        html += '<span>status</span><strong>' + escHtml(res.ok ? "connected" : "failed") + '</strong>';
        html += '<span>elapsed</span><strong>' + escHtml(String(res.elapsed_ms || res.ElapsedMs || 0)) + ' ms</strong>';
        if (health.TickCount != null) {
          html += '<span>ticks</span><strong>' + escHtml(String(health.TickCount)) + '</strong>';
        }
        if (identity.make || identity.sn || identity.endpoint) {
          html += '<span>identity</span><strong>' + escHtml([identity.make, identity.sn, identity.endpoint].filter(Boolean).join(" · ")) + '</strong>';
        }
        html += '</div>';
        if (res.error) {
          html += '<div class="driver-test-error">' + escHtml(res.error) + '</div>';
        }
        if (readings.length) {
          html += '<div class="driver-test-values">';
          readings.forEach(function (r) {
            var soc = r.soc != null ? " · SoC " + (r.soc * 100).toFixed(1) + "%" : "";
            var age = r.updated_at_ms ? " · " + fmtAge(Date.now() - r.updated_at_ms) : "";
            html += '<div><span>' + escHtml(r.type) + '</span><strong>' + escHtml(fmtW(r.smoothed_w)) + '</strong><small>raw ' + escHtml(fmtW(r.raw_w)) + soc + age + '</small></div>';
          });
          html += '</div>';
        }
        if (metrics.length) {
          html += '<div class="driver-test-metrics">';
          metrics.slice(0, 12).forEach(function (m) {
            html += '<span>' + escHtml(m.name) + '</span><strong>' + escHtml(fmtNum(m.value)) + '</strong>';
          });
          if (metrics.length > 12) {
            html += '<span>more</span><strong>' + escHtml(String(metrics.length - 12)) + '</strong>';
          }
          html += '</div>';
        }
        if (!readings.length && !metrics.length && !res.error) {
          html += '<div class="driver-test-empty">No values returned.</div>';
        }
        return html;
      }

      // Driver catalog picker — fetch async, render into select.
      ownerFetch("/api/drivers/catalog").then(function (r) { return r.json(); }).then(function (data) {
        var entries = (data && data.entries) || [];
        // Capability-driven reveal: show the Disable-PV checkbox only
        // on drivers whose catalog entry advertises BOTH meter and pv.
        // Other drivers can't double-count generation, so the toggle
        // would be meaningless. Looking up by `d.lua` ties the UI to
        // what the driver itself declares, not a hard-coded list.
        var byLua = {};
        entries.forEach(function (e) { if (e && e.path) byLua[e.path] = e; });
        // Cache by-lua so the synchronous render pass can detect
        // catalog-driven driver kinds (e.g. "vehicle") on re-renders
        // without waiting for the fetch to resolve again.
        S.catalogByLua = byLua;
        // Populate per-driver secret inputs (api_token, etc.) using the
        // catalog's config_secrets list. Each input uses the standard
        // data-path="drivers.<idx>.config.<key>" so the settings shell
        // saves it back into config.drivers[idx].config[key] like any
        // other form field. Empty existing values render as empty
        // password inputs; the `has_<key>` mirror for masked-saved
        // semantics is intentionally not modeled here — operators can
        // re-enter the token if they need to rotate it.
        bodyEl.querySelectorAll(".drv-secrets-slot").forEach(function (slot) {
          var dIdx = parseInt(slot.getAttribute("data-driver-idx"), 10);
          var d = config.drivers[dIdx];
          if (!d || !d.lua) return;
          var entry = byLua[d.lua];
          var secrets = (entry && entry.config_secrets) || [];
          var dcfg = d.config || {};
          // OAuth apicreds drivers (client_id/client_secret) render the
          // Client Secret in their own "MyUplink connection" fieldset, and
          // the refresh_token is managed by the Connect flow — never hand-
          // entered. Drop both from the generic Secrets section so the
          // secret isn't duplicated and a confusing "Refresh Token" input
          // doesn't appear.
          var isApiCreds = Object.prototype.hasOwnProperty.call(dcfg, 'client_id') ||
                           Object.prototype.hasOwnProperty.call(dcfg, 'client_secret');
          if (isApiCreds) {
            secrets = secrets.filter(function (k) {
              return k !== 'client_secret' && k !== 'refresh_token';
            });
          }
          if (secrets.length === 0) return;
          var fs = '<fieldset><legend>Secrets</legend>';
          secrets.forEach(function (key) {
            // Title-case, keep the raw key for the data-path attribute.
            // BOTH go through ctx.escHtml — config_secrets ultimately
            // comes from driver-authored Lua and a hostile/malformed
            // key containing < or > would otherwise be parsed as
            // markup when we innerHTML this fieldset. Same for the
            // value-readback branch (paranoia: the masked placeholder
            // is server-controlled, but a downstream change might
            // make this user-controlled).
            var label = key.replace(/_/g, " ").replace(/\b\w/g, function (c) { return c.toUpperCase(); });
            // Render the input EMPTY regardless of stored value — the
            // value coming back from /api/config is the masked
            // placeholder anyway (api masks driver config_secrets on
            // GET), but inserting any value into a `value=""` attribute
            // exposes it in the DOM/HTML. Mirror the cloud-password
            // pattern instead: empty input + saved/missing badge.
            // config_secrets come back masked: a non-empty value means the api
            // sent the placeholder (e.g. api_token). For the "password" key the
            // api blanks config.password but sets the driver-level has_password
            // flag, so honour that too — otherwise a saved password reads as unset.
            var saved = (typeof dcfg[key] === "string" && dcfg[key] !== "") ||
                        (key === "password" && d.has_password === true);
            var badge = saved
              ? '<span class="creds-badge creds-saved">✓ Set — not shown here</span>'
              : '<span class="creds-badge creds-missing">⚠ Not set</span>';
            var placeholder = saved
              ? "•••••••• (leave empty to keep)"
              : "Paste from device web UI";
            fs +=
              '<label>' + escHtml(label) + ' ' + badge + '</label>' +
              '<input type="password" autocomplete="off" ' +
              'data-path="drivers.' + dIdx + '.config.' + escHtml(key) + '" ' +
              'value="" placeholder="' + escHtml(placeholder) + '">';
          });
          fs += '</fieldset>';
          slot.innerHTML = fs;
        });
        bodyEl.querySelectorAll(".drv-disable-pv").forEach(function (lbl) {
          var lua = lbl.getAttribute("data-drv-lua");
          var entry = lua && byLua[lua];
          if (!entry) return;
          var caps = entry.capabilities || [];
          if (caps.indexOf("meter") >= 0 && caps.indexOf("pv") >= 0) {
            lbl.style.display = "flex";
          }
        });
        bodyEl.querySelectorAll(".driver-battery-capacity").forEach(function (wrap) {
          var lua = wrap.getAttribute("data-drv-lua");
          var entry = lua && byLua[lua];
          var row = wrap.closest(".device-core-row");
          var caps = (entry && entry.capabilities) || [];
          var show = caps.indexOf("battery") >= 0;
          wrap.hidden = !show;
          wrap.style.display = show ? "" : "none";
          if (row) row.classList.toggle("field-row-single", !show);
        });
        // Local-API credential fields (username + cert pin) reveal only for
        // drivers whose catalog declares apicreds — done post-fetch so a first
        // render before the catalog resolves doesn't drop them.
        bodyEl.querySelectorAll(".drv-local-creds").forEach(function (wrap) {
          var lua = wrap.getAttribute("data-drv-lua");
          var entry = lua && byLua[lua];
          var caps = (entry && entry.capabilities) || [];
          wrap.hidden = caps.indexOf("apicreds") < 0;
        });
        var sel = document.getElementById("driver-catalog-picker");
        if (!sel) return;
        sel.innerHTML = "";
        if (entries.length === 0) {
          sel.innerHTML = "<option value=''>(no drivers found in drivers/)</option>";
          return;
        }
        entries.forEach(function (e) {
          var opt = document.createElement("option");
          opt.value = e.path;
          var protoLabel = (e.protocols || []).join("+");
          var badge =
            e.verification_status === "production" ? "🟢 " :
            e.verification_status === "beta" ? "🟡 " : "🔴 ";
          opt.textContent = badge + (e.name || e.filename) + "  —  " + (e.manufacturer || "?") + "  [" + protoLabel + "]" + (e.version ? "  v" + e.version : "");
          opt.dataset.protocols = protoLabel;
          opt.dataset.id = e.id || "";
          opt.dataset.httpHosts = (e.http_hosts || []).join(",");
          opt.dataset.connectionHost = (e.connection_defaults && e.connection_defaults.host) || "";
          opt.dataset.connPort = (e.connection_defaults && e.connection_defaults.port) || "";
          opt.dataset.verificationStatus = e.verification_status || "experimental";
          if (e.verification_notes) opt.title = e.verification_notes;
          sel.appendChild(opt);
        });
      });

      var btn = document.getElementById("driver-catalog-add");
      if (btn) btn.addEventListener("click", function () {
        var sel = document.getElementById("driver-catalog-picker");
        var nameEl = document.getElementById("driver-catalog-name");
        if (!sel || !sel.value) return;
        ctx.captureCurrentTab();
        var chosen = sel.options[sel.selectedIndex];
        var protocols = (chosen.dataset.protocols || "").split("+");
        var name = (nameEl.value || "").trim() || chosen.dataset.id || ("driver-" + config.drivers.length);
        var driver = { name: name, lua: sel.value };
        driver.capabilities = {};
        if (protocols.indexOf("mqtt") >= 0) driver.capabilities.mqtt = { host: "", port: 1883 };
        if (protocols.indexOf("modbus") >= 0) driver.capabilities.modbus = { host: "", port: 502, unit_id: 1 };
        if (protocols.indexOf("http") >= 0) {
          var hosts = (chosen.dataset.httpHosts || "").split(",").filter(Boolean);
          driver.capabilities.http = { allowed_hosts: hosts };
          // Vehicle drivers (e.g. tesla_vehicle) take {ip, vin}, not
          // {host} or {email,password,serial}. Detect via catalog
          // capability so existing local-HTTP and cloud branches stay
          // untouched.
          var entry = (S.catalogByLua || {})[sel.value];
          var entryCaps = (entry && entry.capabilities) || [];
          var connHost = chosen.dataset.connectionHost || "";
          if (entryCaps.indexOf("vehicle") >= 0) {
            driver.config = { ip: "", vin: "" };
          } else if (connHost) {
            driver.config = { host: connHost };
          } else if (entryCaps.indexOf("apicreds") >= 0 && (chosen.dataset.connPort || "")) {
            // Local HTTP device with HTTP Basic auth (e.g. NIBE local REST API):
            // host + username + password + an optional self-signed cert pin. The
            // backend auto-derives capabilities.http.allowed_hosts from config.host.
            driver.config = { host: "", username: "", password: "" };
          } else if (entryCaps.indexOf("apicreds") >= 0) {
            // OAuth2 client_credentials drivers (e.g. MyUplink).
            driver.config = { client_id: "", client_secret: "" };
          } else {
            driver.config = { email: "", password: "", serial: "" };
          }
        }
        config.drivers.push(driver);
        ctx.renderTab("devices");
      });

      // Cloud-driver Connect buttons.
      bodyEl.querySelectorAll(".ev-connect-btn").forEach(function (connectBtn) {
        connectBtn.addEventListener("click", function () {
          var dIdx = connectBtn.dataset.driverIdx;
          var statusEl = document.getElementById("ev-connect-status-" + dIdx);
          var sel = document.getElementById("ev-charger-select-" + dIdx);
          var emailInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.email"]');
          var pwInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.password"]');
          var email = emailInput ? emailInput.value : "";
          var pw = pwInput ? pwInput.value : "";
          if (!email) { if (statusEl) statusEl.textContent = "Enter email first"; return; }
          if (statusEl) statusEl.textContent = "Connecting...";
          connectBtn.disabled = true;
          var dCfg = config && config.drivers ? config.drivers[dIdx] : null;
          var provider = "easee";
          if (dCfg && typeof dCfg.lua === "string" && dCfg.lua !== "") {
            provider = dCfg.lua
              .replace(/^.*[\\/]/, "")
              .replace(/\.lua$/i, "")
              .replace(/_cloud$/i, "");
            if (!provider) provider = "easee";
          }
          ownerFetch("/api/ev/chargers", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ provider: provider, email: email, password: pw }),
          }).then(function (r) {
            if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || "HTTP " + r.status); });
            return r.json();
          }).then(function (chargers) {
            if (!sel || !Array.isArray(chargers) || chargers.length === 0) {
              if (statusEl) statusEl.textContent = "No chargers found";
              return;
            }
            var d = config.drivers[dIdx];
            var current = (d && d.config && d.config.serial) || "";
            sel.innerHTML = "";
            chargers.forEach(function (ch) {
              var opt = document.createElement("option");
              opt.value = ch.id;
              opt.textContent = ch.id + (ch.name ? "  —  " + ch.name : "");
              if (ch.id === current) opt.selected = true;
              sel.appendChild(opt);
            });
            var selected = sel.value;
            if (d && d.config) d.config.serial = selected;
            if (config.ev_charger) config.ev_charger.serial = selected;
            sel.onchange = function () {
              if (d && d.config) d.config.serial = sel.value;
              if (config.ev_charger) config.ev_charger.serial = sel.value;
            };
            if (statusEl) statusEl.textContent = chargers.length + " charger(s) found";
          }).catch(function (e) {
            if (statusEl) statusEl.textContent = "Error: " + e.message;
          }).finally(function () {
            connectBtn.disabled = false;
          });
        });
      });

      // Tesla "Verify connection" buttons. Issues a backend probe
      // against the configured proxy IP + VIN and renders the result
      // inline. Backend handles SSRF hardening — the UI just collects
      // the two fields and displays the response.
      bodyEl.querySelectorAll(".tesla-verify-btn").forEach(function (vbtn) {
        vbtn.addEventListener("click", function () {
          var dIdx = vbtn.dataset.driverIdx;
          var statusEl = bodyEl.querySelector('.tesla-verify-status[data-driver-idx="' + dIdx + '"]');
          var ipInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.ip"]');
          var vinInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.vin"]');
          var ip = ipInput ? ipInput.value.trim() : "";
          var vin = vinInput ? vinInput.value.trim() : "";
          if (!ip || !vin) {
            if (statusEl) statusEl.textContent = "Enter Proxy IP + VIN first";
            return;
          }
          if (statusEl) { statusEl.textContent = "Verifying…"; statusEl.style.color = "var(--text-dim)"; }
          vbtn.disabled = true;
          ownerFetch("/api/drivers/verify_tesla", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ ip: ip, vin: vin }),
          }).then(function (r) {
            return r.json().then(function (j) { return { ok: r.ok, status: r.status, body: j }; });
          }).then(function (res) {
            if (!statusEl) return;
            if (res.ok && res.body && res.body.ok) {
              var soc = res.body.soc_pct != null ? Math.round(res.body.soc_pct) + "%" : "?";
              var lim = res.body.charge_limit_pct != null ? Math.round(res.body.charge_limit_pct) + "%" : "?";
              var st = res.body.charging_state || "";
              statusEl.style.color = "var(--green-e)";
              statusEl.textContent = "✓ SoC " + soc + " · limit " + lim + (st ? " · " + st : "");
            } else {
              statusEl.style.color = "var(--red-e)";
              statusEl.textContent = "✗ " + ((res.body && res.body.error) || "verification failed");
            }
          }).catch(function (e) {
            if (statusEl) {
              statusEl.style.color = "var(--red-e)";
              statusEl.textContent = "✗ " + e.message;
            }
          }).finally(function () {
            vbtn.disabled = false;
          });
        });
      });

      // MyUplink (and any OAuth authorization-code apicreds driver): open the
      // provider consent in a new tab. /start reads the SAVED client_id, so
      // the operator must save the row first; we pass the browser's own
      // callback URL (location.origin) because on a relay origin the server
      // can't derive it. The refresh_token lands server-side; the operator
      // reloads to see the badge flip to Connected.
      bodyEl.querySelectorAll(".myuplink-connect-btn").forEach(function (cbtn) {
        cbtn.addEventListener("click", function () {
          var dIdx = cbtn.dataset.driverIdx;
          var name = cbtn.dataset.driverName || "";
          var statusEl = bodyEl.querySelector('.myuplink-connect-status[data-driver-idx="' + dIdx + '"]');
          function setStatus(msg, color) {
            if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || "var(--text-dim)"; }
          }
          if (!name) { setStatus("Save the driver name first", "var(--red-e)"); return; }
          setStatus("Opening MyUplink…");
          cbtn.disabled = true;
          var redirectURI = location.origin + "/api/oauth/myuplink/callback";
          var qs = "?driver=" + encodeURIComponent(name) +
            "&redirect_uri=" + encodeURIComponent(redirectURI);
          ownerFetch("/api/oauth/myuplink/start" + qs)
            .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
            .then(function (res) {
              if (!res.ok || !res.body || !res.body.authorize_url) {
                setStatus("✗ " + ((res.body && res.body.error) || "could not start consent — save Client ID + Secret first"), "var(--red-e)");
                return;
              }
              window.open(res.body.authorize_url, "_blank");
              setStatus("Complete the consent in the new tab. If it returns here, reload; if not, paste the URL below.", "var(--green-e)");
            })
            .catch(function (e) { setStatus("✗ " + e.message, "var(--red-e)"); })
            .finally(function () { cbtn.disabled = false; });
        });
      });

      // Manual fallback: exchange a pasted redirect URL (carries code + state)
      // server-side. Works on any origin since the Pi exchanges the code over
      // outbound HTTPS — no inbound callback required.
      bodyEl.querySelectorAll(".myuplink-manual-btn").forEach(function (mbtn) {
        mbtn.addEventListener("click", function () {
          var dIdx = mbtn.dataset.driverIdx;
          var input = bodyEl.querySelector('.myuplink-manual-url[data-driver-idx="' + dIdx + '"]');
          var statusEl = bodyEl.querySelector('.myuplink-connect-status[data-driver-idx="' + dIdx + '"]');
          function setStatus(msg, color) {
            if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || "var(--text-dim)"; }
          }
          var url = input ? input.value.trim() : "";
          if (!url) { setStatus("Paste the redirect URL first", "var(--red-e)"); return; }
          setStatus("Completing…");
          mbtn.disabled = true;
          ownerFetch("/api/oauth/myuplink/exchange", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ redirect_url: url }),
          })
            .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
            .then(function (res) {
              if (res.ok && res.body && res.body.status === "connected") {
                setStatus("✓ Connected — reload to refresh the badge.", "var(--green-e)");
              } else {
                setStatus("✗ " + ((res.body && res.body.error) || "exchange failed"), "var(--red-e)");
              }
            })
            .catch(function (e) { setStatus("✗ " + e.message, "var(--red-e)"); })
            .finally(function () { mbtn.disabled = false; });
        });
      });

      // Generic driver probe. Runs the current row's unsaved config through a
      // short-lived backend driver instance and dumps live readings/metrics
      // inline so the operator can verify host, credentials, and protocol.
      bodyEl.querySelectorAll(".driver-test-btn").forEach(function (testBtn) {
        testBtn.addEventListener("click", function () {
          var dIdx = testBtn.dataset.driverIdx;
          var statusEl = bodyEl.querySelector('.driver-test-status[data-driver-idx="' + dIdx + '"]');
          var outputEl = bodyEl.querySelector('.driver-test-output[data-driver-idx="' + dIdx + '"]');
          ctx.captureCurrentTab();
          var driver = config.drivers && config.drivers[dIdx];
          if (!driver) return;
          if (statusEl) {
            statusEl.textContent = "Testing...";
            statusEl.className = "driver-test-status";
          }
          if (outputEl) {
            outputEl.hidden = false;
            outputEl.innerHTML = '<div class="driver-test-empty">Waiting for live values...</div>';
          }
          testBtn.disabled = true;
          ownerFetch("/api/drivers/test", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(driver),
          }).then(function (r) {
            return r.json().then(function (j) { return { ok: r.ok, status: r.status, body: j }; });
          }).then(function (res) {
            var body = res.body || {};
            if (!res.ok) {
              body = { ok: false, error: body.error || ("HTTP " + res.status) };
            }
            if (statusEl) {
              statusEl.textContent = body.ok ? "Connected" : "Failed";
              statusEl.className = "driver-test-status " + (body.ok ? "ok" : "error");
            }
            if (outputEl) {
              outputEl.hidden = false;
              outputEl.innerHTML = renderProbeOutput(body);
            }
          }).catch(function (e) {
            if (statusEl) {
              statusEl.textContent = "Failed";
              statusEl.className = "driver-test-status error";
            }
            if (outputEl) {
              outputEl.hidden = false;
              outputEl.innerHTML = '<div class="driver-test-error">' + escHtml(e.message) + '</div>';
            }
          }).finally(function () {
            testBtn.disabled = false;
          });
        });
      });

      // Auto-sync capabilities.http.allowed_hosts from the configured
      // Proxy IP. Without this, a fresh tesla driver gets allowed_hosts=[]
      // (set by catalog-add) and every host.http_get call returns
      // "host not in allowed_hosts" — driver never reaches the proxy
      // and watchdog flips it stale. Strip any ":port" suffix; the
      // allowlist is matched on hostname only.
      bodyEl.querySelectorAll(".tesla-ip-input").forEach(function (inp) {
        function syncAllowedHosts() {
          var dIdx = inp.dataset.driverIdx;
          var d = config.drivers[dIdx];
          if (!d || !d.capabilities) return;
          if (!d.capabilities.http) d.capabilities.http = { allowed_hosts: [] };
          var ip = (inp.value || "").trim();
          var bare = ip.split(":")[0];
          d.capabilities.http.allowed_hosts = bare ? [bare] : [];
        }
        inp.addEventListener("input", syncAllowedHosts);
        inp.addEventListener("blur", syncAllowedHosts);
      });

      // Add/remove-device buttons.
      var addMqtt = document.getElementById("add-mqtt");
      var addModbus = document.getElementById("add-modbus");
      if (addMqtt) addMqtt.addEventListener("click", function () {
        ctx.captureCurrentTab();
        config.drivers.push({
          name: "new-device-" + (config.drivers.length + 1),
          lua: "drivers/new.lua",
          is_site_meter: false,
          battery_capacity_wh: 0,
          mqtt: { host: "", port: 1883, username: "", password: "" },
        });
        ctx.renderTab("devices");
      });
      if (addModbus) addModbus.addEventListener("click", function () {
        ctx.captureCurrentTab();
        config.drivers.push({
          name: "new-device-" + (config.drivers.length + 1),
          lua: "drivers/new.lua",
          is_site_meter: false,
          battery_capacity_wh: 0,
          modbus: { host: "", port: 502, unit_id: 1 },
        });
        ctx.renderTab("devices");
      });
      bodyEl.querySelectorAll("[data-remove-idx]").forEach(function (rmBtn) {
        rmBtn.addEventListener("click", function () {
          var idx = parseInt(rmBtn.dataset.removeIdx);
          ctx.captureCurrentTab();
          config.drivers.splice(idx, 1);
          ctx.renderTab("devices");
        });
      });
    },
  };
})();
