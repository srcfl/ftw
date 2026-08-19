// Settings → Weather tab: forecast provider + location + PV arrays.
// Owns its own MapLibre loader + PV-array editor + 3D preview loader
// so the Settings shell stays weather-agnostic.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // MapLibre GL JS (BSD-3), pinned and loaded on demand exactly the way this
  // tab already lazy-loads its other heavy optional dependency — the picker is
  // ~1 MB and only the Weather tab needs it.
  //
  // v6 ships ESM only and is code-split (the entry pulls in
  // maplibre-gl-shared.mjs), so it is loaded with a dynamic import() rather
  // than a <script> tag. That means no integrity hash on the JS: SRI does not
  // propagate to a module's own imports, so hashing the entry point would
  // leave the larger shared chunk unverified regardless. The stylesheet is a
  // single file and keeps its hash. The version is pinned so an upstream
  // republish cannot silently change what loads.
  //
  // MapLibre detects that it is being served cross-origin and routes its
  // worker through a blob, so a CSP would need `worker-src blob:` here; FTW
  // sets no CSP today. The numeric lat/lon fields stay authoritative, so a CDN
  // or WebGL failure costs the picker and nothing else.
  var MAPLIBRE_VERSION = "6.0.0";
  var MAPLIBRE_BASE = "https://unpkg.com/maplibre-gl@" + MAPLIBRE_VERSION + "/dist/";
  var maplibreLoading = null;
  function loadMapLibre() {
    if (window.maplibregl) return Promise.resolve();
    if (maplibreLoading) return maplibreLoading;

    if (!document.getElementById("maplibre-css")) {
      var css = document.createElement("link");
      css.id = "maplibre-css";
      css.rel = "stylesheet";
      css.href = MAPLIBRE_BASE + "maplibre-gl.css";
      css.integrity = "sha256-lGfssQQWd25OyIDWYsILvB0epOQ5rDrtpFkBvfEktgk=";
      css.crossOrigin = "anonymous";
      document.head.appendChild(css);
    }

    // v6 removed the default export, so the module namespace is the API.
    maplibreLoading = import(MAPLIBRE_BASE + "maplibre-gl.mjs")
      .then(function (mod) {
        window.maplibregl = mod.default || mod;
      })
      .catch(function (e) {
        maplibreLoading = null;
        throw new Error("MapLibre GL JS failed to load: " + (e && e.message ? e.message : e));
      });
    return maplibreLoading;
  }

  var pvArraysModulePromise = null;
  var pvArraysModuleFailed = false;
  function ensurePvArraysComponent() {
    if (window.customElements.get("ftw-pv-arrays-3d")) return Promise.resolve();
    if (pvArraysModuleFailed) return Promise.reject(new Error("pv-arrays-3d unavailable"));
    if (pvArraysModulePromise) return pvArraysModulePromise;
    pvArraysModulePromise = import("/components/ftw-pv-arrays-3d.js")
      .catch(function (e) {
        pvArraysModulePromise = null;
        pvArraysModuleFailed = true;
        throw e;
      });
    return pvArraysModulePromise;
  }

  // Legacy yaml/json key `kwp` is kilowatt-peak, except values ≥ 1000
  // which were nameplate watts pasted into that field.
  function ratedWattsFromLegacyKwp(kwp) {
    var v = Number(kwp);
    if (!(v > 0)) return 0;
    return v >= 1000 ? v : v * 1000;
  }

  function migrateArrayRatedW(a) {
    if (!a) return;
    if (!(Number(a.rated_w) > 0) && Number(a.kwp) > 0) {
      a.rated_w = ratedWattsFromLegacyKwp(a.kwp);
    }
    delete a.kwp;
  }

  function renderPVArrays(ctx) {
    var host = document.getElementById("pv-arrays-list");
    if (!host) return;
    var escHtml = ctx.escHtml;
    var config = ctx.config;
    var arrays = (config.weather && config.weather.pv_arrays) || [];
    arrays.forEach(migrateArrayRatedW);
    if (arrays.length === 0) {
      host.innerHTML = '<p style="color:var(--text-dim);font-size:0.75rem;margin:4px 0 8px">No arrays defined — model will learn orientation from telemetry.</p>';
      return;
    }
    var previewHtml = '<div class="pv-arrays-3d-slot" ' +
      'style="margin:4px 0 10px"><ftw-pv-arrays-3d></ftw-pv-arrays-3d></div>';
    var rows = arrays.map(function (a, i) {
      return '<fieldset style="margin:6px 0;padding:8px 10px">' +
        '<div class="field-row" style="gap:8px;align-items:flex-end">' +
          '<div style="flex:1.4"><label>Name</label>' +
            '<input type="text" data-pv-arr="' + i + '" data-field="name" value="' + escHtml(a.name || "") + '" placeholder="e.g. south roof">' +
          '</div>' +
          '<div style="flex:1"><label>Rated (W)</label>' +
            '<input type="number" step="1" min="0" data-pv-arr="' + i + '" data-field="rated_w" value="' + (a.rated_w || 0) + '" placeholder="12960">' +
          '</div>' +
          '<div style="flex:1"><label>Tilt °</label>' +
            '<input type="number" step="1" min="0" max="90" data-pv-arr="' + i + '" data-field="tilt_deg" value="' + (a.tilt_deg || 0) + '">' +
          '</div>' +
          '<div style="flex:1"><label>Azimuth °</label>' +
            '<input type="number" step="1" min="0" max="360" data-pv-arr="' + i + '" data-field="azimuth_deg" value="' + (a.azimuth_deg || 0) + '">' +
          '</div>' +
          '<button class="btn-remove" data-pv-arr-remove="' + i + '" type="button" title="Remove">✕</button>' +
        '</div></fieldset>';
    });
    host.innerHTML = previewHtml + rows.join("");
    var pushArraysToPreview = function () {
      var el = host.querySelector("ftw-pv-arrays-3d");
      if (el && typeof el.setArrays === "function") {
        el.setArrays(config.weather.pv_arrays || []);
      }
    };
    ensurePvArraysComponent().then(pushArraysToPreview).catch(function () {
      var slot = host.querySelector(".pv-arrays-3d-slot");
      if (slot) slot.style.display = "none";
    });
    host.oninput = function (e) {
      var idx = e.target && e.target.dataset && e.target.dataset.pvArr;
      if (idx == null || idx === "") return;
      var fieldName = e.target.dataset.field;
      var arr = config.weather.pv_arrays;
      if (!arr[idx]) return;
      if (fieldName === "name") {
        arr[idx][fieldName] = e.target.value;
      } else {
        var v = parseFloat(e.target.value);
        if (!isNaN(v)) arr[idx][fieldName] = v;
      }
      pushArraysToPreview();
    };
    host.onclick = function (e) {
      var idx = e.target && e.target.dataset && e.target.dataset.pvArrRemove;
      if (idx == null || idx === "") return;
      config.weather.pv_arrays.splice(parseInt(idx, 10), 1);
      renderPVArrays(ctx);
    };
  }

  // Raster style assembled inline from the same OpenStreetMap tiles the old
  // Leaflet picker used: swapping the renderer changes neither the tile source
  // nor the attribution. Raster-only also means no glyph or sprite server is
  // needed, so the picker has exactly one external origin to reach.
  function osmRasterStyle() {
    return {
      version: 8,
      sources: {
        osm: {
          type: "raster",
          tiles: ["https://tile.openstreetmap.org/{z}/{x}/{y}.png"],
          tileSize: 256,
          maxzoom: 19,
          attribution: "© OpenStreetMap",
        },
      },
      layers: [{ id: "osm", type: "raster", source: "osm" }],
    };
  }

  function mountMap(ctx, container) {
    if (!window.maplibregl) return;
    var bodyEl = ctx.bodyEl;
    var setByPath = ctx.setByPath;
    var latInput = bodyEl.querySelector('[data-path="weather.latitude"]');
    var lonInput = bodyEl.querySelector('[data-path="weather.longitude"]');
    if (!latInput || !lonInput) return;
    var lat = parseFloat(latInput.value);
    var lon = parseFloat(lonInput.value);
    if (isNaN(lat)) lat = 59.3293;
    if (isNaN(lon)) lon = 18.0686;
    if (window._weatherMap) { try { window._weatherMap.remove(); } catch (e) {} window._weatherMap = null; }
    // MapLibre takes coordinates as [lng, lat] — the reverse of Leaflet's
    // [lat, lng]. Every call below is ordered accordingly.
    var map = new maplibregl.Map({
      container: container,
      style: osmRasterStyle(),
      center: [lon, lat],
      zoom: 11,
      attributionControl: { compact: true },
    });
    window._weatherMap = map;
    map.addControl(new maplibregl.NavigationControl({ showCompass: false }), "top-right");
    var marker = new maplibregl.Marker({ draggable: true }).setLngLat([lon, lat]).addTo(map);
    function setCoord(la, lo) {
      latInput.value = la.toFixed(4);
      lonInput.value = lo.toFixed(4);
      setByPath(ctx.config, "weather.latitude", la);
      setByPath(ctx.config, "weather.longitude", lo);
      renderCoverage(ctx, la, lo);
    }
    marker.on("dragend", function () {
      var ll = marker.getLngLat();
      setCoord(ll.lat, ll.lng);
    });
    map.on("click", function (e) {
      marker.setLngLat(e.lngLat);
      setCoord(e.lngLat.lat, e.lngLat.lng);
    });
    function syncFromInputs() {
      var la = parseFloat(latInput.value), lo = parseFloat(lonInput.value);
      if (!isNaN(la) && !isNaN(lo)) {
        marker.setLngLat([lo, la]);
        map.panTo([lo, la]);
        renderCoverage(ctx, la, lo);
      }
    }
    latInput.addEventListener("change", syncFromInputs);
    lonInput.addEventListener("change", syncFromInputs);
    setTimeout(function () { map.resize(); }, 150);
  }

  // Data-source coverage for the current pin. Several sources are regional
  // (STRÅNG is Nordic-only, every price provider is European) and until now
  // nothing said so — a site outside them just got empty results. Rendered
  // from GET /api/data-sources, which answers for a specific lat/lon.
  var coverageSeq = 0;
  function renderCoverage(ctx, lat, lon) {
    var host = document.getElementById("data-coverage");
    if (!host) return;
    // Coordinates change on every marker drag; keep only the newest response
    // so a slow early request cannot overwrite a fast later one.
    var seq = ++coverageSeq;
    var q = lat != null && lon != null
      ? "?lat=" + encodeURIComponent(lat) + "&lon=" + encodeURIComponent(lon)
      : "";
    fetch("/api/data-sources" + q)
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (seq !== coverageSeq || !d || !Array.isArray(d.sources)) return;
        var esc = ctx.escHtml;
        var unavailable = d.sources.filter(function (s) { return s.covers === false; });
        var kinds = { forecast: "Forecast", irradiance: "Irradiance", price: "Price" };
        var rows = ["forecast", "irradiance", "price"].map(function (kind) {
          var list = d.sources.filter(function (s) { return s.kind === kind; });
          if (!list.length) return "";
          var names = list.map(function (s) {
            var ok = s.covers !== false;
            var mark = ok ? "✓" : "✕";
            var col = ok ? "var(--text-dim)" : "var(--warn, #f59e0b)";
            return '<span style="color:' + col + '" title="' + esc(s.area + (s.note ? " — " + s.note : "")) + '">' +
              mark + " " + esc(s.label) + "</span>";
          }).join('<span style="color:var(--text-dim)"> · </span>');
          return '<div style="margin:2px 0"><span style="display:inline-block;min-width:74px;color:var(--text-dim)">' +
            kinds[kind] + "</span>" + names + "</div>";
        }).join("");
        var warn = unavailable.length
          ? '<p style="color:var(--warn, #f59e0b);font-size:0.72rem;margin:6px 0 0">' +
            esc(unavailable.length === 1
              ? unavailable[0].label + " does not cover this location."
              : unavailable.length + " sources do not cover this location.") +
            " Hover for details." +
            '</p>'
          : "";
        host.innerHTML =
          '<div style="font-size:0.72rem;font-family:var(--mono);border:1px solid var(--line);' +
          'border-radius:6px;padding:8px 10px;background:var(--ink-sunken)">' +
          '<div style="color:var(--text-dim);margin-bottom:4px">Data sources available here</div>' +
          rows + warn + "</div>";
      })
      .catch(function () { /* coverage is advisory; never break the tab */ });
  }

  function initWeatherMap(ctx) {
    var container = document.getElementById("weather-map");
    if (!container) return;
    // mountMap throws on hosts without WebGL; the same catch that handles a CDN
    // failure degrades those to the numeric fields.
    loadMapLibre().then(function () { mountMap(ctx, container); })
      .catch(function (e) { container.textContent = "map unavailable: " + e.message; });
  }

  S.tabs.weather = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField, help = ctx.help, config = ctx.config;
      if (!config.weather) config.weather = { latitude: 59.3293, longitude: 18.0686 };
      if (!Array.isArray(config.weather.pv_arrays)) config.weather.pv_arrays = [];
      return '<fieldset><legend>Weather forecast &amp; PV</legend>' +
        selectField("Provider", "weather.provider", ["met_no", "openweather", "open_meteo", "forecast_solar", "none"], "met_no",
          "met_no + openweather: cloud-cover only. open_meteo: direct shortwave radiation (better day-one forecast). forecast_solar: site-calibrated watts using the panel geometry below (best with multi-array setups).") +
        '<div class="field-row"><div>' +
        field("Latitude", "weather.latitude", "number", 59.3293) +
        '</div><div>' +
        field("Longitude", "weather.longitude", "number", 18.0686) +
        '</div></div>' +
        '<div id="weather-map" style="height:260px;border-radius:6px;margin:6px 0;background:var(--ink-sunken)"></div>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:-2px 0 8px">Click or drag the marker to set your location.</p>' +
        '<div id="data-coverage" style="margin:0 0 10px"></div>' +
        field("PV rated (W)", "weather.pv_rated_w", "number", 10000) +
        field("API key (OpenWeather only)", "weather.api_key", "text", "") +
        '</fieldset>' +
        '<fieldset><legend>PV arrays ' + help(
          'Optional. Open-Meteo uses these per-plane values to project shortwave radiation onto each array. ' +
          'Forecast.Solar uses them for its site-calibrated forecast. Leave empty for the safe flat estimate or the provider default.') + '</legend>' +
        '<div id="pv-arrays-list"></div>' +
        '<button class="btn-add" id="pv-array-add" type="button">+ Add array</button>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:8px 0 0">' +
        'Tilt: 0° = flat roof, 35° = typical pitched roof, 90° = wall. Azimuth: 0 = N, 90 = E, 180 = S, 270 = W. ' +
        'Rated (W) is watts, same unit as PV rated.' +
        '</p>' +
        '</fieldset>';
    },
    after: function (ctx) {
      initWeatherMap(ctx);
      renderPVArrays(ctx);
      // The map may fail (no WebGL, CDN blocked); coverage must still render,
      // so drive it from the numeric fields rather than from the map.
      var w = (ctx.config && ctx.config.weather) || {};
      renderCoverage(ctx, w.latitude, w.longitude);
      var addBtn = document.getElementById("pv-array-add");
      if (addBtn) addBtn.addEventListener("click", function () {
        ctx.config.weather.pv_arrays.push({ name: "", rated_w: 0, tilt_deg: 35, azimuth_deg: 180 });
        renderPVArrays(ctx);
      });
    },
  };
})();
