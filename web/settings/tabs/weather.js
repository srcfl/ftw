// Settings → Weather tab: forecast provider + location + PV arrays.
// Owns its own Leaflet loader + PV-array editor + 3D preview loader
// so the Settings shell stays weather-agnostic.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  var leafletLoading = null;
  function loadLeaflet() {
    if (window.L) return Promise.resolve();
    if (leafletLoading) return leafletLoading;
    leafletLoading = new Promise(function (resolve, reject) {
      var css = document.createElement("link");
      css.rel = "stylesheet";
      css.href = "https://unpkg.com/leaflet@1.9.4/dist/leaflet.css";
      css.integrity = "sha256-p4NxAoJBhIIN+hmNHrzRCf9tD/miZyoHS5obTRR9BMY=";
      css.crossOrigin = "";
      document.head.appendChild(css);

      var script = document.createElement("script");
      script.src = "https://unpkg.com/leaflet@1.9.4/dist/leaflet.js";
      script.integrity = "sha256-20nQCchB9co0qIjJZRGuk2/Z9VM+kNiyxNV1lvTlZBo=";
      script.crossOrigin = "";
      script.async = true;
      script.onload = function () { resolve(); };
      script.onerror = function () {
        leafletLoading = null;
        reject(new Error("Leaflet failed to load"));
      };
      document.head.appendChild(script);
    });
    return leafletLoading;
  }

  // Terra Draw (MIT) supplies the drawing. Both bundles are UMD, so unlike an
  // ES module they can carry a real integrity hash: one self-contained file
  // each, with no sub-imports for SRI to silently miss.
  var TERRA_DRAW = {
    src: "https://unpkg.com/terra-draw@1.32.2/dist/terra-draw.umd.js",
    integrity: "sha384-TYV8O/5VLLcJCLall6+2ipTEOgCQ0Fy4YkAoL3AU15s0qmNqs20zeWGiIJyMiztH",
  };
  var TERRA_DRAW_LEAFLET = {
    src: "https://unpkg.com/terra-draw-leaflet-adapter@1.3.0/dist/terra-draw-leaflet-adapter.umd.js",
    integrity: "sha384-ynliNmmpIbn8Cwh1eYvXemMxJbdMJtdN/czSOHWWCiF9vp7ZiFu8WRdfTUUNYiA5",
  };

  function loadScript(spec) {
    return new Promise(function (resolve, reject) {
      var script = document.createElement("script");
      script.src = spec.src;
      script.integrity = spec.integrity;
      script.crossOrigin = "anonymous";
      script.async = true;
      script.onload = function () { resolve(); };
      script.onerror = function () { reject(new Error("could not load " + spec.src)); };
      document.head.appendChild(script);
    });
  }

  var terraDrawLoading = null;
  function loadTerraDraw() {
    if (window.terraDrawLeafletAdapter) return Promise.resolve();
    if (terraDrawLoading) return terraDrawLoading;
    terraDrawLoading = loadLeaflet()
      .then(function () {
        // The adapter's UMD captures window.leaflet as it evaluates and uses
        // it for its own vertex markers. Leaflet itself only ever defines
        // window.L, so without this alias the adapter loads and then throws
        // the first time a vertex is drawn.
        if (!window.leaflet) window.leaflet = window.L;
        return loadScript(TERRA_DRAW);
      })
      .then(function () { return loadScript(TERRA_DRAW_LEAFLET); })
      .catch(function (e) { terraDrawLoading = null; throw e; });
    return terraDrawLoading;
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

  function renderPVArrays(ctx) {
    var host = document.getElementById("pv-arrays-list");
    if (!host) return;
    var escHtml = ctx.escHtml;
    var config = ctx.config;
    var arrays = (config.weather && config.weather.pv_arrays) || [];
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
          '<div style="flex:1"><label>kWp</label>' +
            '<input type="number" step="0.1" data-pv-arr="' + i + '" data-field="kwp" value="' + (a.kwp || 0) + '">' +
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

  // --- drawing arrays on the map -------------------------------------------
  // A drawn rectangle answers two of the three questions an array asks: how
  // big it is, and which way it is turned. Tilt is the one thing an overhead
  // outline cannot show, so it is typed once before drawing and used to turn
  // the outline into real panel area.
  var drawInstance = null;
  var drawGeometry = null;
  var drawHandled = {};
  var lastDrawnArray = null;

  function drawStatus(html) {
    var el = document.getElementById("pv-draw-status");
    if (el) el.innerHTML = html;
  }

  function drawnTiltDeg() {
    var el = document.getElementById("pv-draw-tilt");
    var v = el ? parseFloat(el.value) : NaN;
    return isNaN(v) ? drawGeometry.DEFAULT_TILT_DEG : Math.min(Math.max(v, 0), 90);
  }

  function onRectangleFinished(ctx, id) {
    if (drawHandled[id]) return;
    drawHandled[id] = true;
    var snapshot = drawInstance.getSnapshot() || [];
    var feature = null;
    for (var i = 0; i < snapshot.length; i++) {
      if (snapshot[i] && snapshot[i].id === id) feature = snapshot[i];
    }
    if (!feature || !feature.geometry || feature.geometry.type !== "Polygon") return;
    var weather = ctx.config.weather;
    var derived = drawGeometry.arrayFromRing(feature.geometry.coordinates[0], {
      latitude: weather.latitude,
      tiltDeg: drawnTiltDeg(),
    });
    if (!derived) {
      drawStatus("That outline enclosed no area — draw it again.");
      return;
    }
    weather.pv_arrays.push(derived.array);
    lastDrawnArray = derived.array;
    renderPVArrays(ctx);
    drawStatus(
      "<strong>" + ctx.escHtml(derived.array.name) + "</strong> added: " +
      derived.planAreaM2 + " m² outline is " + derived.slopeAreaM2 + " m² of roof at " +
      derived.array.tilt_deg + "°, about " + derived.array.kwp + " kWp. " +
      "Facing " + derived.array.azimuth_deg + "° — the outline fits " +
      derived.azimuthCandidates.join("° and ") + "° equally well. " +
      '<button type="button" id="pv-draw-flip">Flip 180°</button> ' +
      "Draw another, or edit the numbers below."
    );
  }

  function startArrayDrawing(ctx) {
    var map = window._weatherMap;
    if (!map) {
      drawStatus("The map has to finish loading before you can draw on it.");
      return;
    }
    drawStatus("Loading the drawing tools…");
    Promise.all([loadTerraDraw(), import("/components/pv-array-geometry.js")])
      .then(function (loaded) {
        drawGeometry = loaded[1];
        if (!drawInstance) {
          drawInstance = new window.terraDraw.TerraDraw({
            adapter: new window.terraDrawLeafletAdapter.TerraDrawLeafletAdapter({
              lib: window.L,
              map: map,
            }),
            modes: [new window.terraDraw.TerraDrawAngledRectangleMode()],
          });
          drawInstance.start();
          drawInstance.on("finish", function (id) { onRectangleFinished(ctx, id); });
        }
        drawInstance.setMode("angled-rectangle");
        var container = document.getElementById("weather-map");
        if (container && container.scrollIntoView) {
          container.scrollIntoView({ block: "nearest" });
        }
        drawStatus(
          "Click one corner of your panels, click along the ridge to set the " +
          "angle, then click again to finish the rectangle."
        );
      })
      .catch(function (e) {
        drawStatus("Drawing is unavailable (" + ctx.escHtml(e.message) +
          "). The numbers below still work.");
      });
  }

  function stopArrayDrawing() {
    if (!drawInstance) return;
    // "static" keeps what has been drawn on the map while stopping new
    // drawing; stop() would take the outlines with it.
    try { drawInstance.setMode("static"); } catch (e) { /* stays in draw mode */ }
    drawStatus("Drawing finished. The arrays below are yours to edit.");
  }

  function mountMap(ctx, container) {
    if (!window.L) return;
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
    var map = L.map(container, { zoomControl: true }).setView([lat, lon], 11);
    window._weatherMap = map;
    L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png", {
      maxZoom: 18,
      attribution: "© OpenStreetMap",
    }).addTo(map);
    var marker = L.marker([lat, lon], { draggable: true }).addTo(map);
    function setCoord(la, lo) {
      latInput.value = la.toFixed(4);
      lonInput.value = lo.toFixed(4);
      setByPath(ctx.config, "weather.latitude", la);
      setByPath(ctx.config, "weather.longitude", lo);
    }
    marker.on("dragend", function () {
      var ll = marker.getLatLng();
      setCoord(ll.lat, ll.lng);
    });
    map.on("click", function (e) {
      marker.setLatLng(e.latlng);
      setCoord(e.latlng.lat, e.latlng.lng);
    });
    function syncFromInputs() {
      var la = parseFloat(latInput.value), lo = parseFloat(lonInput.value);
      if (!isNaN(la) && !isNaN(lo)) {
        marker.setLatLng([la, lo]);
        map.panTo([la, lo]);
      }
    }
    latInput.addEventListener("change", syncFromInputs);
    lonInput.addEventListener("change", syncFromInputs);
    setTimeout(function () { map.invalidateSize(); }, 150);
  }

  function initWeatherMap(ctx) {
    var container = document.getElementById("weather-map");
    if (!container) return;
    loadLeaflet().then(function () { mountMap(ctx, container); })
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
        field("PV rated (W)", "weather.pv_rated_w", "number", 10000) +
        field("API key (OpenWeather only)", "weather.api_key", "text", "") +
        '</fieldset>' +
        '<fieldset><legend>PV arrays ' + help(
          'Optional. Open-Meteo uses these per-plane values to project shortwave radiation onto each array. ' +
          'Forecast.Solar uses them for its site-calibrated forecast. Leave empty for the safe flat estimate or the provider default.') + '</legend>' +
        '<div id="pv-arrays-list"></div>' +
        '<div class="field-row" style="gap:8px;align-items:flex-end;margin-top:4px">' +
          '<button class="btn-add" id="pv-array-add" type="button">+ Add array</button>' +
          '<button class="btn-add" id="pv-array-draw" type="button">✎ Draw on the map</button>' +
          '<button class="btn-add" id="pv-array-draw-done" type="button">Done drawing</button>' +
          '<div style="flex:0 0 7rem"><label>Roof tilt °</label>' +
            '<input type="number" step="1" min="0" max="90" id="pv-draw-tilt" value="35">' +
          '</div>' +
        '</div>' +
        '<p id="pv-draw-status" style="color:var(--text-dim);font-size:0.75rem;margin:6px 0 0"></p>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:8px 0 0">' +
        'Drawing gives the size and the direction; tilt is the one thing an overhead ' +
        'outline cannot show, so set it above before you draw — a 35° roof holds about ' +
        '22 % more panel than its outline suggests. ' +
        'Tilt: 0° = flat roof, 35° = typical pitched roof, 90° = wall. Azimuth: 0 = N, 90 = E, 180 = S, 270 = W.' +
        '</p>' +
        '</fieldset>';
    },
    after: function (ctx) {
      initWeatherMap(ctx);
      renderPVArrays(ctx);
      var addBtn = document.getElementById("pv-array-add");
      if (addBtn) addBtn.addEventListener("click", function () {
        ctx.config.weather.pv_arrays.push({ name: "", kwp: 0, tilt_deg: 35, azimuth_deg: 180 });
        renderPVArrays(ctx);
      });
      var drawBtn = document.getElementById("pv-array-draw");
      if (drawBtn) drawBtn.addEventListener("click", function () { startArrayDrawing(ctx); });
      var doneBtn = document.getElementById("pv-array-draw-done");
      if (doneBtn) doneBtn.addEventListener("click", stopArrayDrawing);
      var status = document.getElementById("pv-draw-status");
      if (status) status.addEventListener("click", function (e) {
        if (!e.target || e.target.id !== "pv-draw-flip" || !lastDrawnArray) return;
        // Flip the object, not an index: removing another row above it would
        // otherwise silently turn a different roof around.
        lastDrawnArray.azimuth_deg = drawGeometry.flipAzimuthDeg(lastDrawnArray.azimuth_deg);
        lastDrawnArray.name = drawGeometry.compassName(
          lastDrawnArray.azimuth_deg, lastDrawnArray.tilt_deg);
        renderPVArrays(ctx);
        drawStatus("Now facing " + lastDrawnArray.azimuth_deg + "°.");
      });
    },
  };
})();
