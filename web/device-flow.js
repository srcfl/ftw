// One way to add a device, for Settings → Devices and the setup guide.
//
// Find the driver in the catalog, give the device its address, and test it
// before it joins the settings. Both places used to keep their own copy of
// these steps, and the guide's copy never tested anything: a wrong port
// gave a green LIVE with nothing in it.
(function () {
  'use strict';

  // ---- Catalog ----

  // Everything an owner might type: the maker, the driver's name, and the
  // models it has run against. You know you own an SH10RT, not that you need
  // "Sungrow SH Hybrid Inverter".
  function haystack(e) {
    return [e.name, e.manufacturer, e.id, e.filename]
      .concat(e.tested_models || [])
      .concat(e.protocols || [])
      .join(' ').toLowerCase();
  }

  // The ones most homes run first: that is what a count is for.
  function rank(e) {
    return typeof e.run_by === 'number' ? -e.run_by : 1;
  }

  // Every term has to match somewhere, so "sungrow hybrid" narrows.
  function search(entries, query) {
    var terms = String(query || '').trim().toLowerCase().split(/\s+/).filter(Boolean);
    var matches = (entries || []).filter(function (e) {
      if (terms.length === 0) return true;
      var hay = haystack(e);
      return terms.every(function (t) { return hay.indexOf(t) >= 0; });
    });
    return matches.sort(function (a, b) {
      var byRank = rank(a) - rank(b);
      if (byRank !== 0) return byRank;
      return String(a.name || '').localeCompare(String(b.name || ''));
    });
  }

  // Several drivers already begin with the maker, which read as
  // "CTEK CTEK Chargestorm".
  function title(e) {
    var name = e.name || e.filename || e.id || '';
    var maker = e.manufacturer || '';
    if (!maker || name.toLowerCase().indexOf(maker.toLowerCase()) === 0) return name;
    return maker + ' ' + name;
  }

  // How many boxes ran exactly this version the last closed day.
  function usageLabel(count) {
    if (typeof count !== 'number') return '';
    return count === 1 ? '1 home runs this version' : count + ' homes run this version';
  }

  // Whether to say "reads only" on a driver. driver-control.js has the
  // sentence for the kinds FTW would otherwise steer, and skips any driver
  // that reads solar, so a read-only hybrid such as Huawei never got it. A
  // read-only driver with a battery or solar says so too. A meter is only
  // ever read, so it says nothing, and a driver whose DRIVER block does not
  // state read_only is unknown: nothing is guessed.
  function readsOnly(e) {
    if (!e || e.read_only !== true) return false;
    var words = window.FTWDriverControl;
    if (words && words.readOnlyNote(e)) return true;
    var caps = e.capabilities || [];
    return caps.indexOf('battery') >= 0 || caps.indexOf('pv') >= 0;
  }

  function readOnlySentence(e) {
    var words = window.FTWDriverControl;
    var note = words ? words.readOnlyNote(e) : null;
    if (note) return words.sentence(note);
    return readsOnly(e) ? 'FTW reads this device but cannot control it.' : '';
  }

  // The one sentence a driver gives on what to turn on at the device.
  function setupHint(e) {
    return e && typeof e.setup_hint === 'string' ? e.setup_hint.trim() : '';
  }

  // A card for one catalog entry. Built from nodes: every string here comes
  // from a driver file. opts: document, chosen, hideCapabilities, onChoose.
  function card(e, opts) {
    opts = opts || {};
    var doc = opts.document || document;
    var el = doc.createElement('button');
    el.type = 'button';
    el.className = 'drv-catalog-card' + (opts.chosen ? ' is-chosen' : '');

    var head = doc.createElement('div');
    head.className = 'drv-catalog-title';
    head.textContent = title(e);
    el.appendChild(head);

    var tags = doc.createElement('div');
    tags.className = 'drv-catalog-tags';
    function tag(text, extra, hover) {
      var t = doc.createElement('span');
      t.className = 'drv-catalog-tag' + (extra ? ' ' + extra : '');
      t.textContent = text;
      if (hover) t.title = hover;
      tags.appendChild(t);
    }
    if (!opts.hideCapabilities) (e.capabilities || []).forEach(function (cap) { tag(cap); });
    if (e.channel === 'beta') tag('beta', 'drv-catalog-unproven');
    else if (e.channel === 'stable') tag('from the driver channel');
    var usage = usageLabel(e.run_by);
    if (usage) tag(usage);
    // Said before it is added, not found out after.
    if (readsOnly(e)) tag('reads only', 'drv-catalog-unproven', readOnlySentence(e));
    el.appendChild(tags);

    // The models this has run against, to compare with the label on the unit.
    var models = (e.tested_models || []).slice(0, 3).join(', ');
    var foot = doc.createElement('div');
    foot.className = 'drv-catalog-foot';
    foot.textContent = [(e.protocols || []).join(' + '), e.version ? 'v' + e.version : '', models]
      .filter(Boolean).join(' · ');
    el.appendChild(foot);

    var hint = setupHint(e);
    if (hint) {
      var h = doc.createElement('div');
      h.className = 'drv-catalog-setup';
      h.textContent = hint;
      el.appendChild(h);
    }

    if (opts.onChoose) el.addEventListener('click', function () { opts.onChoose(e); });
    return el;
  }

  // ---- A new device ----

  // Some drivers expose more than one fixed register map. Keep these choices
  // here until the signed driver catalog grows a general config-schema
  // field. Version gating keeps an older bundled GoodWe driver from being
  // offered a profile it cannot load.
  var CONFIG_PROFILES = {
    goodwe: [
      { value: 'community-v1', label: 'Community v1', unitId: 1 },
      { value: 'gw8kn-et-hk3000', label: 'GW8KN-ET + HK3000', unitId: 247 }
    ]
  };

  function versionAtLeast(version, minimum) {
    var got = String(version || '').match(/^(\d+)\.(\d+)\.(\d+)/);
    var want = String(minimum || '').match(/^(\d+)\.(\d+)\.(\d+)/);
    if (!got || !want) return false;
    for (var i = 1; i <= 3; i++) {
      var g = parseInt(got[i], 10);
      var w = parseInt(want[i], 10);
      if (g !== w) return g > w;
    }
    return true;
  }

  // Register maps a driver offers, each with the Unit ID it answers on.
  function configProfiles(entry) {
    if (!entry || entry.id !== 'goodwe' || !versionAtLeast(entry.version, '1.0.2')) return [];
    return CONFIG_PROFILES.goodwe;
  }

  function profileByValue(profiles, value) {
    for (var i = 0; i < profiles.length; i++) {
      if (profiles[i].value === value) return profiles[i];
    }
    return profiles[0] || null;
  }

  function has(obj, key) {
    return !!obj && Object.prototype.hasOwnProperty.call(obj, key);
  }

  // The driver block for a catalog entry, before the owner types anything.
  // opts: name, profile (a value from configProfiles).
  function newDriver(entry, opts) {
    opts = opts || {};
    var protocols = entry.protocols || [];
    var caps = entry.capabilities || [];
    var defaults = entry.connection_defaults || {};
    var driver = { name: opts.name || entry.id || 'device', lua: entry.path, capabilities: {} };
    if (protocols.indexOf('mqtt') >= 0) driver.capabilities.mqtt = { host: '', port: 1883 };
    if (protocols.indexOf('modbus') >= 0) driver.capabilities.modbus = { host: '', port: 502, unit_id: 1 };
    var profiles = configProfiles(entry);
    if (profiles.length > 0) {
      var profile = profileByValue(profiles, opts.profile);
      driver.config = { profile: profile.value };
      if (driver.capabilities.modbus) driver.capabilities.modbus.unit_id = profile.unitId;
    }
    if (entry.read_only && caps.indexOf('battery') >= 0) {
      // Admit the battery reading of a driver that only reads (Zap, a
      // Huawei hybrid) without adding it to the dispatch pool. Without this
      // and with no capacity, Core drops the battery emits.
      driver.battery_telemetry_only = true;
    }
    if (protocols.indexOf('http') >= 0) {
      var hosts = (entry.http_hosts || []).filter(Boolean);
      driver.capabilities.http = { allowed_hosts: hosts };
      if (caps.indexOf('vehicle') >= 0) {
        driver.config = { ip: '', vin: '' };
      } else if (defaults.host) {
        driver.config = { host: defaults.host };
      } else if (caps.indexOf('apicreds') >= 0 && defaults.port) {
        // A local API with HTTP Basic auth (NIBE). Core derives the allowed
        // host from config.host.
        driver.config = { host: '', username: '', password: '' };
      } else if (caps.indexOf('apicreds') >= 0) {
        // OAuth2 client credentials (MyUplink).
        driver.config = { client_id: '', client_secret: '' };
      } else if (caps.indexOf('meter') >= 0 || caps.indexOf('pv') >= 0 || caps.indexOf('battery') >= 0 ||
                 (caps.indexOf('ev') >= 0 && hosts.length === 0)) {
        // A local device with no fixed name (ESPHome, a Wall Connector).
        // An empty host is a local-HTTP signal, not a cloud one.
        driver.config = { host: '' };
      } else {
        // An account at the maker's cloud (Easee, Zaptec).
        driver.config = { email: '', password: '', serial: '' };
      }
    }
    // Zap reads PV and battery only on opt-in. Save the off state, because
    // Core treats a missing read_pv as PV that may still report.
    if (entry.id === 'zap' || entry.filename === 'zap.lua') {
      driver.config = driver.config || {};
      driver.config.read_pv = false;
      driver.config.read_battery = false;
    }
    return driver;
  }

  // What the owner fills in for this driver: 'modbus' or 'mqtt' (an
  // address and a port), 'host' (an address in config.host), 'account'
  // (email, password and which charger), or '' when the device needs a step
  // only Settings has, such as a sign-in at the maker's site.
  function addressKind(driver) {
    var cap = driver.capabilities || {};
    var cfg = driver.config || {};
    if (cap.modbus) return 'modbus';
    if (cap.mqtt) return 'mqtt';
    if (has(cfg, 'client_id') || has(cfg, 'vin')) return '';
    if (has(cfg, 'host')) return 'host';
    if (has(cfg, 'email')) return 'account';
    return '';
  }

  // Put the address the owner gave into the driver. The port and Unit ID
  // come from the scan or the driver's own defaults unless the owner set
  // them. addr: host, port, unitId.
  function setAddress(driver, entry, addr) {
    var cap = driver.capabilities || {};
    var defaults = (entry && entry.connection_defaults) || {};
    var host = String(addr.host || '').trim();
    var port = parseInt(addr.port, 10) || parseInt(defaults.port, 10) || 0;
    if (cap.modbus) {
      cap.modbus.host = host;
      cap.modbus.port = port || 502;
      var unit = parseInt(addr.unitId, 10);
      if (isNaN(unit)) unit = parseInt(defaults.unit_id, 10);
      if (!isNaN(unit)) cap.modbus.unit_id = unit;
    } else if (cap.mqtt) {
      cap.mqtt.host = host;
      cap.mqtt.port = port || 1883;
    } else if (has(driver.config, 'host')) {
      driver.config.host = host;
    }
    return driver;
  }

  function addressOf(driver) {
    var cap = driver.capabilities || {};
    if (cap.modbus) return cap.modbus.host + ':' + cap.modbus.port;
    if (cap.mqtt) return cap.mqtt.host + ':' + cap.mqtt.port;
    var cfg = driver.config || {};
    return cfg.host || cfg.email || '';
  }

  // Secrets a driver asks for that no other field already holds.
  function secretKeys(entry, driver) {
    var cfg = (driver && driver.config) || {};
    return ((entry && entry.config_secrets) || []).filter(function (key) {
      if (has(cfg, key)) return false;
      return key !== 'refresh_token' && key !== 'cookie';
    });
  }

  // ---- Test ----

  // Run the driver once against the device, without saving it. Resolves
  // to the probe reply; a refused request is a failed test, not an error.
  function test(driver, fetchFn) {
    var f = fetchFn || window.fetch.bind(window);
    return f('/api/drivers/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(driver)
    }).then(function (r) {
      return r.json().then(function (body) {
        if (!r.ok) return { ok: false, error: (body && body.error) || ('HTTP ' + r.status) };
        return body || { ok: false, error: 'no reply' };
      });
    }).catch(function (err) {
      return { ok: false, error: err.message };
    });
  }

  function kw(w) {
    var a = Math.abs(w);
    return a >= 1000 ? (a / 1000).toFixed(1) + ' kW' : Math.round(a) + ' W';
  }

  // What the test read, in a few words: "grid 1.2 kW in · solar 3.4 kW".
  function readingWords(res) {
    var words = [];
    (res.readings || []).forEach(function (r) {
      var w = Number(r.smoothed_w != null ? r.smoothed_w : r.raw_w);
      if (!isFinite(w)) return;
      if (r.type === 'meter') words.push('grid ' + kw(w) + (w > 0 ? ' in' : w < 0 ? ' out' : ''));
      else if (r.type === 'pv') words.push('solar ' + kw(w));
      else if (r.type === 'battery') {
        words.push('battery ' + (r.soc != null ? Math.round(r.soc * 100) + ' %' : kw(w)));
      } else if (r.type === 'ev') words.push('charger ' + kw(w));
    });
    return words.join(' · ');
  }

  function fmtW(v) {
    if (!isFinite(v)) return '—';
    return Math.abs(v) >= 1000 ? (v / 1000).toFixed(2) + ' kW' : v.toFixed(0) + ' W';
  }

  function fmtNum(v) {
    if (!isFinite(v)) return '—';
    return Math.abs(v) >= 100 ? v.toFixed(0) : v.toFixed(2);
  }

  function fmtAge(ms) {
    if (!isFinite(ms) || ms < 0) return '—';
    var s = Math.floor(ms / 1000);
    return s < 60 ? s + 's ago' : Math.floor(s / 60) + 'm ago';
  }

  // The full test reply for Settings: status, identity, every reading and
  // up to 12 metrics. esc escapes text for markup.
  function probeHTML(res, esc) {
    var readings = res.readings || res.Readings || [];
    var metrics = res.metrics || res.Metrics || [];
    var health = res.health || res.Health || {};
    var identity = res.identity || res.Identity || {};
    var html = '<div class="driver-test-kv">';
    html += '<span>status</span><strong>' + esc(res.ok ? 'connected' : 'failed') + '</strong>';
    html += '<span>elapsed</span><strong>' + esc(String(res.elapsed_ms || res.ElapsedMs || 0)) + ' ms</strong>';
    if (health.TickCount != null) {
      html += '<span>ticks</span><strong>' + esc(String(health.TickCount)) + '</strong>';
    }
    if (identity.make || identity.sn || identity.endpoint) {
      html += '<span>identity</span><strong>' + esc([identity.make, identity.sn, identity.endpoint].filter(Boolean).join(' · ')) + '</strong>';
    }
    html += '</div>';
    if (res.error) {
      html += '<div class="driver-test-error">' + esc(res.error) + '</div>';
    }
    if (readings.length) {
      html += '<div class="driver-test-values">';
      readings.forEach(function (r) {
        var soc = r.soc != null ? ' · SoC ' + (r.soc * 100).toFixed(1) + '%' : '';
        var age = r.updated_at_ms ? ' · ' + fmtAge(Date.now() - r.updated_at_ms) : '';
        html += '<div><span>' + esc(r.type) + '</span><strong>' + esc(fmtW(r.smoothed_w)) + '</strong><small>raw ' + esc(fmtW(r.raw_w)) + soc + age + '</small></div>';
      });
      html += '</div>';
    }
    if (metrics.length) {
      html += '<div class="driver-test-metrics">';
      metrics.slice(0, 12).forEach(function (m) {
        var label = m.name;
        var value = fmtNum(m.value);
        if (m.name === 'vehicle_soc_pct') {
          label = 'Vehicle SoC';
          if (isFinite(m.value)) value = m.value + ' %';
        } else if (m.name === 'vehicle_soc_measured_at_unix_s') {
          label = 'SoC measured at';
          var measuredAt = new Date(m.value * 1000);
          if (isFinite(m.value) && isFinite(measuredAt.getTime())) value = measuredAt.toLocaleString();
        } else if (m.name === 'vehicle_soc_age_s') {
          label = 'SoC age at test';
          if (isFinite(m.value)) {
            var seconds = Math.floor(Math.max(0, m.value));
            var hours = Math.floor(seconds / 3600);
            var minutes = Math.floor((seconds % 3600) / 60);
            value = hours + ' h ' + minutes + ' min ' + (seconds % 60) + ' s';
          }
        }
        html += '<span>' + esc(label) + '</span><strong>' + esc(value) + '</strong>';
      });
      if (metrics.length > 12) {
        html += '<span>more</span><strong>' + esc(String(metrics.length - 12)) + '</strong>';
      }
      html += '</div>';
    }
    if (!readings.length && !metrics.length && !res.error) {
      html += '<div class="driver-test-empty">No values returned.</div>';
    }
    return html;
  }

  window.FTWDeviceFlow = {
    search: search,
    title: title,
    usageLabel: usageLabel,
    readsOnly: readsOnly,
    readOnlySentence: readOnlySentence,
    setupHint: setupHint,
    card: card,
    configProfiles: configProfiles,
    profileByValue: profileByValue,
    newDriver: newDriver,
    addressKind: addressKind,
    setAddress: setAddress,
    addressOf: addressOf,
    secretKeys: secretKeys,
    test: test,
    readingWords: readingWords,
    probeHTML: probeHTML
  };
})();
