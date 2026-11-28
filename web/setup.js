// setup.js — multi-step setup wizard for FTW
// Single-page state machine: show/hide step divs, collect config, POST to /api/config.

(function () {
  'use strict';

  var TOTAL_STEPS = 5;
  var currentStep = 1;
  // A new box has no /api/auth/status until it first starts, so a missing
  // one means this wizard claims it and asks for the house password. Run
  // again from Settings, the box keeps the password it has.
  var firstRun = false;
  fetch('/api/auth/status', {cache: 'no-store'}).then(function (r) {
    firstRun = r.status === 404;
    // The answer can arrive after the last step is already on screen.
    var step = document.getElementById('house-password-step');
    if (step && currentStep === TOTAL_STEPS) step.hidden = !asksHousePassword();
  }).catch(function () {});

  function asksHousePassword() {
    return firstRun && !window.FTWHomeAssistant;
  }

  // Collected state. Each device carries the driver block it saves, the
  // catalog entry it came from, and what its test found.
  var devices = [];          // [{driver, entry, tested: 'ok' | 'untested', words}]
  var driverCatalog = [];    // /api/drivers/catalog entries
  var catalogLoaded = false;
  var pick = null;           // {entry, scan} while one device is being added
  var scanChoice = null;     // a scanned device waiting for its driver

  function flow() {
    return window.FTWDeviceFlow;
  }

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
    if (currentStep === 2 && n > 2 && !validateConnection()) return;

    // Pre-step hooks
    if (n === 3) { loadCatalog(); renderDeviceList(); }
    if (n === 4) prepareIntegrations();
    if (n === 5) renderReview();

    currentStep = n;
    var steps = document.querySelectorAll('.step');
    for (var i = 0; i < steps.length; i++) {
      steps[i].classList.remove('visible');
    }
    document.getElementById('step-' + n).classList.add('visible');
    renderDots();
    window.scrollTo(0, 0);
  };

  // --- Electricity connection ---

  function connectionHelp() {
    var choice = document.getElementById('site-connection').value;
    var country = document.getElementById('price-country').value;
    var text;
    if (choice === 'unknown') {
      text = "Check your electricity network contract for the number of phases and main fuse rating. If it is not listed, ask your network operator. FTW has not verified your connection.";
    } else if (choice === 'custom') {
      text = 'Custom connection: ' + document.getElementById('fuse-phases').value +
        ' phases, ' + document.getElementById('fuse-voltage').value +
        ' V per phase. Check the values in Advanced.';
    } else if (country === 'Belgium' || country === 'Norway') {
      text = 'Different grid types exist here. These standard choices use 230 V phase to neutral (400/230 V for three-phase). If your supply is 3 × 230 V without neutral, check compatibility with your installer before continuing.';
    } else {
      text = country === 'Sweden'
        ? 'Three-phase is common in Swedish houses; some homes have single-phase. Check your electricity network contract if unsure.'
        : 'Choose the connection on your electricity network contract. The standard choices use 230 V per phase.';
    }
    document.getElementById('connection-help').textContent = text;
  }

  function chooseConnection() {
    var choice = document.getElementById('site-connection').value;
    if (choice === '1' || choice === '3') {
      document.getElementById('fuse-phases').value = choice;
      document.getElementById('fuse-voltage').value = '230';
    } else if (choice === 'custom') {
      document.getElementById('connection-advanced').open = true;
    }
    document.getElementById('connection-error').hidden = true;
    connectionHelp();
  }

  function customConnection() {
    // An explicit expert edit resolves "I don't know", but never rewrites
    // another expert field or guesses the fuse rating.
    var phases = document.getElementById('fuse-phases').value;
    var voltage = Number(document.getElementById('fuse-voltage').value);
    document.getElementById('site-connection').value = voltage === 230 && (phases === '1' || phases === '3')
      ? phases : 'custom';
    connectionHelp();
  }

  function validateConnection() {
    var choice = document.getElementById('site-connection');
    var amps = document.getElementById('fuse-amps');
    var voltage = document.getElementById('fuse-voltage');
    var phases = document.getElementById('fuse-phases');
    var error = document.getElementById('connection-error');
    var invalid = null;
    if (choice.value === 'unknown') {
      error.textContent = 'Choose your connection before continuing. Your network operator can confirm it.';
      invalid = choice;
    } else if (!amps.checkValidity()) {
      error.textContent = 'Enter your main fuse rating from your electricity network contract (1–100 A).';
      invalid = amps;
    } else if (!voltage.checkValidity() || !['1', '2', '3'].includes(phases.value)) {
      error.textContent = 'Check the number of phases and phase-to-neutral voltage in Advanced.';
      document.getElementById('connection-advanced').open = true;
      invalid = voltage;
    }
    error.hidden = !invalid;
    if (invalid) {
      // A deep link to Review must not bypass the required installation values.
      if (currentStep !== 2) goStep(2);
      invalid.focus();
      return false;
    }
    return true;
  }


  // --- Step 3: Devices ---
  //
  // The flow Settings → Devices runs, from device-flow.js: find the driver,
  // give the address, and test the device when it is added. A device that
  // does not answer stays out until the owner fixes it or adds it anyway.

  function loadCatalog() {
    if (catalogLoaded) { renderCatalog(); return; }
    fetch('/api/drivers/catalog')
      .then(function (r) { return r.json(); })
      .then(function (data) {
        driverCatalog = (data && data.entries) || [];
        catalogLoaded = true;
        renderCatalog();
      })
      .catch(function () {
        document.getElementById('catalog-results').textContent =
          'The driver list did not load. Reload the page to try again.';
      });
  }

  function renderCatalog() {
    if (!catalogLoaded) return;
    var host = document.getElementById('catalog-results');
    var query = document.getElementById('catalog-search').value;
    var matches = flow().search(driverCatalog, query);
    host.innerHTML = '';
    if (matches.length === 0) {
      host.textContent = query.trim()
        ? 'No driver matches “' + query.trim() + '”. Try the maker, or the model on the label.'
        : 'No drivers on this box.';
      return;
    }
    matches.forEach(function (entry) {
      host.appendChild(flow().card(entry, {
        onChoose: function () { chooseDriver(entry, scanChoice); }
      }));
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

  // One calm line under the address: a .local name survives a new address
  // from the router, a raw IP only if the router keeps it.
  function updateHostHint() {
    var el = document.getElementById('dev-host-hint');
    var input = document.getElementById('dev-host');
    if (!el || !input) return;
    var host = input.value.trim();
    el.hidden = true;
    if (isMDNSName(host)) {
      el.textContent = 'FTW finds this device by name, even if its address changes.';
      el.hidden = false;
    } else if (isIPv4Literal(host)) {
      el.textContent = 'Tip: give this device a fixed address in your router.';
      el.hidden = false;
    }
  }

  // Names stay unique: the maker or driver id, then _2, _3.
  function uniqueName(entry) {
    var base = String(entry.manufacturer || entry.id || 'device').toLowerCase()
      .replace(/[^a-z0-9]/g, '_').replace(/_+/g, '_').replace(/^_|_$/g, '') || 'device';
    var name = base;
    for (var n = 2; devices.some(function (d) { return d.driver.name === name; }); n++) {
      name = base + '_' + n;
    }
    return name;
  }

  function hasSiteMeter() {
    return devices.some(function (d) { return d.driver.is_site_meter; });
  }

  // Core balances the home against one meter at the grid connection, and
  // refuses devices without one. A charger or a solar-only inverter cannot
  // be it, so the guide says so here rather than failing at Start FTW.
  var SITE_METER_NEEDED = 'FTW needs a meter for the whole home: add your electricity meter, a P1 reader, or an inverter that measures the grid.';

  function needsSiteMeter() {
    return devices.length > 0 && !hasSiteMeter();
  }

  function canMeasureHome(d) {
    return (d.entry.capabilities || []).indexOf('meter') >= 0;
  }

  function has(obj, key) {
    return !!obj && Object.prototype.hasOwnProperty.call(obj, key);
  }

  // A labelled input, built from nodes: labels and keys come from driver files.
  function field(parent, id, labelText, input) {
    var group = document.createElement('div');
    group.className = 'form-group';
    var label = document.createElement('label');
    label.htmlFor = id;
    label.textContent = labelText;
    input.id = id;
    group.appendChild(label);
    group.appendChild(input);
    parent.appendChild(group);
    return group;
  }

  function input(type, value, placeholder) {
    var el = document.createElement('input');
    el.type = type;
    el.value = value == null ? '' : String(value);
    if (placeholder) el.placeholder = placeholder;
    el.setAttribute('autocomplete', 'off');
    return el;
  }

  function chooseDriver(entry, scan) {
    pick = { entry: entry, scan: scan || null };
    var driver = flow().newDriver(entry, { name: uniqueName(entry) });
    var kind = flow().addressKind(driver);
    var defaults = entry.connection_defaults || {};

    document.getElementById('device-find').hidden = true;
    document.getElementById('device-form').hidden = false;
    document.getElementById('device-title').textContent = flow().title(entry);
    var hint = flow().setupHint(entry);
    var hintEl = document.getElementById('device-hint');
    hintEl.textContent = hint;
    hintEl.hidden = !hint;
    var readOnly = flow().readOnlySentence(entry);
    var readOnlyEl = document.getElementById('device-readonly');
    readOnlyEl.textContent = readOnly;
    readOnlyEl.hidden = !readOnly;
    resetTest();

    var fields = document.getElementById('device-fields');
    fields.innerHTML = '';
    var add = document.getElementById('device-add');
    add.hidden = kind === '';
    add.textContent = 'Add and test';
    if (kind === '') {
      var later = document.createElement('p');
      later.className = 'connection-hint';
      later.textContent = 'This device needs a sign-in the guide does not have. Add it in Settings → Devices after setup.';
      fields.appendChild(later);
      return;
    }

    if (entry.id === 'zap' || entry.filename === 'zap.lua') {
      var zap = document.createElement('p');
      zap.className = 'connection-hint';
      zap.textContent = 'Zap is the P1/HAN site meter by default. Add inverters, batteries and chargers as their own devices. If Zap is the only reader, turn on PV or battery reads later in Settings → Devices.';
      fields.appendChild(zap);
    }

    if (kind === 'modbus' || kind === 'mqtt' || kind === 'host') {
      var host = (driver.config && driver.config.host) || defaults.host || '';
      if (scan) host = isMDNSName(scan.hostname) ? scan.hostname : scan.ip;
      var hostInput = input('text', host, '192.168.1.10 or inverter.local');
      hostInput.addEventListener('input', updateHostHint);
      var group = field(fields, 'dev-host', 'Address', hostInput);
      var hostHint = document.createElement('p');
      hostHint.className = 'connection-hint host-hint';
      hostHint.id = 'dev-host-hint';
      hostHint.hidden = true;
      group.appendChild(hostHint);
      updateHostHint();
    }
    if (kind === 'account') {
      field(fields, 'dev-email', 'Email', input('text', '', 'you@example.com'));
      field(fields, 'dev-password', 'Password', input('password', ''));
      var find = document.createElement('button');
      find.type = 'button';
      find.className = 'btn-secondary';
      find.id = 'dev-find-chargers';
      find.textContent = 'Find my chargers';
      find.addEventListener('click', findChargers);
      fields.appendChild(find);
      var serial = document.createElement('select');
      var serialGroup = field(fields, 'dev-serial', 'Charger', serial);
      serialGroup.id = 'dev-serial-group';
      serialGroup.hidden = true;
    }
    if (has(driver.config, 'username')) field(fields, 'dev-username', 'Username', input('text', ''));
    if (kind === 'host' && has(driver.config, 'password')) field(fields, 'dev-password', 'Password', input('password', ''));
    flow().secretKeys(entry, driver).forEach(function (key) {
      var label = key.replace(/_/g, ' ').replace(/\b\w/g, function (c) { return c.toUpperCase(); });
      field(fields, 'dev-secret-' + key, label, input('password', '', 'From the device or its app'));
    });
    var profiles = flow().configProfiles(entry);
    if (profiles.length > 0) {
      var select = document.createElement('select');
      profiles.forEach(function (profile) {
        var opt = document.createElement('option');
        opt.value = profile.value;
        opt.textContent = profile.label;
        select.appendChild(opt);
      });
      field(fields, 'dev-profile', 'Register map', select);
      // Each map answers on its own Unit ID.
      select.addEventListener('change', function () {
        var unitEl = document.getElementById('dev-unit');
        var chosen = flow().profileByValue(profiles, select.value);
        if (unitEl && chosen) unitEl.value = String(chosen.unitId);
      });
    }
    // Battery size is the opt-in to control; a driver that only reads gets none.
    var caps = entry.capabilities || [];
    if (caps.indexOf('battery') >= 0 && !entry.read_only) {
      var battery = input('number', '', 'e.g. 10');
      battery.min = '0';
      battery.step = '0.1';
      var bgroup = field(fields, 'dev-battery', 'Battery size (kWh)', battery);
      var bhint = document.createElement('p');
      bhint.className = 'connection-hint';
      bhint.textContent = 'On the battery label. Without it, FTW only reads the battery.';
      bgroup.appendChild(bhint);
    }
    if (caps.indexOf('meter') >= 0) {
      var check = document.createElement('div');
      check.className = 'form-check';
      var box = document.createElement('input');
      box.type = 'checkbox';
      box.id = 'dev-site-meter';
      box.checked = !hasSiteMeter();
      var boxLabel = document.createElement('label');
      boxLabel.htmlFor = 'dev-site-meter';
      boxLabel.textContent = 'This meter measures the whole home at the grid connection';
      check.appendChild(box);
      check.appendChild(boxLabel);
      fields.appendChild(check);
    }

    // What an expert may change: the name, and the port and Unit ID the
    // scan or the driver already knows.
    var more = document.createElement('details');
    more.className = 'device-advanced';
    var summary = document.createElement('summary');
    summary.textContent = 'Advanced';
    more.appendChild(summary);
    field(more, 'dev-name', 'Name', input('text', driver.name));
    if (kind === 'modbus' || kind === 'mqtt') {
      var port = (scan && scan.port) || defaults.port || (kind === 'mqtt' ? 1883 : 502);
      field(more, 'dev-port', 'Port', input('number', port));
    }
    if (kind === 'modbus') {
      var unit = profiles.length === 0 && defaults.unit_id != null ? defaults.unit_id : driver.capabilities.modbus.unit_id;
      field(more, 'dev-unit', 'Unit ID', input('number', unit));
    }
    fields.appendChild(more);
  }

  function value(id) {
    var el = document.getElementById(id);
    return el ? String(el.value || '').trim() : '';
  }

  // The driver block from the form, or {error} naming what is missing.
  function readDevice() {
    var entry = pick.entry;
    var profile = document.getElementById('dev-profile');
    var driver = flow().newDriver(entry, {
      name: value('dev-name') || uniqueName(entry),
      profile: profile ? profile.value : undefined
    });
    if (devices.some(function (d) { return d.driver.name === driver.name; })) {
      return { error: 'Another device already has the name ' + driver.name + '.' };
    }
    var kind = flow().addressKind(driver);
    if (kind === 'modbus' || kind === 'mqtt' || kind === 'host') {
      var host = value('dev-host');
      if (!host) return { error: 'Enter the device address.' };
      flow().setAddress(driver, entry, { host: host, port: value('dev-port'), unitId: value('dev-unit') });
    }
    driver.config = driver.config || {};
    if (kind === 'account') {
      driver.config.email = value('dev-email');
      driver.config.password = document.getElementById('dev-password').value;
      driver.config.serial = value('dev-serial');
      if (!driver.config.email || !driver.config.password) return { error: 'Enter the email and password of the account.' };
      if (!driver.config.serial) return { error: 'Find your chargers and choose one.' };
    }
    if (has(driver.config, 'username')) driver.config.username = value('dev-username');
    if (kind === 'host' && has(driver.config, 'password')) driver.config.password = document.getElementById('dev-password').value;
    flow().secretKeys(entry, driver).forEach(function (key) {
      var v = value('dev-secret-' + key);
      if (v) driver.config[key] = v;
    });
    if (Object.keys(driver.config).length === 0) delete driver.config;
    var battery = parseFloat(value('dev-battery'));
    if (battery > 0) driver.battery_capacity_wh = battery * 1000;
    var meter = document.getElementById('dev-site-meter');
    driver.is_site_meter = !!(meter && meter.checked);
    return { driver: driver };
  }

  function resetTest() {
    var el = document.getElementById('device-test');
    el.hidden = true;
    el.className = 'device-test';
    el.textContent = '';
    document.getElementById('device-add-anyway').hidden = true;
  }

  function showTestFailure(res) {
    var el = document.getElementById('device-test');
    el.hidden = false;
    el.className = 'device-test failed';
    el.textContent = 'FTW could not read this device. Check the address. If the device is off now, add it anyway.';
    // The driver's hint moves here, beside the error, rather than twice.
    var hint = flow().setupHint(pick.entry);
    document.getElementById('device-hint').hidden = true;
    if (hint) {
      var h = document.createElement('span');
      h.className = 'device-test-hint';
      h.textContent = hint;
      el.appendChild(h);
    }
    if (res.error) {
      var detail = document.createElement('span');
      detail.className = 'device-error';
      detail.textContent = res.error;
      el.appendChild(detail);
    }
    document.getElementById('device-add').textContent = 'Test again';
    document.getElementById('device-add-anyway').hidden = false;
  }

  window.addDevice = function () {
    var read = readDevice();
    var el = document.getElementById('device-test');
    if (read.error) {
      el.hidden = false;
      el.className = 'device-test failed';
      el.textContent = read.error;
      return;
    }
    var add = document.getElementById('device-add');
    add.disabled = true;
    document.getElementById('device-add-anyway').hidden = true;
    el.hidden = false;
    el.className = 'device-test';
    el.innerHTML = '<span class="spinner"></span> Testing the connection…';
    flow().test(read.driver).then(function (res) {
      add.disabled = false;
      if (res && res.ok) {
        keepDevice(read.driver, 'ok', flow().readingWords(res));
        return;
      }
      showTestFailure(res || {});
    });
  };

  // For a device that is off right now. It is saved, and marked untested.
  window.addDeviceAnyway = function () {
    var read = readDevice();
    if (read.error) return;
    keepDevice(read.driver, 'untested', '');
  };

  function keepDevice(driver, tested, words) {
    if (driver.is_site_meter) {
      devices.forEach(function (d) { d.driver.is_site_meter = false; });
    }
    devices.push({ driver: driver, entry: pick.entry, tested: tested, words: words });
    window.closeDeviceForm();
    renderDeviceList();
  }

  window.closeDeviceForm = function () {
    pick = null;
    scanChoice = null;
    document.getElementById('device-form').hidden = true;
    document.getElementById('device-find').hidden = false;
    document.getElementById('catalog-for').hidden = true;
    document.getElementById('catalog-search').value = '';
    renderCatalog();
  };

  function deviceDetail(d) {
    var parts = [flow().addressOf(d.driver)];
    if (d.driver.is_site_meter) parts.push('site meter');
    if (d.driver.battery_capacity_wh) parts.push((d.driver.battery_capacity_wh / 1000).toFixed(1) + ' kWh battery');
    return parts.filter(Boolean).join(' · ');
  }

  function deviceState(d) {
    return d.tested === 'ok'
      ? 'Connected' + (d.words ? ' · ' + d.words : '')
      : 'Not tested: it did not answer';
  }

  function renderDeviceList() {
    var container = document.getElementById('device-list');
    container.innerHTML = '';
    devices.forEach(function (d, idx) {
      var item = document.createElement('div');
      item.className = 'driver-summary-item';
      var info = document.createElement('div');
      info.className = 'driver-info';
      var label = document.createElement('span');
      label.className = 'driver-label';
      label.textContent = flow().title(d.entry) + ' · ' + d.driver.name;
      var detail = document.createElement('span');
      detail.className = 'driver-detail';
      detail.textContent = deviceDetail(d);
      var state = document.createElement('span');
      state.className = 'device-state' + (d.tested === 'ok' ? ' ok' : '');
      state.textContent = deviceState(d);
      info.appendChild(label);
      info.appendChild(detail);
      info.appendChild(state);
      // With no site meter, a device that can be one offers to be it.
      if (needsSiteMeter() && canMeasureHome(d)) {
        var use = document.createElement('button');
        use.className = 'btn-secondary device-use-meter';
        use.textContent = 'Measures the whole home';
        use.addEventListener('click', function () {
          d.driver.is_site_meter = true;
          renderDeviceList();
        });
        info.appendChild(use);
      }
      var btn = document.createElement('button');
      btn.className = 'btn-remove-sm';
      btn.textContent = 'Remove';
      btn.addEventListener('click', function () {
        devices.splice(idx, 1);
        renderDeviceList();
      });
      item.appendChild(info);
      item.appendChild(btn);
      container.appendChild(item);
    });
    var line = document.getElementById('site-meter-needed');
    line.textContent = SITE_METER_NEEDED;
    line.hidden = !needsSiteMeter();
    document.getElementById('devices-next').textContent = devices.length ? 'Continue' : 'Continue without devices';
  }

  // Cloud chargers: the account lists its chargers, the owner picks one.
  function findChargers() {
    var btn = document.getElementById('dev-find-chargers');
    var el = document.getElementById('device-test');
    var email = value('dev-email');
    var password = document.getElementById('dev-password').value;
    resetTest();
    if (!email || !password) {
      el.hidden = false;
      el.className = 'device-test failed';
      el.textContent = 'Enter the email and password of the account.';
      return;
    }
    var provider = String(pick.entry.filename || pick.entry.id || '')
      .replace(/\.lua$/i, '').replace(/_cloud$/i, '');
    btn.disabled = true;
    el.hidden = false;
    el.innerHTML = '<span class="spinner"></span> Signing in…';
    fetch('/api/ev/chargers', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ provider: provider, email: email, password: password })
    }).then(function (r) {
      return r.json().then(function (j) { return { ok: r.ok, body: j }; });
    }).then(function (res) {
      var chargers = res.ok && Array.isArray(res.body) ? res.body : [];
      if (!res.ok || chargers.length === 0) {
        el.className = 'device-test failed';
        el.textContent = res.ok ? 'This account has no chargers.' : ((res.body && res.body.error) || 'Sign-in failed.');
        return;
      }
      var sel = document.getElementById('dev-serial');
      sel.innerHTML = '';
      chargers.forEach(function (c) {
        var opt = document.createElement('option');
        opt.value = c.id;
        opt.textContent = c.name ? c.name + ' · ' + c.id : c.id;
        sel.appendChild(opt);
      });
      document.getElementById('dev-serial-group').hidden = false;
      resetTest();
    }).catch(function (err) {
      el.className = 'device-test failed';
      el.textContent = err.message;
    }).finally(function () {
      btn.disabled = false;
    });
  }

  // The scan finds addresses that answer on a known port and, when it can,
  // the device behind them.
  window.startScan = function () {
    var statusEl = document.getElementById('scan-status');
    var resultsEl = document.getElementById('scan-results');
    var btn = document.getElementById('scan-btn');
    statusEl.innerHTML = '<span class="spinner"></span> Scanning your network…';
    resultsEl.hidden = true;
    btn.disabled = true;
    fetch('/api/scan?fingerprint=1')
      .then(function (r) { return r.json(); })
      .then(function (data) {
        var found = Array.isArray(data) ? data : (data.devices || []);
        if (found.length === 0) {
          statusEl.textContent = 'Nothing found. Find your device below and type its address.';
          return;
        }
        statusEl.textContent = found.length === 1 ? '1 device found.' : found.length + ' devices found.';
        found.sort(function (a, b) {
          return Number(Boolean(b.matches && b.matches.length)) - Number(Boolean(a.matches && a.matches.length));
        });
        resultsEl.innerHTML = '';
        found.forEach(function (d) {
          var row = document.createElement('div');
          row.className = 'scan-found';
          var match = d.matches && d.matches.length ? d.matches[0] : null;
          var text = document.createElement('span');
          var name = match ? (match.name || match.make || match.driver) : 'Unknown device';
          text.textContent = name + ' · ' + (d.hostname || d.ip) + ':' + d.port;
          var use = document.createElement('button');
          use.className = 'btn-secondary';
          use.textContent = 'Use';
          use.addEventListener('click', function () { useScanned(d, match); });
          row.appendChild(text);
          row.appendChild(use);
          resultsEl.appendChild(row);
        });
        resultsEl.hidden = false;
      })
      .catch(function (err) {
        statusEl.textContent = 'The scan failed: ' + err.message + '. Find your device below and type its address.';
      })
      .finally(function () { btn.disabled = false; });
  };

  function useScanned(dev, match) {
    var entry = null;
    if (match && match.driver) {
      for (var i = 0; i < driverCatalog.length; i++) {
        if (driverCatalog[i].filename === match.driver) { entry = driverCatalog[i]; break; }
      }
    }
    if (entry) { chooseDriver(entry, dev); return; }
    scanChoice = dev;
    var forEl = document.getElementById('catalog-for');
    forEl.textContent = 'Choose the driver for ' + (dev.hostname || dev.ip) + ' (port ' + dev.port + ').';
    forEl.hidden = false;
  }

  // --- Step 4: Prices and Home Assistant ---

  var integrationListenersBound = false;

  function prepareIntegrations() {
    var zone = document.getElementById('price-zone').value;
    document.getElementById('price-zone-readonly').value = zone;

    if (!integrationListenersBound) {
      integrationListenersBound = true;
      document.getElementById('ha-enabled').addEventListener('change', function () {
        document.getElementById('ha-fields').style.display = this.checked ? 'block' : 'none';
      });
    }
  }

  // --- Step 5: Review ---

  // The zone's own name, "SE3 · Stockholm", as the step 2 list shows it.
  function zoneLabel(code) {
    var sel = document.getElementById('price-zone');
    for (var i = 0; i < sel.options.length; i++) {
      if (sel.options[i].value === code) return sel.options[i].textContent;
    }
    return code;
  }

  function renderReview() {
    var zone = document.getElementById('price-zone').value;
    var html = '';

    // Site
    html += '<div class="review-section"><h3>Home</h3><div class="review-item">';
    html += esc(document.getElementById('site-name').value) + ', ';
    var connection = document.getElementById('site-connection').value;
    var connectionLabel = connection === '3' ? 'Three-phase' : connection === '1' ? 'Single-phase' :
      connection === 'unknown' ? 'Connection not confirmed' : 'Custom connection';
    html += esc(connectionLabel) + ' · main fuse ' +
      esc(document.getElementById('fuse-amps').value || 'not confirmed') + ' A';
    if (connection === 'custom') {
      html += ' · ' + esc(document.getElementById('fuse-phases').value) + ' phases · ' +
        esc(document.getElementById('fuse-voltage').value) + ' V per phase';
    }
    html += ' · ' + (zone ? 'price area ' + esc(zoneLabel(zone)) : 'no price area');
    var clock = browserTimeZone();
    if (clock) html += ' · clock ' + esc(clock);
    html += '</div></div>';
    // Said here, not found out later: without an area FTW has no prices.
    if (!zone) {
      html += '<div class="notice" id="no-price-area">No price area. FTW will not fetch prices or plan by price until you choose one in Settings → Price.</div>';
    }

    // Devices
    if (devices.length > 0) {
      html += '<div class="review-section"><h3>Devices</h3>';
      devices.forEach(function (d) {
        var desc = esc(flow().title(d.entry)) + ' — ' + esc(deviceDetail(d));
        html += '<div class="review-item">' + desc +
          '<span class="device-state' + (d.tested === 'ok' ? ' ok' : '') + '" style="display:block">' +
          esc(deviceState(d)) + '</span></div>';
      });
      html += '</div>';
    } else {
      html += '<div class="notice">No devices. FTW shows nothing live until you add one in Settings → Devices.</div>';
    }
    if (needsSiteMeter()) {
      html += '<div class="notice" id="review-site-meter">' + esc(SITE_METER_NEEDED) + '</div>';
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
    var priceProv = zone ? document.getElementById('price-provider').value : '';
    if (priceProv) {
      html += '<div class="review-section"><h3>Price</h3><div class="review-item">';
      html += esc(priceProv) + ' / ' + esc(zone);
      html += '</div></div>';
    }

    document.getElementById('review-content').innerHTML = html;
    document.getElementById('house-password-step').hidden = !asksHousePassword();
    document.getElementById('save-btn').disabled = needsSiteMeter();
    document.getElementById('save-error').style.display = 'none';
    document.getElementById('save-actions').style.display = 'flex';
    document.getElementById('save-success').style.display = 'none';
  }

  // --- Save config ---

  window.saveConfig = function () {
    if (!validateConnection()) return;
    if (needsSiteMeter()) return;
    // An empty device list is valid: the review says FTW shows nothing
    // live, and the owner adds devices later in Settings.
    var btn = document.getElementById('save-btn');
    var errEl = document.getElementById('save-error');
    var cfg = buildConfig();
    var asked = asksHousePassword();
    if (asked) {
      var password = document.getElementById('house-password').value;
      if (!password || password !== document.getElementById('house-password-confirm').value) {
        errEl.className = 'error-msg';
        errEl.textContent = 'Choose a house password, and type the same one twice.';
        errEl.style.display = 'block';
        document.getElementById('house-password').focus();
        return;
      }
      cfg.house_password = password;
    }
    btn.disabled = true;
    btn.textContent = 'Saving...';

    fetch('/api/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg)
    })
      .then(function (r) {
        // Core's own reason, said where the owner is looking.
        if (!r.ok) return r.json().then(function (d) { throw new Error('FTW could not save: ' + (d.error || 'HTTP ' + r.status)); });
        return r.json();
      })
      .then(function () {
        if (asked) {
          document.getElementById('house-password').value = '';
          document.getElementById('house-password-confirm').value = '';
          document.getElementById('save-success-password').hidden = false;
        }
        document.getElementById('house-password-step').hidden = true;
        document.getElementById('save-actions').style.display = 'none';
        document.getElementById('save-success').style.display = 'block';
        setTimeout(function () { window.location.href = '/'; }, 3000);
      })
      .catch(function (err) {
        btn.disabled = false;
        btn.textContent = 'Start FTW';
        errEl.className = 'error-msg';
        errEl.textContent = err.message;
        errEl.style.display = 'block';
      });
  };

  // The household clock car goals and daily history read. A Docker box runs
  // on UTC, so the zone comes from the browser that sets the home up. A
  // browser on UTC says nothing about the home; the box then keeps its own.
  function browserTimeZone() {
    try {
      var tz = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
      return /^(Etc\/)?(UTC|UCT|GMT|Universal|Zulu)$/.test(tz) ? '' : tz;
    } catch (e) {
      return '';
    }
  }

  function buildConfig() {
    var zone = document.getElementById('price-zone').value;

    var cfg = {
      site: {
        name: document.getElementById('site-name').value.trim() || 'My Home',
        time_zone: browserTimeZone() || undefined,
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
        max_amps: Number(document.getElementById('fuse-amps').value),
        phases: Number(document.getElementById('fuse-phases').value),
        voltage: Number(document.getElementById('fuse-voltage').value)
      },
      drivers: devices.map(function (d) { return d.driver; }),
      api: { port: 8080 },
      // A new box plans from the start. It does not sell from the battery
      // until the owner allows it, so Plan does not ask on the first day.
      planner: { enabled: true, battery_export: 'not_allowed' }
    };

    // Price. With no area there is no provider: Core would otherwise price
    // the home by SE3 without saying so.
    var priceProv = zone ? document.getElementById('price-provider').value : '';
    if (priceProv) {
      cfg.price = {
        provider: priceProv,
        zone: zone
      };
      var cur = currencyForZone(zone);
      if (cur) cfg.price.currency = cur;
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

  // A country with one area needs no question. With more, nothing is
  // chosen for the owner: a wrong area gives another area's prices.
  function fillZonesForCountry() {
    var country = document.getElementById('price-country').value;
    var zoneEl = document.getElementById('price-zone');
    var inCountry = priceZones.filter(function (z) { return z.country === country; });
    zoneEl.innerHTML = '';
    if (inCountry.length > 1) {
      var none = document.createElement('option');
      none.value = '';
      none.textContent = 'Choose your price area';
      none.selected = true;
      zoneEl.appendChild(none);
    }
    inCountry.forEach(function (z) {
      var sub = z.name && z.name.indexOf(' — ') > 0 ? z.name.split(' — ')[1] : '';
      var o = document.createElement('option');
      o.value = z.code;
      o.textContent = sub ? z.code + ' · ' + sub : z.code;
      if (inCountry.length === 1) o.selected = true;
      zoneEl.appendChild(o);
    });
    zoneEl.value = inCountry.length === 1 ? inCountry[0].code : '';
    document.getElementById('price-zone-group').hidden = inCountry.length === 1;
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
  // Honor a `?step=N` deep-link (the dashboard and Settings link to
  // /setup?step=3, the devices step). Clamp into the valid 1..TOTAL_STEPS range so a
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

  // A short house password is the owner's choice: a word, not a refusal.
  (function () {
    var input = document.getElementById('house-password');
    var hint = document.getElementById('house-password-hint');
    input.addEventListener('input', function () {
      var n = Array.from(input.value).length;
      hint.textContent = n > 0 && n < 8 ? 'Short passwords are easy to guess. You can still use this one.' : '';
    });
  })();

  document.getElementById('catalog-search').addEventListener('input', renderCatalog);
  document.getElementById('site-connection').addEventListener('change', chooseConnection);
  document.getElementById('price-country').addEventListener('change', connectionHelp);
  document.getElementById('fuse-phases').addEventListener('change', customConnection);
  document.getElementById('fuse-voltage').addEventListener('input', customConnection);
  connectionHelp();

  renderDots();
  loadPriceZones();
  goStep(initialStep());
})();
