// Settings → Devices tab: driver catalog picker + per-driver editor.
// Owns its own add/remove/connect button wiring; the Settings shell
// stays driver-agnostic.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

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
        html += '<div class="device-item">' +
          '<div class="device-item-header">' +
          '<strong>' + escHtml(d.name) + '</strong>' +
          '<span style="color:var(--text-dim);font-size:0.75rem">lua · ' + protocol + ' · ' + escHtml(driverFile) + '</span>' +
          '<button class="btn-remove" data-remove-idx="' + idx + '">Remove</button>' +
          '</div>' +
          '<div class="field-row"><div>' +
          '<label>Driver file ' + help('Path to the .lua driver. Absolute or relative to the config file directory.') + '</label>' +
          '<input type="text" data-path="drivers.' + idx + '.lua" value="' + escHtml(driverFile) + '">' +
          '</div><div>' +
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
        var catalogEntry = (S.catalogByLua || {})[d.lua];
        var caps = (catalogEntry && catalogEntry.capabilities) || [];
        var isVehicleDriver = cap.http != null &&
          (caps.indexOf("vehicle") >= 0 ||
           Object.prototype.hasOwnProperty.call(dcfg, 'vin') ||
           Object.prototype.hasOwnProperty.call(dcfg, 'ip'));
        var isLocalHTTP = !isVehicleDriver && cap.http != null && hasHostField;
        var isCloudDriver = !isVehicleDriver && cap.http != null && !hasHostField &&
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
          // Render the Disable-PV checkbox for every HTTP driver; the
          // post-fetch pass in `after` hides it for drivers whose
          // catalog doesn't advertise BOTH meter + pv capabilities
          // (only those can double-count generation). Hiding via a
          // post-render DOM edit mirrors the site-meter pattern above
          // and avoids a re-render race with the async catalog fetch.
          html += '<fieldset><legend>HTTP</legend>' +
            '<label>Host / IP ' + help('Hostname (e.g. zap.local) or IP address of the device. mDNS names work when your OS resolver supports them; otherwise use the LAN IP.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.host" value="' + escHtml(lcfg.host || '') + '" placeholder="zap.local">' +
            '<label class="drv-disable-pv" data-drv-lua="' + escHtml(d.lua || '') + '" style="margin-top:8px;display:none;align-items:center;gap:6px;font-weight:normal">' +
              '<input type="checkbox" data-checkbox-path="drivers.' + idx + '.config.disable_pv"' +
              (lcfg.disable_pv ? ' checked' : '') + '>' +
              'Disable PV readings ' +
              help('Use this gateway for the P1 meter only. When another driver already owns PV aggregation, set this so the two drivers don\'t double-count generation.') +
            '</label>' +
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

      // Driver catalog picker — fetch async, render into select.
      fetch("/api/drivers/catalog").then(function (r) { return r.json(); }).then(function (data) {
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
          if (secrets.length === 0) return;
          var dcfg = d.config || {};
          var fs = '<fieldset><legend>Secrets</legend>';
          secrets.forEach(function (key) {
            var label = key.replace(/_/g, " ").replace(/\b\w/g, function (c) { return c.toUpperCase(); });
            var v = dcfg[key] || "";
            fs +=
              '<label>' + label + '</label>' +
              '<input type="password" autocomplete="off" ' +
              'data-path="drivers.' + dIdx + '.config.' + key + '" ' +
              'value="' + (v ? v.replace(/"/g, "&quot;") : "") + '" ' +
              'placeholder="Paste from device web UI">';
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
          fetch("/api/ev/chargers", {
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
          fetch("/api/drivers/verify_tesla", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ ip: ip, vin: vin }),
          }).then(function (r) {
            return r.json().then(function (j) { return { ok: r.ok, body: j }; });
          }).then(function (res) {
            if (!statusEl) return;
            if (res.ok && res.body && res.body.ok) {
              var soc = res.body.soc_pct != null ? Math.round(res.body.soc_pct) + "%" : "?";
              var lim = res.body.charge_limit_pct != null ? Math.round(res.body.charge_limit_pct) + "%" : "?";
              var st = res.body.charging_state || "";
              statusEl.style.color = "#2a7";
              statusEl.textContent = "✓ SoC " + soc + " · limit " + lim + (st ? " · " + st : "");
            } else {
              statusEl.style.color = "#c44";
              statusEl.textContent = "✗ " + ((res.body && res.body.error) || "verification failed");
            }
          }).catch(function (e) {
            if (statusEl) {
              statusEl.style.color = "#c44";
              statusEl.textContent = "✗ " + e.message;
            }
          }).finally(function () {
            vbtn.disabled = false;
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
