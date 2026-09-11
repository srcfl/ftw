// setup.js — multi-step setup wizard for FTW
// Single-page state machine: show/hide step divs, collect config, POST to /api/config.

(function () {
  'use strict';

  var TOTAL_STEPS = 8;
  var currentStep = 1;

  // Collected state
  var configuredDrivers = [];    // array of driver objects ready for config.drivers
  var selectedDevice = null;     // { ip, port, protocol, hostname } from scan or manual entry
  var selectedCatalog = null;    // CatalogEntry from /api/drivers/catalog
  var driverCatalog = [];        // full catalog cache

  // Picker option for a device with no catalog entry. Deliberately not a
  // number so it can never be mistaken for a catalog index.
  var NOT_LISTED = '__not_listed__';

  // --- Step navigation ---

  function renderDots() {
    var container = document.getElementById('step-dots');
    container.innerHTML = '';
    for (var i = 1; i <= TOTAL_STEPS; i++) {
      var dot = document.createElement('div');
      dot.className = 'step-dot';
      if (i === currentStep) dot.className += ' active';
      else if (i < currentStep) dot.className += ' done';
      container.appendChild(dot);
    }
  }

  window.goStep = function (n) {
    if (n < 1 || n > TOTAL_STEPS) return;

    // Pre-step hooks
    if (n === 4) loadCatalog();
    if (n === 6) renderDriversSummary();
    if (n === 7) prepareIntegrations();
    if (n === 8) renderReview();

    currentStep = n;
    var steps = document.querySelectorAll('.step');
    for (var i = 0; i < steps.length; i++) {
      steps[i].classList.remove('visible');
    }
    document.getElementById('step-' + n).classList.add('visible');
    renderDots();
    window.scrollTo(0, 0);
  };

  // Back from step 7 goes to step 6 if we have drivers, step 2 if we skipped
  window.goStepBack7 = function () {
    goStep(configuredDrivers.length > 0 ? 6 : 2);
  };

  // Forward path for a device the catalog cannot serve: the devices
  // summary when something is already configured, otherwise straight on
  // to the integrations.
  window.skipUnlistedDevice = function () {
    goStep(configuredDrivers.length > 0 ? 6 : 7);
  };

  // --- Step 3: Scan ---

  window.startScan = function () {
    var statusEl = document.getElementById('scan-status');
    var resultsEl = document.getElementById('scan-results');
    statusEl.style.display = 'block';
    statusEl.innerHTML = '<span class="spinner"></span> Scanning network...';
    resultsEl.style.display = 'none';

    fetch('/api/scan?fingerprint=1')
      .then(function (r) { return r.json(); })
      .then(function (data) {
        // API returns a raw array, not {devices: [...]}
        var devices = Array.isArray(data) ? data : (data.devices || []);
        if (devices.length === 0) {
          statusEl.innerHTML = 'No devices found. Try entering the IP manually.';
          return;
        }
        statusEl.style.display = 'none';
        var tbody = document.getElementById('scan-tbody');
        tbody.innerHTML = '';
        devices.sort(function (a, b) {
          return Number(Boolean(b.matches && b.matches.length)) - Number(Boolean(a.matches && a.matches.length));
        });
        devices.forEach(function (d) {
          var tr = document.createElement('tr');
          var proto = d.protocol || 'modbus';
          var match = d.matches && d.matches.length ? d.matches[0] : null;
          var address = esc(d.ip);
          if (d.hostname) address += '<span class="dev-host">' + esc(d.hostname) + '</span>';
          var identity = '<span class="dev-unknown">Unrecognised</span>';
          if (match) {
            var label = match.name || match.make || match.driver;
            var detail = [match.model, match.serial ? 'SN ' + match.serial : ''].filter(Boolean).join(' · ');
            identity = '<span class="dev-name">' + esc(label) + '</span>' +
              (detail ? '<span class="dev-caps">' + esc(detail) + '</span>' : '');
          }
          tr.innerHTML =
            '<td>' + address + '</td>' +
            '<td>' + esc(String(d.port)) + '</td>' +
            '<td>' + esc(proto) + '</td>' +
            '<td>' + identity + '</td>' +
            '<td><button class="btn-use">Use this device</button></td>';
          tr.querySelector('.btn-use').addEventListener('click', function () {
            useScanDevice(d, proto, match && match.driver);
          });
          tbody.appendChild(tr);
        });
        resultsEl.style.display = 'block';
      })
      .catch(function (err) {
        statusEl.innerHTML = 'Scan failed: ' + esc(err.message) +
          '. Try entering the IP manually.';
      });
  };

  window.showManualIP = function () {
    document.getElementById('manual-ip-form').style.display = 'block';
    document.getElementById('manual-ip-toggle').style.display = 'none';
  };

  function useScanDevice(dev, protocol, matchedFilename) {
    selectedDevice = {
      ip: dev.ip,
      port: dev.port,
      protocol: protocol,
      hostname: dev.hostname || '',
      matchedFilename: matchedFilename || ''
    };
    goStep(4);
  }

  window.useManualDevice = function () {
    var host = document.getElementById('manual-ip').value.trim();
    var port = parseInt(document.getElementById('manual-port').value, 10) || 502;
    if (!host) return;
    selectedDevice = { ip: host, port: port, protocol: guessProtocol(port), hostname: '' };
    goStep(4);
  };

  function guessProtocol(port) {
    if (port === 1883 || port === 8883) return 'mqtt';
    if (port === 80 || port === 443 || port === 8080) return 'http';
    return 'modbus';
  }

  // --- Step 4: Driver catalog ---

  function loadCatalog() {
    if (driverCatalog.length > 0) {
      populateDriverDropdown();
      return;
    }
    fetch('/api/drivers/catalog')
      .then(function (r) { return r.json(); })
      .then(function (data) {
        driverCatalog = data.entries || [];
        populateDriverDropdown();
      })
      .catch(function () {
        driverCatalog = [];
        populateDriverDropdown();
      });
  }

  // DRIVER.verification_status, in words an operator can act on.
  function verificationLabel(status) {
    if (status === 'production') return 'verified on hardware';
    if (status === 'beta') return 'in testing';
    if (status === 'experimental') return 'untested';
    return '';
  }

  function populateDriverDropdown() {
    var sel = document.getElementById('driver-select');
    sel.innerHTML = '<option value="">-- Select a driver --</option>';

    var proto = selectedDevice ? selectedDevice.protocol : null;

    driverCatalog.forEach(function (entry, idx) {
      // Filter by protocol if we have one
      if (proto && entry.protocols && entry.protocols.length > 0) {
        var match = entry.protocols.some(function (p) {
          return p.toLowerCase() === proto.toLowerCase();
        });
        if (!match) return;
      }

      var label = '';
      var displayName = entry.name || entry.filename;
      // Several drivers already begin their name with the manufacturer, which
      // read as "Sungrow Sungrow SH Hybrid Inverter".
      if (entry.manufacturer && displayName.toLowerCase()
            .indexOf(entry.manufacturer.toLowerCase()) !== 0) {
        label += entry.manufacturer + ' ';
      }
      label += displayName;
      if (entry.protocols && entry.protocols.length > 0) {
        label += ' (' + entry.protocols.join(', ');
        if (entry.capabilities && entry.capabilities.length > 0) {
          label += ', ' + entry.capabilities.join('+');
        }
        label += ')';
      }
      // Say how well tested it is. The signed channel carries drivers that
      // have run on customer sites for months alongside ones nobody has ever
      // put on hardware, and an operator choosing between them should not have
      // to guess which is which.
      var verdict = verificationLabel(entry.verification_status);
      if (verdict) label += ' — ' + verdict;

      var opt = document.createElement('option');
      opt.value = String(idx);
      opt.textContent = label;
      sel.appendChild(opt);
    });

    // The escape hatch. Without it, a device with no catalog entry
    // dead-ends this step: Continue stays disabled and the only
    // affordance left is Back.
    var notListed = document.createElement('option');
    notListed.value = NOT_LISTED;
    notListed.textContent = 'My device is not listed…';
    sel.appendChild(notListed);

    // A positive fingerprint preselects the matching catalog driver while
    // still sending the operator through the normal configuration form.
    if (selectedDevice && selectedDevice.matchedFilename) {
      for (var i = 0; i < sel.options.length; i++) {
        var value = sel.options[i].value;
        if (value === '') continue;
        var entry = driverCatalog[parseInt(value, 10)];
        if (entry && entry.filename === selectedDevice.matchedFilename) {
          sel.value = value;
          window.onDriverSelected();
          break;
        }
      }
    }
  }

  window.onDriverSelected = function () {
    var sel = document.getElementById('driver-select');
    var btn = document.getElementById('driver-next-btn');
    var descEl = document.getElementById('driver-description');
    var notListedEl = document.getElementById('driver-not-listed');

    if (!sel.value) {
      selectedCatalog = null;
      btn.disabled = true;
      descEl.style.display = 'none';
      notListedEl.style.display = 'none';
      return;
    }

    if (sel.value === NOT_LISTED) {
      // Nothing to configure, so Continue stays held; the panel offers
      // its own forward path instead.
      selectedCatalog = null;
      btn.disabled = true;
      descEl.style.display = 'none';
      notListedEl.style.display = 'block';
      return;
    }
    notListedEl.style.display = 'none';

    selectedCatalog = driverCatalog[parseInt(sel.value, 10)];
    btn.disabled = false;

    var lines = [];
    if (selectedCatalog.filename === 'zap.lua' || selectedCatalog.id === 'sourceful-zap') {
      lines.push('This driver is the P1/HAN site meter by default. Add inverters, batteries and chargers as their own devices in FTW when you can. If Zap is the only reader, turn on PV or battery ingest later under Settings → Devices.');
    }
    if (selectedCatalog.description) lines.push(selectedCatalog.description);

    var version = selectedCatalog.installed_version || selectedCatalog.version;
    var provenance = [];
    if (version) provenance.push('version ' + version);
    // Where the file came from: an operator's own override, a signed artifact
    // from the channel, or the copy bundled with this build.
    if (selectedCatalog.source === 'local') provenance.push('your own override');
    else if (selectedCatalog.source === 'managed') provenance.push('signed driver channel');
    else if (selectedCatalog.source === 'bundled') provenance.push('bundled with this build');
    if (provenance.length) lines.push(provenance.join(' · '));

    var verdict = verificationLabel(selectedCatalog.verification_status);
    if (verdict) {
      var note = selectedCatalog.verification_notes;
      lines.push(note ? verdict + ' — ' + note : verdict);
    }

    if (lines.length) {
      descEl.textContent = '';
      lines.forEach(function (line, i) {
        var el = document.createElement('div');
        el.textContent = line;
        if (i > 0) el.style.marginTop = '4px';
        descEl.appendChild(el);
      });
      descEl.style.display = 'block';
    } else {
      descEl.style.display = 'none';
    }

    // Pre-fill step 5
    prefillDriverConfig();
  };

  // --- Step 5: Configure driver ---

  function prefillDriverConfig() {
    if (!selectedCatalog) return;

    var nameBase = (selectedCatalog.manufacturer || selectedCatalog.id || 'device').toLowerCase()
      .replace(/[^a-z0-9]/g, '_').replace(/_+/g, '_').replace(/^_|_$/g, '');
    // Make unique if name already taken
    var name = nameBase;
    var n = 2;
    while (configuredDrivers.some(function (d) { return d.name === name; })) {
      name = nameBase + '_' + n;
      n++;
    }
    document.getElementById('drv-name').value = name;

    if (selectedDevice) {
      // Prefer the device's self-broadcast mDNS (.local) name over the raw
      // IP: the name follows the device across DHCP lease changes, the IP
      // doesn't. Other reverse-DNS names stay display-only — resolving them
      // at runtime depends on the router, so the IP is the safer default.
      var host = isMDNSName(selectedDevice.hostname) ? selectedDevice.hostname : selectedDevice.ip;
      document.getElementById('drv-ip').value = host;
      document.getElementById('drv-port').value = selectedDevice.port;
    }
    updateHostHint();

    // Show/hide unit ID for modbus
    var isModbus = !selectedCatalog.protocols || selectedCatalog.protocols.length === 0 ||
      selectedCatalog.protocols.some(function (p) { return p === 'modbus'; });
    document.getElementById('drv-unitid-group').style.display = isModbus ? 'block' : 'none';

    // Read-only gateway drivers may expose battery telemetry, but battery
    // capacity is the control-pool opt-in. Never offer that field for them.
    var hasBattery = selectedCatalog.capabilities &&
      selectedCatalog.capabilities.some(function (c) { return c === 'battery'; }) &&
      !selectedCatalog.read_only;
    document.getElementById('drv-battery-group').style.display = hasBattery ? 'block' : 'none';

    // First driver defaults to site meter
    document.getElementById('drv-site-meter').checked = configuredDrivers.length === 0;

    // Default port based on protocol, with a driver-specific override for
    // appliances whose official local API uses a different well-known port.
    if (!selectedDevice) {
      var defPort = 502;
      if (selectedCatalog.protocols && selectedCatalog.protocols.indexOf('mqtt') >= 0) defPort = 1883;
      if (selectedCatalog.protocols && selectedCatalog.protocols.indexOf('http') >= 0) defPort = 8080;
      if (selectedCatalog.connection_defaults && selectedCatalog.connection_defaults.port) {
        defPort = selectedCatalog.connection_defaults.port;
      }
      document.getElementById('drv-port').value = defPort;
    }

    // Render password inputs for each catalog-declared config secret
    // (e.g. sonnen Auth-Token). Each input id is "drv-secret-<key>" and
    // saveDriver reads them back into driver.config[<key>].
    var secretsGroup = document.getElementById('drv-secrets-group');
    secretsGroup.innerHTML = '';
    var secrets = selectedCatalog.config_secrets || [];
    secrets.forEach(function (key) {
      var label = key.replace(/_/g, ' ').replace(/\b\w/g, function (c) { return c.toUpperCase(); });
      var fg = document.createElement('div');
      fg.className = 'form-group';
      fg.innerHTML =
        '<label for="drv-secret-' + key + '">' + esc(label) + '</label>' +
        '<input type="password" id="drv-secret-' + key + '" autocomplete="off" ' +
        'placeholder="Paste from device web UI">';
      secretsGroup.appendChild(fg);
    });
  }

  // A self-broadcast mDNS name (RFC 6762 .local). Only these are safe to
  // prefer over the IP — any other hostname needs the router's DNS to
  // resolve, which FTW can't verify from here.
  function isMDNSName(host) {
    return /\.local\.?$/i.test(host || '');
  }

  function isIPv4Literal(host) {
    return /^\d{1,3}(\.\d{1,3}){3}$/.test(host || '');
  }

  // Addressing hint under the host field: a .local name survives DHCP
  // lease changes; a raw IP only stays valid if the operator reserves it
  // for the device in the router's DHCP pool. Tracks user edits live.
  function updateHostHint() {
    var el = document.getElementById('drv-host-hint');
    var input = document.getElementById('drv-ip');
    if (!el || !input) return;
    var host = input.value.trim();
    el.style.display = 'none';
    el.className = 'host-hint';
    if (isMDNSName(host)) {
      var discovered = '';
      if (selectedDevice && selectedDevice.ip &&
          host.toLowerCase() === (selectedDevice.hostname || '').toLowerCase()) {
        discovered = ' (discovered at ' + selectedDevice.ip + ')';
      }
      el.textContent = 'Connecting by the device’s mDNS name' + discovered +
        ' — it keeps working even if your router assigns the device a new IP address.';
      el.className = 'host-hint mdns';
      el.style.display = 'block';
    } else if (isIPv4Literal(host)) {
      el.textContent = 'No mDNS (.local) name in use — FTW will connect to this fixed IP address. ' +
        'Reserve it for the device in your router’s DHCP settings so a new ' +
        'DHCP lease can’t move the device and break the connection.';
      el.className = 'host-hint dhcp';
      el.style.display = 'block';
    }
  }

  window.saveDriver = function () {
    var name = document.getElementById('drv-name').value.trim();
    if (!name) { alert('Driver name is required.'); return; }

    var host = document.getElementById('drv-ip').value.trim();
    var port = parseInt(document.getElementById('drv-port').value, 10);
    var unitId = parseInt(document.getElementById('drv-unitid').value, 10) || 1;
    var isSiteMeter = document.getElementById('drv-site-meter').checked;
    // Only read battery capacity for drivers that actually support it —
    // the <input> has a default value of 10, so without this gate a
    // PV-only driver (e.g. solaredge_pv) would persist a phantom
    // battery_capacity_wh = 10000 that the control loop later tries to
    // target against a device with no battery.
    var hasBattery = selectedCatalog && selectedCatalog.capabilities &&
      selectedCatalog.capabilities.some(function (c) { return c === 'battery'; }) &&
      !selectedCatalog.read_only;
    var batteryKwh = hasBattery
      ? (parseFloat(document.getElementById('drv-battery-kwh').value) || 0)
      : 0;

    if (!host) { alert('IP address or hostname is required.'); return; }

    // Determine protocol from catalog entry
    var protocol = 'modbus';
    if (selectedCatalog && selectedCatalog.protocols && selectedCatalog.protocols.length > 0) {
      protocol = selectedCatalog.protocols[0];
    }

    var driver = {
      name: name,
      lua: selectedCatalog ? selectedCatalog.path : '',
      is_site_meter: isSiteMeter,
      capabilities: {}
    };

    var hasBatteryTelemetry = selectedCatalog && selectedCatalog.capabilities &&
      selectedCatalog.capabilities.some(function (c) { return c === 'battery'; });
    if (hasBatteryTelemetry && selectedCatalog.read_only) {
      driver.battery_telemetry_only = true;
    }

    if (batteryKwh > 0) {
      driver.battery_capacity_wh = batteryKwh * 1000;
    }

    if (protocol === 'modbus') {
      driver.capabilities.modbus = { host: host, port: port, unit_id: unitId };
    } else if (protocol === 'mqtt') {
      driver.capabilities.mqtt = { host: host, port: port };
    } else if (protocol === 'http') {
      driver.capabilities.http = { allowed_hosts: [host] };
      // connection_defaults.host is declared only by drivers that take a
      // user-configurable local endpoint — seed config.host from the IP the
      // user just entered. Cloud drivers (Easee etc.) declare http_hosts
      // for allowed-hosts handling but have no connection_defaults.host;
      // their vendor endpoint is hardcoded and they key off
      // config.email/password instead, so leave config untouched here.
      var connDefaults = (selectedCatalog && selectedCatalog.connection_defaults) || {};
      // connection_defaults.host being declared (even when empty) is the
      // signal that this driver takes a user-configurable local endpoint.
      // Cloud drivers don't declare it and key off email/password.
      if (Object.prototype.hasOwnProperty.call(connDefaults, 'host')) {
        driver.config = driver.config || {};
        driver.config.host = host;
      }
    }

    // Persist per-driver secrets the prefill rendered (api_token, etc.)
    // into driver.config.<key>. Lives OUTSIDE the protocol branches so
    // a future Modbus/MQTT driver that declares config_secrets still
    // captures them — the previous version silently dropped operator-
    // entered secrets for any driver whose top-protocol wasn't http.
    var secrets = (selectedCatalog && selectedCatalog.config_secrets) || [];
    secrets.forEach(function (key) {
      var el = document.getElementById('drv-secret-' + key);
      if (!el) return;
      var v = (el.value || '').trim();
      if (v === '') return;
      driver.config = driver.config || {};
      driver.config[key] = v;
    });

    // If this is the site meter, uncheck others
    if (isSiteMeter) {
      configuredDrivers.forEach(function (d) { d.is_site_meter = false; });
    }

    // Store catalog ref for display
    driver._catalog = selectedCatalog;

    configuredDrivers.push(driver);
    selectedDevice = null;
    selectedCatalog = null;
    goStep(6);
  };

  // --- Driver description helper ---

  function driverDetail(d) {
    var detail = '';
    if (d.capabilities.modbus) {
      detail = d.capabilities.modbus.host + ':' + d.capabilities.modbus.port;
    } else if (d.capabilities.mqtt) {
      detail = d.capabilities.mqtt.host + ':' + d.capabilities.mqtt.port;
    } else if (d.capabilities.http) {
      detail = (d.config && d.config.host) || (d.capabilities.http.allowed_hosts || [])[0] || '';
    }
    var tags = [];
    if (d.is_site_meter) tags.push('site meter');
    if (d.battery_capacity_wh) tags.push((d.battery_capacity_wh / 1000).toFixed(1) + ' kWh battery');
    if (tags.length > 0) detail += ' (' + tags.join(', ') + ')';
    return detail;
  }

  // --- Step 6: Drivers summary ---

  function renderDriversSummary() {
    var container = document.getElementById('drivers-summary');
    container.innerHTML = '';

    if (configuredDrivers.length === 0) {
      container.innerHTML = '<p style="color:var(--text-dim);font-size:0.85rem;">No devices configured yet.</p>';
      return;
    }

    configuredDrivers.forEach(function (d, idx) {
      var item = document.createElement('div');
      item.className = 'driver-summary-item';
      item.innerHTML =
        '<div class="driver-info">' +
          '<span class="driver-label">' + esc(d.name) + '</span>' +
          '<span class="driver-detail">' + esc(driverDetail(d)) + '</span>' +
        '</div>';
      var btn = document.createElement('button');
      btn.className = 'btn-remove-sm';
      btn.textContent = 'Remove';
      btn.addEventListener('click', function () { removeDriver(idx); });
      item.appendChild(btn);
      container.appendChild(item);
    });
  }

  function removeDriver(idx) {
    configuredDrivers.splice(idx, 1);
    renderDriversSummary();
  }

  window.addAnotherDevice = function () {
    selectedDevice = null;
    selectedCatalog = null;
    goStep(3);
  };

  // --- Step 7: Integrations ---

  var integrationListenersBound = false;

  // Known EV charger providers, keyed by the `provider` string the Go
  // config (EVCharger.Provider) accepts. `transport` selects which field
  // block (#ev-fields-http vs #ev-fields-modbus) the wizard reveals:
  //   - easee:    cloud HTTP, needs username/password + serial lookup.
  //   - zaptec:   cloud HTTP, needs username/password + serial lookup.
  //   - tesla-wc: local HTTP, needs host, no auth. Observation only.
  //   - ctek:     local Modbus/TCP, needs host/port/unit, no auth.
  // Mirrors go/internal/config/config.go EVCharger.Validate.
  var EV_PROVIDERS = [
    { value: 'easee', label: 'Easee', transport: 'http' },
    { value: 'zaptec', label: 'Zaptec', transport: 'http' },
    { value: 'tesla-wc', label: 'Tesla Wall Connector', transport: 'http-local' },
    { value: 'ctek', label: 'CTEK', transport: 'modbus' }
  ];

  function evProviderTransport(value) {
    for (var i = 0; i < EV_PROVIDERS.length; i++) {
      if (EV_PROVIDERS[i].value === value) return EV_PROVIDERS[i].transport;
    }
    return null;
  }

  function evProviderLabel(value) {
    for (var i = 0; i < EV_PROVIDERS.length; i++) {
      if (EV_PROVIDERS[i].value === value) return EV_PROVIDERS[i].label;
    }
    return value;
  }

  function populateEVProviders() {
    var sel = document.getElementById('ev-provider');
    if (!sel || sel.dataset.populated === '1') return;
    // Keep the existing "None" option (value="") as the first child.
    EV_PROVIDERS.forEach(function (p) {
      var opt = document.createElement('option');
      opt.value = p.value;
      opt.textContent = p.label;
      sel.appendChild(opt);
    });
    sel.dataset.populated = '1';
  }

  // Reveal the field block matching the selected provider's transport.
  function syncEVFields() {
    var provider = document.getElementById('ev-provider').value;
    var transport = evProviderTransport(provider);
    document.getElementById('ev-fields').style.display = provider ? 'block' : 'none';
    document.getElementById('ev-fields-http').style.display = transport === 'http' ? 'block' : 'none';
    var localHttp = document.getElementById('ev-fields-http-local');
    if (localHttp) localHttp.style.display = transport === 'http-local' ? 'block' : 'none';
    document.getElementById('ev-fields-modbus').style.display = transport === 'modbus' ? 'block' : 'none';
    // Probe is offered for cloud accounts and for a LAN Tesla Wall Connector.
    var canProbe = transport === 'http' || transport === 'http-local';
    document.getElementById('ev-load-chargers').style.display = canProbe ? '' : 'none';
    if (!canProbe) {
      document.getElementById('ev-serial-group').style.display = 'none';
    }
  }

  function prepareIntegrations() {
    var zone = document.getElementById('price-zone').value;
    document.getElementById('price-zone-readonly').value = zone;

    populateEVProviders();
    syncEVFields();

    if (!integrationListenersBound) {
      integrationListenersBound = true;
      document.getElementById('ev-provider').addEventListener('change', syncEVFields);
      document.getElementById('ha-enabled').addEventListener('change', function () {
        document.getElementById('ha-fields').style.display = this.checked ? 'block' : 'none';
      });
    }
  }

  // Populate the <select id="ev-serial"> by calling /api/ev/chargers —
  // mirrors the settings screen so operators don't have to transcribe a
  // serial off the side of the charger. The serial field only appears
  // after a successful call returns at least one device.
  function evLocalHTTPBase(host) {
    host = (host || '').trim();
    if (!host) return '';
    if (host.indexOf('://') >= 0) return host.replace(/\/$/, '');
    return 'http://' + host.replace(/\/$/, '');
  }

  window.loadEVChargers = function () {
    var provider = document.getElementById('ev-provider').value || 'easee';
    var transport = evProviderTransport(provider);
    var username = document.getElementById('ev-username').value.trim();
    var password = document.getElementById('ev-password').value;
    var btn = document.getElementById('ev-load-chargers');
    var statusEl = document.getElementById('ev-chargers-status');
    var group = document.getElementById('ev-serial-group');
    var sel = document.getElementById('ev-serial');
    var body;

    statusEl.style.display = 'inline';
    statusEl.style.color = 'var(--fg-muted)';
    if (transport === 'http-local') {
      var localHost = document.getElementById('ev-http-host').value.trim();
      if (!localHost) { statusEl.textContent = 'Enter the wall connector IP first'; return; }
      body = { provider: provider, http: { base_url: evLocalHTTPBase(localHost) } };
    } else {
      if (!username) { statusEl.textContent = 'Enter username first'; return; }
      if (!password) { statusEl.textContent = 'Enter password first'; return; }
      body = { provider: provider, email: username, password: password };
    }

    statusEl.textContent = 'Connecting…';
    btn.disabled = true;

    fetch('/api/ev/chargers', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    })
      .then(function (r) {
        return r.json().then(function (j) { return { ok: r.ok, body: j }; });
      })
      .then(function (res) {
        if (!res.ok) {
          statusEl.style.color = 'var(--red-e)';
          statusEl.textContent = (res.body && res.body.error) || 'Failed to load chargers';
          return;
        }
        var chargers = Array.isArray(res.body) ? res.body : [];
        if (chargers.length === 0) {
          statusEl.textContent = 'No chargers found on this account';
          group.style.display = 'none';
          return;
        }
        sel.innerHTML = '';
        chargers.forEach(function (c) {
          var opt = document.createElement('option');
          opt.value = c.id;
          opt.textContent = c.id + (c.name ? '  —  ' + c.name : '');
          sel.appendChild(opt);
        });
        group.style.display = 'block';
        statusEl.style.color = 'var(--green-e)';
        statusEl.textContent = chargers.length + ' charger' + (chargers.length === 1 ? '' : 's') + ' found';
      })
      .catch(function (e) {
        statusEl.style.color = 'var(--red-e)';
        statusEl.textContent = 'Error: ' + e.message;
      })
      .finally(function () {
        btn.disabled = false;
      });
  };

  // --- Step 8: Review ---

  function renderReview() {
    var zone = document.getElementById('price-zone').value;
    var html = '';

    // Site
    html += '<div class="review-section"><h3>Site</h3><div class="review-item">';
    html += esc(document.getElementById('site-name').value) + ', ';
    html += document.getElementById('fuse-phases').value + '&times;' +
            document.getElementById('fuse-amps').value + 'A @ ' +
            document.getElementById('fuse-voltage').value + 'V, ' +
            esc(zone);
    html += '</div></div>';

    // Devices
    if (configuredDrivers.length > 0) {
      html += '<div class="review-section"><h3>Devices</h3>';
      configuredDrivers.forEach(function (d) {
        var desc = esc(d.name);
        var detail = driverDetail(d);
        if (detail) desc += ' — ' + esc(detail);
        html += '<div class="review-item">' + desc + '</div>';
      });
      html += '</div>';
    }

    // EV
    var evProvider = document.getElementById('ev-provider').value;
    if (evProvider) {
      html += '<div class="review-section"><h3>EV Charger</h3><div class="review-item">';
      html += esc(evProviderLabel(evProvider));
      if (evProviderTransport(evProvider) === 'modbus') {
        var mbHost = document.getElementById('ev-mb-host').value.trim();
        if (mbHost) html += ' (' + esc(mbHost) + ')';
      } else if (evProviderTransport(evProvider) === 'http-local') {
        var httpHost = document.getElementById('ev-http-host').value.trim();
        if (httpHost) html += ' (' + esc(httpHost) + ')';
      } else {
        var evSerial = document.getElementById('ev-serial').value;
        if (evSerial) html += ' (' + esc(evSerial) + ')';
      }
      html += '</div></div>';
    }

    // HA
    var haEnabled = document.getElementById('ha-enabled').checked;
    if (haEnabled) {
      var haBroker = document.getElementById('ha-broker').value;
      var haPort = document.getElementById('ha-port').value;
      html += '<div class="review-section"><h3>Home Assistant</h3><div class="review-item">';
      html += esc(haBroker) + ':' + esc(haPort);
      html += '</div></div>';
    }

    // Price
    var priceProv = document.getElementById('price-provider').value;
    if (priceProv) {
      html += '<div class="review-section"><h3>Price</h3><div class="review-item">';
      html += esc(priceProv) + ' / ' + esc(zone);
      html += '</div></div>';
    }

    document.getElementById('review-content').innerHTML = html;
    document.getElementById('save-error').style.display = 'none';
    document.getElementById('save-actions').style.display = 'flex';
    document.getElementById('save-success').style.display = 'none';
  }

  // --- Save config ---

  window.saveConfig = function () {
    // Empty drivers list is valid — e.g. an EV-only site that only
    // configured a cloud EV charger in step 7 and doesn't own local
    // hardware. The backend accepts this and runs with a no-op
    // control loop (no site meter to balance against).
    var btn = document.getElementById('save-btn');
    btn.disabled = true;
    btn.textContent = 'Saving...';

    var cfg = buildConfig();

    fetch('/api/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg)
    })
      .then(function (r) {
        if (!r.ok) return r.json().then(function (d) { throw new Error(d.error || 'Save failed'); });
        return r.json();
      })
      .then(function () {
        document.getElementById('save-actions').style.display = 'none';
        document.getElementById('save-success').style.display = 'block';
        setTimeout(function () { window.location.href = '/'; }, 3000);
      })
      .catch(function (err) {
        btn.disabled = false;
        btn.textContent = 'Start FTW';
        var errEl = document.getElementById('save-error');
        errEl.className = 'error-msg';
        errEl.textContent = err.message;
        errEl.style.display = 'block';
      });
  };

  function buildConfig() {
    var zone = document.getElementById('price-zone').value;

    var cfg = {
      site: {
        name: document.getElementById('site-name').value.trim() || 'My Home',
        control_interval_s: 5,
        grid_target_w: 0,
        grid_tolerance_w: 42,
        watchdog_timeout_s: 60,
        smoothing_alpha: 0.3,
        gain: 0.5,
        slew_rate_w: 500,
        min_dispatch_interval_s: 5
      },
      fuse: {
        max_amps: parseFloat(document.getElementById('fuse-amps').value) || 16,
        phases: parseInt(document.getElementById('fuse-phases').value, 10) || 3,
        voltage: parseFloat(document.getElementById('fuse-voltage').value) || 230
      },
      drivers: configuredDrivers.map(function (d) {
        var clean = {};
        for (var k in d) {
          if (d.hasOwnProperty(k) && k !== '_catalog') clean[k] = d[k];
        }
        return clean;
      }),
      api: { port: 8080 }
    };

    // Price
    var priceProv = document.getElementById('price-provider').value;
    if (priceProv) {
      cfg.price = {
        provider: priceProv,
        zone: zone
      };
      var cur = currencyForZone(zone);
      if (cur) cfg.price.currency = cur;
    }

    // EV Charger — shape the block to match the provider's transport
    // (see go/internal/config/config.go EVCharger). Cloud HTTP providers
    // (easee, zaptec) carry username/password/serial; local HTTP
    // (tesla-wc) carries http.base_url; local Modbus (ctek) carries a
    // modbus{host,port,unit_id} block and rejects auth.
    var evProvider = document.getElementById('ev-provider').value;
    if (evProvider) {
      var ev = { provider: evProvider };
      if (evProviderTransport(evProvider) === 'modbus') {
        var mbHost = document.getElementById('ev-mb-host').value.trim();
        var mbPort = parseInt(document.getElementById('ev-mb-port').value, 10);
        var mbUnit = parseInt(document.getElementById('ev-mb-unit').value, 10);
        ev.modbus = { host: mbHost };
        if (mbPort) ev.modbus.port = mbPort;
        if (!isNaN(mbUnit)) ev.modbus.unit_id = mbUnit;
      } else if (evProviderTransport(evProvider) === 'http-local') {
        var localBase = evLocalHTTPBase(document.getElementById('ev-http-host').value);
        if (localBase) ev.http = { base_url: localBase };
        var localSerial = document.getElementById('ev-serial').value.trim();
        if (localSerial) ev.serial = localSerial;
      } else {
        ev.username = document.getElementById('ev-username').value.trim();
        ev.password = document.getElementById('ev-password').value;
        ev.serial = document.getElementById('ev-serial').value.trim();
      }
      cfg.ev_charger = ev;
    }

    // Home Assistant
    var haEnabled = document.getElementById('ha-enabled').checked;
    if (haEnabled) {
      cfg.homeassistant = {
        enabled: true,
        broker: document.getElementById('ha-broker').value.trim(),
        port: parseInt(document.getElementById('ha-port').value, 10) || 1883,
        username: document.getElementById('ha-user').value.trim(),
        password: document.getElementById('ha-pass').value
      };
    }

    return cfg;
  }

  // --- Helpers ---

  function esc(s) {
    if (!s) return '';
    var d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
  }

  // --- Price zones ---
  //
  // The country and zone lists come from /api/prices/zones — the same
  // table the price fetchers use — so the wizard can't offer a zone the
  // providers don't know. A failed request leaves the Swedish options in
  // setup.html standing.
  var priceZones = [];

  function loadPriceZones() {
    fetch('/api/prices/zones')
      .then(function (r) { return r.json(); })
      .then(function (j) {
        if (!j || !Array.isArray(j.zones) || !j.zones.length) return;
        priceZones = j.zones;
        var countryEl = document.getElementById('price-country');
        countryEl.innerHTML = '';
        var seen = {};
        priceZones.forEach(function (z) {
          if (seen[z.country]) return;
          seen[z.country] = true;
          var o = document.createElement('option');
          o.value = z.country;
          o.textContent = z.country;
          if (z.country === 'Sweden') o.selected = true;
          countryEl.appendChild(o);
        });
        countryEl.addEventListener('change', fillZonesForCountry);
        fillZonesForCountry();
      })
      .catch(function () { /* the Swedish fallback list stays */ });
  }

  function fillZonesForCountry() {
    var country = document.getElementById('price-country').value;
    var zoneEl = document.getElementById('price-zone');
    zoneEl.innerHTML = '';
    priceZones.filter(function (z) { return z.country === country; })
      .forEach(function (z, i) {
        var sub = z.name && z.name.indexOf(' — ') > 0 ? z.name.split(' — ')[1] : '';
        var o = document.createElement('option');
        o.value = z.code;
        o.textContent = sub ? z.code + ' · ' + sub : z.code;
        if (i === 0 || z.code === 'SE3') o.selected = true;
        zoneEl.appendChild(o);
      });
  }

  // The currency the picked zone bills in, so a Belgian install doesn't
  // start out priced in Swedish öre.
  function currencyForZone(code) {
    for (var i = 0; i < priceZones.length; i++) {
      if (priceZones[i].code === code) return priceZones[i].currency;
    }
    return '';
  }

  // --- Init ---
  // Honor a `?step=N` deep-link (the dashboard links to /setup?step=3 from
  // its "no devices" prompt). Clamp into the valid 1..TOTAL_STEPS range so a
  // hand-typed or stale param can't land on a non-existent step. Default to
  // step 1 when absent or unparseable.
  //
  // NOTE: this wizard's Save REPLACES config — it does not yet pre-load and
  // merge the existing config, so deep-linking to a later step is navigation
  // only, not a safe additive "add a device" flow. The dashboard copy is kept
  // honest ("Run setup wizard") to match.
  function initialStep() {
    var raw = new URLSearchParams(window.location.search).get('step');
    if (raw == null) return 1;
    var n = parseInt(raw, 10);
    if (isNaN(n)) return 1;
    if (n < 1) return 1;
    if (n > TOTAL_STEPS) return TOTAL_STEPS;
    return n;
  }

  document.getElementById('drv-ip').addEventListener('input', updateHostHint);

  renderDots();
  loadPriceZones();
  goStep(initialStep());
})();
