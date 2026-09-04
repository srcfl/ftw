// Forecast-input sections of the Settings → Control tab: forecast
// provider + location + PV arrays. Registered as S.tabs.weather but
// rendered inside the Control tab (control.js delegates here); there is
// no Weather tab button. Owns its own MapLibre loader + PV-array editor
// + 3D preview loader so the Settings shell stays weather-agnostic.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // MapLibre GL JS (BSD-3), vendored under /vendor/maplibre and loaded on
  // demand exactly the way this tab already lazy-loads its other heavy
  // optional dependency — the picker is ~1 MB and only these sections need
  // it. Shipping it on the box follows the same policy as /vendor/three and
  // the Leaflet copy this replaced: no third-party JS from a CDN, and the map
  // must load when the gateway cannot reach the internet.
  //
  // v6 ships ESM only and is code-split (the entry pulls in
  // maplibre-gl-shared.mjs and spawns maplibre-gl-worker.mjs by relative
  // URL), so it is loaded with a dynamic import() rather than a <script>
  // tag. Same-origin also means the worker loads directly instead of through
  // the blob: detour MapLibre uses for cross-origin serving. The numeric
  // lat/lon fields stay authoritative, so a WebGL failure costs the picker
  // and nothing else.
  var MAPLIBRE_BASE = "/vendor/maplibre/";
  var maplibreLoading = null;
  function loadMapLibre() {
    if (window.maplibregl) return Promise.resolve();
    if (maplibreLoading) return maplibreLoading;

    if (!document.getElementById("maplibre-css")) {
      var css = document.createElement("link");
      css.id = "maplibre-css";
      css.rel = "stylesheet";
      css.href = MAPLIBRE_BASE + "maplibre-gl.css";
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

  // Terra Draw (MIT) supplies the drawing, vendored under /vendor/terra-draw
  // for the same reason MapLibre above ships on the box: no third-party CDN
  // JS, and the drawing tools load without internet. Both bundles are UMD —
  // one self-contained file each.
  var TERRA_DRAW_SRC = "/vendor/terra-draw/terra-draw.umd.js";
  var TERRA_DRAW_MAPLIBRE_SRC = "/vendor/terra-draw/terra-draw-maplibre-gl-adapter.umd.js";

  function loadScript(src) {
    return new Promise(function (resolve, reject) {
      var script = document.createElement("script");
      script.src = src;
      script.async = true;
      script.onload = function () { resolve(); };
      script.onerror = function () { reject(new Error("could not load " + src)); };
      document.head.appendChild(script);
    });
  }

  var terraDrawLoading = null;
  function loadTerraDraw() {
    if (window.terraDrawMaplibreGlAdapter) return Promise.resolve();
    if (terraDrawLoading) return terraDrawLoading;
    // The adapter's UMD only needs the terra-draw global — the map instance
    // is handed to it at construction — but the map library must already be
    // up, which loadMapLibre guarantees before any drawing can start.
    terraDrawLoading = loadMapLibre()
      .then(function () { return loadScript(TERRA_DRAW_SRC); })
      .then(function () { return loadScript(TERRA_DRAW_MAPLIBRE_SRC); })
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
      host.innerHTML = '<p style="color:var(--text-dim);font-size:0.75rem;margin:4px 0 8px">No arrays defined. The model learns the production pattern from measured solar.</p>';
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
      refreshArraysSummary(config);
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
  // What the next finished shape means: a PV array rectangle, or the
  // building footprint the LiDAR should be clipped to. One Terra Draw
  // instance serves both; the entry points set the purpose.
  var drawPurpose = "array";

  function drawStatus(html) {
    var el = document.getElementById("pv-draw-status");
    if (el) el.innerHTML = html;
  }

  function drawnTiltDeg() {
    var el = document.getElementById("pv-draw-tilt");
    var v = el ? parseFloat(el.value) : NaN;
    return isNaN(v) ? drawGeometry.DEFAULT_TILT_DEG : Math.min(Math.max(v, 0), 90);
  }

  function onShapeFinished(ctx, id) {
    if (drawHandled[id]) return;
    drawHandled[id] = true;
    var snapshot = drawInstance.getSnapshot() || [];
    var feature = null;
    for (var i = 0; i < snapshot.length; i++) {
      if (snapshot[i] && snapshot[i].id === id) feature = snapshot[i];
    }
    if (!feature || !feature.geometry || feature.geometry.type !== "Polygon") return;
    if (drawPurpose === "footprint") {
      onFootprintFinished(feature);
      return;
    }
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
      derived.array.tilt_deg + "°, about " + derived.array.rated_w + " W. " +
      "Facing " + derived.array.azimuth_deg + "° — the outline fits " +
      derived.azimuthCandidates.join("° and ") + "° equally well. " +
      '<button type="button" id="pv-draw-flip">Flip 180°</button> ' +
      "Draw another, or edit the numbers below."
    );
  }

  function ensureDrawInstance(ctx, map) {
    if (drawInstance) return;
    drawInstance = new window.terraDraw.TerraDraw({
      adapter: new window.terraDrawMaplibreGlAdapter.TerraDrawMapLibreGLAdapter({
        map: map,
      }),
      modes: [
        new window.terraDraw.TerraDrawAngledRectangleMode(),
        new window.terraDraw.TerraDrawPolygonMode(),
      ],
    });
    drawInstance.start();
    drawInstance.on("finish", function (id) { onShapeFinished(ctx, id); });
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
        ensureDrawInstance(ctx, map);
        drawPurpose = "array";
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

  function startFootprintDrawing(ctx) {
    var map = window._weatherMap;
    if (!map) {
      roofSay("The map has to finish loading before you can draw on it.", "bad");
      return;
    }
    roofSay("Loading the drawing tools…");
    loadTerraDraw()
      .then(function () {
        ensureDrawInstance(ctx, map);
        drawPurpose = "footprint";
        drawInstance.setMode("polygon");
        var container = document.getElementById("weather-map");
        if (container && container.scrollIntoView) {
          container.scrollIntoView({ block: "nearest" });
        }
        roofSay(
          "Click each corner of the building on the map, then click the " +
          "first corner again to close the outline."
        );
      })
      .catch(function (e) {
        roofSay("Drawing is unavailable (" + ctx.escHtml(e.message) + ").", "bad");
      });
  }

  function onFootprintFinished(feature) {
    var ring = (feature.geometry.coordinates || [])[0] || [];
    if (ring.length < 4) { // GeoJSON rings close themselves: 3 corners = 4 points
      roofSay("That outline has too few corners — draw it again.", "bad");
      return;
    }
    roofState.drawnFootprint = ring;
    // A drawn footprint and a picked building answer the same question;
    // the newest answer wins.
    roofState.selectedId = null;
    drawBuildings();
    try { drawInstance.setMode("static"); } catch (e) { /* keeps drawing */ }
    var derive = document.getElementById("roof-derive");
    if (derive) derive.disabled = false;
    roofSay("Footprint drawn (" + (ring.length - 1) + " corners). " +
            "<em>Read roof from LiDAR</em> will clip the laser scan to it.");
  }

  function stopArrayDrawing() {
    if (!drawInstance) return;
    // "static" keeps what has been drawn on the map while stopping new
    // drawing; stop() would take the outlines with it.
    try { drawInstance.setMode("static"); } catch (e) { /* stays in draw mode */ }
    drawStatus("Drawing finished. The arrays below are yours to edit.");
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
      // OSM volunteer tiles 403 when the page sends no Referer. The box sends
      // Referrer-Policy: no-referrer on every response, so tile requests must
      // opt back in. Origin only; the path stays private.
      transformRequest: function (url) {
        return { url: url, referrerPolicy: "strict-origin-when-cross-origin" };
      },
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
      // A click on a building footprint selects that building (the layer's own
      // handler). It must not also drag the site pin there and silently
      // rewrite the saved coordinates.
      if (map.getLayer("roof-buildings-fill") &&
          map.queryRenderedFeatures(e.point, { layers: ["roof-buildings-fill"] }).length) {
        return;
      }
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

  function arraysSummary(n) {
    var count = Number(n) || 0;
    if (count > 0) {
      return count === 1 ? "1 array set in config" : count + " arrays set in config";
    }
    return "The solar production pattern is learned from measured solar.";
  }

  function refreshArraysSummary(config) {
    var el = document.getElementById("pv-arrays-summary");
    if (!el) return;
    var n = ((config.weather && config.weather.pv_arrays) || []).length;
    el.textContent = arraysSummary(n);
  }

  // ---- Roof geometry from Lantmäteriet -------------------------------------
  //
  // Typing a tilt and an azimuth means measuring your own roof, and most people
  // guess. Sweden publishes the two datasets needed to do it properly, so this
  // picks your building off a map and reads the slants out of the LiDAR.
  //
  // Both datasets are free but sit behind a Geotorget account, so the operator
  // brings their own credentials. Everything degrades to the numeric fields
  // above when the module, the credentials or the coverage is missing.

  var roofState = { features: [], selectedId: null, drawnFootprint: null };

  // Known point-cloud catalogs, verified live 2026-09-02. `base: ""` means
  // the module's built-in Lantmäteriet defaults; `base: null` marks the
  // custom entry, which leaves whatever the operator typed alone.
  var STAC_CATALOGS = [
    { key: "lantmateriet", label: "Lantmäteriet (Sweden — Geotorget account)",
      base: "", buildings: "", lidar: "" },
    { key: "ign-france", label: "IGN LiDAR HD (France — open, no account)",
      base: "https://api.stac.teledetection.fr", buildings: "", lidar: "lidarhd" },
    { key: "kagis", label: "KAGIS ALS (Austria/Carinthia — open, no account)",
      base: "https://gis.ktn.gv.at/api/stac/v1", buildings: "", lidar: "" },
    { key: "custom", label: "Custom STAC endpoint…", base: null },
  ];

  // Set when the operator picks "Custom" while the base URL is still empty,
  // so the re-render doesn't snap the select back to Lantmäteriet before
  // they had a chance to type the root.
  var roofCustomCatalog = false;

  function catalogPresetKey(rm) {
    var base = String(rm.stac_base_url || "").replace(/\/+$/, "");
    if (!base) return roofCustomCatalog ? "custom" : "lantmateriet";
    for (var i = 0; i < STAC_CATALOGS.length; i++) {
      var c = STAC_CATALOGS[i];
      if (c.base && c.base.replace(/\/+$/, "") === base) return c.key;
    }
    return "custom";
  }

  function roofFieldset(ctx) {
    var field = ctx.field, help = ctx.help, config = ctx.config;
    if (!config.roofmodel) config.roofmodel = {};
    var stored = config.roofmodel.has_stac_password;
    var preset = catalogPresetKey(config.roofmodel);
    var isDefault = preset === "lantmateriet";
    var options = STAC_CATALOGS.map(function (c) {
      return '<option value="' + c.key + '"' + (c.key === preset ? " selected" : "") + '>' +
        ctx.escHtml(c.label) + '</option>';
    }).join("");
    return '<fieldset><legend>Roof geometry from LiDAR ' + help(
        'Optional. Reads the tilt and azimuth of each roof face out of a ' +
        'LiDAR point cloud for a building you pick on the map, and fills in ' +
        'the PV arrays above. The default catalog is Lantmäteriet (Sweden), ' +
        'which needs a free Geotorget account — sign in with the account\'s ' +
        'own username and password. The open catalogs need no account, and ' +
        'any STAC-conformant endpoint can be entered as a custom catalog.') +
      '</legend>' +
      '<label><input type="checkbox" data-checkbox-path="roofmodel.enabled"' +
      (config.roofmodel.enabled ? " checked" : "") + '> Enable roof derivation</label>' +
      '<label>Data catalog ' + help(
        'Where the building footprints and the point cloud come from. ' +
        'Lantmäteriet covers all of Sweden (buildings + LiDAR). IGN LiDAR HD ' +
        'covers France with open COPC point clouds but publishes no building ' +
        'footprints, so the building picker cannot narrow the derive there. ' +
        'KAGIS covers Carinthia (Austria): pick the ALS2 point-cloud ' +
        'collection for your zone as the LiDAR collection — mind that its ' +
        'tiles run to hundreds of MB. For anything else, choose Custom and ' +
        'paste the STAC API root; open catalogs need no credentials.') + '</label>' +
      '<select id="roof-catalog">' + options + '</select>' +
      '<div id="roof-catalog-fields" style="display:' + (isDefault ? "none" : "block") + '">' +
      field("STAC API root", "roofmodel.stac_base_url", "text", "") +
      '<div class="field-row"><div>' +
      field("Buildings collection", "roofmodel.stac_buildings_collection", "text", "",
        "STAC collection id for building footprints. Leave empty if the catalog has none — the derive then reads the whole search radius.") +
      '</div><div>' +
      field("LiDAR collection", "roofmodel.stac_lidar_collection", "text", "",
        "STAC collection id for the point cloud.") +
      '</div></div>' +
      '</div>' +
      '<div class="field-row"><div>' +
      field(isDefault ? "Geotorget username" : "Catalog username (empty for open catalogs)",
            "roofmodel.stac_username", "text", "") +
      '</div><div>' +
      field((isDefault ? "Geotorget password" : "Catalog password") +
            (stored ? " (stored — type to replace)" : ""),
            "roofmodel.stac_password", "password", "") +
      '</div></div>' +
      '<div style="display:flex;gap:8px;flex-wrap:wrap;margin:8px 0 0">' +
      '<button class="btn-add" id="roof-find" type="button">Find buildings here</button>' +
      '<button class="btn-add" id="roof-draw-footprint" type="button">Draw the footprint on the map</button>' +
      '<button class="btn-add" id="roof-derive" type="button" disabled>Read roof from LiDAR</button>' +
      '</div>' +
      '<p style="color:var(--text-dim);font-size:0.72rem;margin:6px 0 0">' +
      'Drawing the footprint yourself is optional: it stands in for ' +
      '<em>Find buildings here</em> when the catalog publishes no building ' +
      'footprints over STAC for your region — the open LiDAR catalogs ' +
      'mostly ship point clouds only. Trace your building\'s outline and ' +
      'the laser scan is clipped to it.' +
      '</p>' +
      '<div id="roof-status" style="font-size:0.75rem;color:var(--text-dim);margin:8px 0 0"></div>' +
      '<div id="roof-buildings" style="margin:6px 0 0"></div>' +
      '<p style="color:var(--text-dim);font-size:0.72rem;margin:8px 0 0">' +
      (isDefault
        ? 'Data © Lantmäteriet (CC BY 4.0). '
        : 'Check the catalog\'s licence and attribution terms. ') +
      'Derived values are a starting point — check them ' +
      'against your installation before relying on the forecast.' +
      '</p>' +
      '</fieldset>';
  }

  function roofSay(html, tone) {
    var el = document.getElementById("roof-status");
    if (!el) return;
    el.style.color = tone === "bad" ? "var(--warn, #f59e0b)" : "var(--text-dim)";
    el.innerHTML = html;
  }

  function findBuildings(ctx) {
    var w = (ctx.config && ctx.config.weather) || {};
    var q = "";
    if (w.latitude != null && w.longitude != null) {
      q = "?lat=" + encodeURIComponent(w.latitude) + "&lon=" + encodeURIComponent(w.longitude);
    }
    roofSay("Searching Lantmäteriet for buildings…");
    return fetch("/api/roofmodel/buildings" + q)
      .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
      .then(function (res) {
        var d = res.d || {};
        if (d.enabled === false) {
          roofSay("Roof derivation is off. Tick <em>Enable roof derivation</em>, add your " +
                  "Geotorget credentials and save, then try again.", "bad");
          return;
        }
        if (!res.ok || d.error) {
          roofSay(ctx.escHtml(d.error || "the building search failed"), "bad");
          return;
        }
        roofState.features = d.buildings || [];
        roofState.selectedId = null;
        if (!roofState.features.length) {
          roofSay("No buildings found here. Move the marker onto your roof and try again.", "bad");
          return;
        }
        roofSay("Found " + roofState.features.length +
                " building(s). Pick yours on the map or in the list.");
        drawBuildings();
        fitToBuildings(d.latitude, d.longitude);
        renderBuildingList(ctx);
        var mapEl = document.getElementById("weather-map");
        if (mapEl && mapEl.scrollIntoView) mapEl.scrollIntoView({ block: "nearest" });
      })
      .catch(function (e) { roofSay(ctx.escHtml(String(e && e.message || e)), "bad"); });
  }

  function featureCollection() {
    return {
      type: "FeatureCollection",
      features: roofState.features.map(function (f) {
        var p = Object.assign({}, f.properties || {});
        p.selected = (f.id || p.building_id) === roofState.selectedId;
        return { type: "Feature", id: f.id, geometry: f.geometry, properties: p };
      }),
    };
  }

  // MapLibre paints with concrete colours and cannot read var(), the same
  // problem the canvas charts have. Resolve the theme tokens through a hidden
  // probe that inherits :root, exactly as app.js's cssColor does. The theme
  // authors its tokens in oklch(), which getComputedStyle passes through
  // verbatim and MapLibre's parser rejects — so bake the resolved colour to
  // sRGB bytes through a 1x1 canvas, whose getImageData is sRGB by contract.
  // Resolved once per layer creation, so a theme toggle mid-pick keeps the old
  // hue until the tab is reopened — the footprints stay legible either way.
  var _probe = null;
  function themeColor(name, fallback) {
    if (!_probe) {
      _probe = document.createElement("span");
      _probe.style.cssText = "position:absolute;visibility:hidden;pointer-events:none";
      document.body.appendChild(_probe);
    }
    _probe.style.color = "var(" + name + ", " + fallback + ")";
    var resolved = getComputedStyle(_probe).color || fallback;
    try {
      var ctx = document.createElement("canvas").getContext("2d", { willReadFrequently: true });
      ctx.fillStyle = fallback;  // an unparseable resolved value leaves this
      ctx.fillStyle = resolved;
      ctx.fillRect(0, 0, 1, 1);
      var px = ctx.getImageData(0, 0, 1, 1).data;
      return "rgb(" + px[0] + "," + px[1] + "," + px[2] + ")";
    } catch (e) {
      return fallback;
    }
  }

  // A "case" condition must be a typed boolean: a bare ["get", ...] is value-
  // typed, and MapLibre rejects the whole layer through its error event without
  // throwing — the picker then looks enabled while the map stays empty.
  function whenSelected(then, otherwise) {
    return ["case", ["boolean", ["get", "selected"], false], then, otherwise];
  }

  function drawBuildings() {
    var map = window._weatherMap;
    if (!map) return;
    if (!map.isStyleLoaded || !map.isStyleLoaded()) {
      // A find can win the race against the style. Idempotent, so a stacked
      // retry only costs a setData with identical data.
      map.once("load", drawBuildings);
      return;
    }
    var data = featureCollection();
    var src = map.getSource("roof-buildings");
    if (src) { src.setData(data); return; }
    var candidate = themeColor("--cyan", "#38bdf8");
    var picked = themeColor("--accent-e", "#f5b942");
    map.addSource("roof-buildings", { type: "geojson", data: data });
    map.addLayer({
      id: "roof-buildings-fill", type: "fill", source: "roof-buildings",
      paint: {
        "fill-color": whenSelected(picked, candidate),
        "fill-opacity": whenSelected(0.55, 0.25),
      },
    });
    map.addLayer({
      id: "roof-buildings-line", type: "line", source: "roof-buildings",
      paint: {
        "line-color": whenSelected(picked, candidate),
        "line-width": whenSelected(2.5, 1),
      },
    });
    map.on("click", "roof-buildings-fill", function (e) {
      if (!e.features || !e.features.length) return;
      var p = e.features[0].properties || {};
      selectBuilding(p.building_id || e.features[0].id);
    });
    map.on("mouseenter", "roof-buildings-fill", function () {
      map.getCanvas().style.cursor = "pointer";
    });
    map.on("mouseleave", "roof-buildings-fill", function () {
      map.getCanvas().style.cursor = "";
    });
  }

  // [[west, south], [east, north]] around the buildings someone would actually
  // pick — the nearby ones the list also shows — plus the site pin. Fitting
  // the whole search radius leaves every footprint a few pixels wide.
  function buildingsBounds(features, siteLat, siteLon) {
    var west = null, south = null, east = null, north = null;
    function extend(lon, lat) {
      if (typeof lon !== "number" || typeof lat !== "number") return;
      if (west === null || lon < west) west = lon;
      if (east === null || lon > east) east = lon;
      if (south === null || lat < south) south = lat;
      if (north === null || lat > north) north = lat;
    }
    extend(siteLon, siteLat);
    var near = features.filter(function (f) {
      return ((f.properties || {}).distance_m || 0) <= 150;
    });
    if (near.length < 3) near = features;
    near.forEach(function (f) {
      var rings = (f.geometry && f.geometry.coordinates) || [];
      (rings[0] || []).forEach(function (pt) { extend(pt[0], pt[1]); });
    });
    if (west === null) return null;
    return [[west, south], [east, north]];
  }

  // The picker opens at city zoom, where a footprint is smaller than a pixel.
  // Zoom to the search results once per find; selection redraws leave the
  // camera where the operator put it.
  function fitToBuildings(siteLat, siteLon) {
    var map = window._weatherMap;
    if (!map || !roofState.features.length) return;
    var bounds = buildingsBounds(roofState.features, siteLat, siteLon);
    if (!bounds) return;
    map.fitBounds(bounds, { padding: 48, maxZoom: 17.5, duration: 600 });
  }

  function selectBuilding(id) {
    roofState.selectedId = id;
    // A picked building and a drawn footprint answer the same question;
    // the newest answer wins.
    roofState.drawnFootprint = null;
    drawBuildings();
    var list = document.getElementById("roof-buildings");
    if (list) {
      Array.prototype.forEach.call(list.querySelectorAll("[data-building]"), function (el) {
        var on = el.getAttribute("data-building") === id;
        el.style.borderColor = on ? "var(--accent-e)" : "var(--line)";
        el.style.background = on ? "var(--ink-sunken)" : "transparent";
      });
    }
    var derive = document.getElementById("roof-derive");
    if (derive) derive.disabled = !id;
  }

  // Also rendered as a list, so the picker still works where the map does not
  // (no WebGL, blocked CDN).
  function renderBuildingList(ctx) {
    var host = document.getElementById("roof-buildings");
    if (!host) return;
    var esc = ctx.escHtml;
    host.innerHTML = roofState.features.slice(0, 12).map(function (f) {
      var p = f.properties || {};
      var id = f.id || p.building_id;
      return '<button type="button" data-building="' + esc(String(id)) + '" ' +
        'style="display:block;width:100%;text-align:left;font-family:var(--mono);' +
        'font-size:0.72rem;border:1px solid var(--line);border-radius:6px;' +
        'padding:5px 8px;margin:3px 0;background:transparent;color:inherit;cursor:pointer">' +
        Math.round(p.area_m2 || 0) + " m² · " + Math.round(p.distance_m || 0) + " m away" +
        '</button>';
    }).join("");
    Array.prototype.forEach.call(host.querySelectorAll("[data-building]"), function (el) {
      el.addEventListener("click", function () {
        selectBuilding(el.getAttribute("data-building"));
      });
    });
  }

  function deriveRoof(ctx) {
    if (!roofState.selectedId && !roofState.drawnFootprint) return;
    roofSay("Reading the laser scan and fitting roof planes… this can take a minute.");
    // Send the same coordinates the building search used — the live form
    // state, saved or not. Deriving against the stored site while the pin
    // has moved makes the picked id "not found near this site".
    var w = (ctx.config && ctx.config.weather) || {};
    var payload = {};
    if (roofState.selectedId) payload.building_id = roofState.selectedId;
    else payload.footprint = roofState.drawnFootprint;
    var lat = parseFloat(w.latitude), lon = parseFloat(w.longitude);
    if (isFinite(lat) && isFinite(lon)) {
      payload.latitude = lat;
      payload.longitude = lon;
    }
    fetch("/api/roofmodel/derive", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    })
      .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
      .then(function (res) {
        var d = res.d || {};
        if (!res.ok || d.error || d.enabled === false) {
          roofSay(ctx.escHtml(d.error || "the derive failed"), "bad");
          return;
        }
        var arrays = d.proposed_arrays || [];
        if (!arrays.length) {
          roofSay("No roof faces worth mounting panels on were found on that building. " +
                  "North-facing and very small faces are dropped.", "bad");
          return;
        }
        // Fill the form rather than saving: the operator sees the numbers and
        // presses Save, so the panel config never changes behind their back.
        ctx.config.weather.pv_arrays = arrays.map(function (a) {
          var rated = Number(a.rated_w);
          if (!(rated > 0) && Number(a.kwp) > 0) {
            rated = ratedWattsFromLegacyKwp(a.kwp);
          }
          return {
            name: a.name || "", rated_w: rated || 0,
            tilt_deg: a.tilt_deg, azimuth_deg: a.azimuth_deg,
          };
        });
        renderPVArrays(ctx);
        var m = d.model || {};
        var when = m.captured_at_ms
          ? new Date(m.captured_at_ms).toISOString().slice(0, 10)
          : "date unknown";
        var shade = m.shading && m.shading.evaluated ? ", shading evaluated" : "";
        roofSay("Filled in " + arrays.length + " array(s) from " + m.planes_found +
                " roof plane(s). Laser data from " + ctx.escHtml(when) + shade +
                ". <strong>Review them and press Save.</strong>");
      })
      .catch(function (e) { roofSay(ctx.escHtml(String(e && e.message || e)), "bad"); });
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
      var n = config.weather.pv_arrays.length;
      return '<fieldset><legend>Weather forecast &amp; PV</legend>' +
        selectField("Provider", "weather.provider", ["met_no", "openweather", "open_meteo", "forecast_solar", "none"], "met_no",
          "met_no + openweather: cloud-cover only. open_meteo: direct shortwave radiation (better day-one forecast). forecast_solar: site-calibrated watts. The production pattern is learned from measured solar.") +
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
        '<p id="pv-arrays-summary" style="color:var(--text-dim);font-size:0.8rem;margin:8px 0">' +
        arraysSummary(n) + '</p>' +
        '<details class="engine-details" id="pv-arrays-advanced">' +
        '<summary>Advanced array geometry — leave this unless you are debugging.</summary>' +
        '<fieldset><legend>PV arrays ' + help(
          'Optional. Open-Meteo uses these per-plane values to project shortwave radiation onto each array. ' +
          'Forecast.Solar uses them for its site-calibrated forecast. Leave empty unless you are debugging a multi-plane site.') + '</legend>' +
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
        'Tilt: 0° = flat roof, 35° = typical pitched roof, 90° = wall. Azimuth: 0 = N, 90 = E, 180 = S, 270 = W. ' +
        'Rated (W) is watts, same unit as PV rated.' +
        '</p>' +
        '</fieldset></details>' +
        roofFieldset(ctx);
    },
    after: function (ctx) {
      initWeatherMap(ctx);
      renderPVArrays(ctx);
      refreshArraysSummary(ctx.config);
      // The map may fail (no WebGL, CDN blocked); coverage must still render,
      // so drive it from the numeric fields rather than from the map.
      var w = (ctx.config && ctx.config.weather) || {};
      renderCoverage(ctx, w.latitude, w.longitude);
      var addBtn = document.getElementById("pv-array-add");
      if (addBtn) addBtn.addEventListener("click", function () {
        ctx.config.weather.pv_arrays.push({ name: "", rated_w: 0, tilt_deg: 35, azimuth_deg: 180 });
        renderPVArrays(ctx);
        refreshArraysSummary(ctx.config);
      });
      roofState = { features: [], selectedId: null, drawnFootprint: null };
      var catalogSel = document.getElementById("roof-catalog");
      if (catalogSel) catalogSel.addEventListener("change", function () {
        var chosen = null;
        for (var i = 0; i < STAC_CATALOGS.length; i++) {
          if (STAC_CATALOGS[i].key === catalogSel.value) chosen = STAC_CATALOGS[i];
        }
        if (!chosen) return;
        roofCustomCatalog = chosen.key === "custom";
        // Keep everything already typed on this tab, then swap the catalog
        // fields to the preset. Custom leaves the operator's values alone —
        // it only reveals the fields.
        ctx.captureCurrentTab();
        if (!ctx.config.roofmodel) ctx.config.roofmodel = {};
        if (chosen.base !== null) {
          ctx.setByPath(ctx.config, "roofmodel.stac_base_url", chosen.base);
          ctx.setByPath(ctx.config, "roofmodel.stac_buildings_collection", chosen.buildings || "");
          ctx.setByPath(ctx.config, "roofmodel.stac_lidar_collection", chosen.lidar || "");
        }
        // Re-render so the fields, labels and attribution follow the pick.
        var active = document.querySelector("#settings-tabs button.active");
        if (ctx.renderTab && active) ctx.renderTab(active.dataset.tab);
      });
      var findBtn = document.getElementById("roof-find");
      if (findBtn) findBtn.addEventListener("click", function () { findBuildings(ctx); });
      var footprintBtn = document.getElementById("roof-draw-footprint");
      if (footprintBtn) footprintBtn.addEventListener("click", function () { startFootprintDrawing(ctx); });
      var deriveBtn = document.getElementById("roof-derive");
      if (deriveBtn) deriveBtn.addEventListener("click", function () { deriveRoof(ctx); });
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

  S.tabs.weather._pure = {
    arraysSummary: arraysSummary,
    whenSelected: whenSelected,
    buildingsBounds: buildingsBounds,
  };
})();
