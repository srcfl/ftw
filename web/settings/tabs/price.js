// Settings → Price tab: spot price provider + zone + grid tariff + VAT.
//
// The zone list comes from /api/prices/zones, the same table the fetchers
// use, so the picker can't offer a zone the providers don't know. Country
// first, then zone — most countries have one, and nobody knows off-hand
// that Belgium's area code is "BE" while Italy has six.
//
// provider=static skips the zone picker: the operator types a flat rate
// or a time-of-use schedule, which is how every non-European residential
// tariff actually works.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // Filled by the first render that reaches the API; kept across renders so
  // switching tabs doesn't re-fetch. The Nordic fallback is what shipped
  // before the catalog existed — enough to keep the tab usable offline.
  var zones = null;
  var FALLBACK = [
    { code: "SE1", country: "Sweden", currency: "SEK", name: "Sweden — Luleå" },
    { code: "SE2", country: "Sweden", currency: "SEK", name: "Sweden — Sundsvall" },
    { code: "SE3", country: "Sweden", currency: "SEK", name: "Sweden — Stockholm" },
    { code: "SE4", country: "Sweden", currency: "SEK", name: "Sweden — Malmö" },
  ];

  var CURRENCIES = ["EUR", "SEK", "NOK", "DKK", "PLN", "CZK", "HUF", "RON", "CHF", "USD", "GBP", "AUD", "CAD"];

  // elprisetjustnu is a Swedish service and 404s for anything else, so it
  // gets the Swedish zones and nothing more. static is not a bidding-zone
  // feed, so the picker is hidden rather than emptied.
  function zonesFor(provider) {
    var all = zones || FALLBACK;
    if (provider === "elprisetjustnu") {
      return all.filter(function (z) { return z.code.indexOf("SE") === 0; });
    }
    return all;
  }

  function countriesFor(provider) {
    var seen = {}, out = [];
    zonesFor(provider).forEach(function (z) {
      if (!seen[z.country]) { seen[z.country] = true; out.push(z.country); }
    });
    return out;
  }

  function zoneByCode(code) {
    var all = zones || FALLBACK;
    for (var i = 0; i < all.length; i++) {
      if (all[i].code === code) return all[i];
    }
    return null;
  }

  function opt(value, label, selected) {
    return '<option value="' + value + '"' + (selected ? ' selected' : '') + '>' + label + '</option>';
  }

  function touEditor(ctx, unit) {
    var escHtml = ctx.escHtml;
    var windows = (ctx.config.price && ctx.config.price.static_tou) || [];
    var rows = windows.map(function (w, i) {
      return '<div class="field-row" style="gap:8px;align-items:flex-end;margin:4px 0">' +
        '<div><label>Start</label>' +
          '<input type="text" data-tou="' + i + '" data-field="start" value="' + escHtml(w.start || "") + '" placeholder="07:00">' +
        '</div>' +
        '<div><label>End</label>' +
          '<input type="text" data-tou="' + i + '" data-field="end" value="' + escHtml(w.end || "") + '" placeholder="23:00">' +
        '</div>' +
        '<div style="flex:1"><label>' + escHtml(unit) + '/kWh</label>' +
          '<input type="number" step="0.1" data-tou="' + i + '" data-field="ore_kwh" value="' + (w.ore_kwh || 0) + '">' +
        '</div>' +
        '<div style="flex:1.2"><label>Days (blank = every)</label>' +
          '<input type="text" data-tou="' + i + '" data-field="days" value="' + escHtml((w.days || []).join(", ")) + '" placeholder="mon, tue, wed">' +
        '</div>' +
        '<button class="btn-remove" data-tou-remove="' + i + '" type="button" title="Remove">✕</button>' +
      '</div>';
    }).join("");
    return '<details class="engine-details" id="static-tou"' + (windows.length ? " open" : "") + '>' +
      '<summary>Time-of-use windows — leave empty for a flat rate.</summary>' +
      '<p style="color:var(--text-dim);font-size:0.75rem;margin:4px 0 8px">' +
      'Local time on the box. First matching window wins; hours they miss keep the flat rate. ' +
      'Overnight windows wrap (22:00–06:00). Days are weekday names; blank means every day.</p>' +
      '<div id="static-tou-list">' + rows + '</div>' +
      '<button class="btn-add" id="static-tou-add" type="button">+ Add window</button>' +
      '</details>';
  }

  function bindTOU(ctx) {
    var host = document.getElementById("static-tou-list");
    if (host) {
      host.oninput = function (e) {
        var idx = e.target && e.target.dataset && e.target.dataset.tou;
        if (idx == null || idx === "") return;
        var fieldName = e.target.dataset.field;
        var arr = ctx.config.price.static_tou;
        if (!arr || !arr[idx]) return;
        if (fieldName === "ore_kwh") {
          var v = parseFloat(e.target.value);
          if (!isNaN(v)) arr[idx][fieldName] = v;
        } else if (fieldName === "days") {
          arr[idx].days = e.target.value.split(/[,\s]+/).filter(Boolean);
        } else {
          arr[idx][fieldName] = e.target.value;
        }
      };
      host.onclick = function (e) {
        var idx = e.target && e.target.dataset && e.target.dataset.touRemove;
        if (idx == null || idx === "") return;
        ctx.config.price.static_tou.splice(parseInt(idx, 10), 1);
        ctx.captureCurrentTab();
        ctx.renderTab("price");
      };
    }
    var add = document.getElementById("static-tou-add");
    if (add) add.addEventListener("click", function () {
      if (!ctx.config.price.static_tou) ctx.config.price.static_tou = [];
      ctx.config.price.static_tou.push({ start: "07:00", end: "23:00", ore_kwh: 0, days: [] });
      ctx.captureCurrentTab();
      ctx.renderTab("price");
    });
  }

  S.tabs.price = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField, escHtml = ctx.escHtml;
      if (!ctx.config.price) ctx.config.price = {};
      var cfg = ctx.config.price;
      var provider = cfg.provider || "sourceful";
      var isStatic = provider === "static";
      var zoneCode = cfg.zone || (isStatic ? "STATIC" : "SE3");
      // Switching to a Sweden-only provider while sitting on a foreign
      // zone would leave both selects empty, so the zone follows the
      // provider back into range. STATIC is not a bidding zone.
      var available = zonesFor(provider);
      if (!isStatic) {
        if (zoneCode === "STATIC" || !available.some(function (z) { return z.code === zoneCode; })) {
          if (available.length) {
            zoneCode = available[0].code;
            ctx.setByPath(ctx.config, "price.zone", zoneCode);
          }
        }
      } else if (!cfg.zone) {
        ctx.setByPath(ctx.config, "price.zone", "STATIC");
      }
      var zone = zoneByCode(zoneCode);
      var country = zone ? zone.country : "Sweden";
      var currency = cfg.currency || (isStatic ? "SEK" : (zone ? zone.currency : "SEK"));
      var unit = window.FTWUnits ? window.FTWUnits.unitLabel(currency) : "öre";

      var countryOpts = countriesFor(provider).map(function (c) {
        return opt(escHtml(c), escHtml(c), c === country);
      }).join("");
      var inCountry = zonesFor(provider).filter(function (z) { return z.country === country; });
      var zoneOpts = inCountry.map(function (z) {
        var sub = z.name && z.name.indexOf(" — ") > 0 ? z.name.split(" — ")[1] : "";
        return opt(escHtml(z.code), escHtml(sub ? z.code + " · " + sub : z.code), z.code === zoneCode);
      }).join("");

      var zoneFields = isStatic ? "" :
        '<label>Country</label>' +
        '<select id="price-country">' + countryOpts + '</select>' +
        '<label>Bidding zone</label>' +
        '<select id="price-zone-select" data-path="price.zone">' + zoneOpts + '</select>';

      var staticFields = !isStatic ? "" :
        field("Energy price (" + escHtml(unit) + "/kWh)", "price.static_ore_kwh", "number", 0,
          "The commodity price your retailer charges, in the same minor units as the grid tariff. " +
          "Time-of-use windows below override matching hours.") +
        touEditor(ctx, unit);

      var currencyHelp = isStatic
        ? "The currency your bill is in. Static prices are already in this currency — nothing is converted."
        : "Prices are converted to this currency with ECB daily rates. It follows the country you pick; " +
          "change it only if you are billed in something else. Switching it clears cached prices.";

      return '<fieldset><legend>Spot price</legend>' +
        selectField("Provider", "price.provider", ["sourceful", "elprisetjustnu", "entsoe", "static", "none"], "sourceful") +
        zoneFields +
        selectField("Currency", "price.currency", CURRENCIES, currency, currencyHelp) +
        staticFields +
        '<div class="field-row"><div>' +
        field("Grid tariff excl. VAT (" + escHtml(unit) + "/kWh)", "price.grid_tariff_ore_kwh", "number", 60,
          "Per-kWh network/distribution fee from your DSO, excluding VAT. This is the cost of moving " +
          "electricity over the wire, independent of the spot price.") +
        '</div><div>' +
        field("VAT (%)", "price.vat_percent", "number", 25) +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Export bonus (" + escHtml(unit) + "/kWh)", "price.export_bonus_ore_kwh", "number", 0) +
        '</div><div>' +
        field("Export fee (" + escHtml(unit) + "/kWh)", "price.export_fee_ore_kwh", "number", 0) +
        '</div></div>' +
        '<p id="tariff-warning" class="tariff-warning" style="display:none">' +
        '⚠ Grid tariff below ~60 öre/kWh (0.06 €/kWh) is unusually low. ' +
        'Underestimating it will make the MPC planner over-charge from the grid — you may lose money. ' +
        'Include DSO transmission fee + any fixed taxes.</p>' +
        '<p id="country-note" class="tariff-warning" style="display:none">' +
        '⚠ Grid tariff and VAT still hold their Swedish defaults. Set both for your own country — ' +
        'the planner prices every decision with them.</p>' +
        (isStatic ? "" : field("API key (ENTSO-E only)", "price.api_key", "text", "")) +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        (isStatic
          ? "Static is a flat rate or time-of-use schedule you type yourself. It is the way to run " +
            "price-driven planning outside Europe, or on any contract that is not day-ahead spot. " +
            "Grid tariff and VAT still apply on top."
          : "Sourceful is the keyless default and covers every European bidding zone ENTSO-E publishes. " +
            "elprisetjustnu.se remains a Sweden-only alternative. Direct ENTSO-E access needs an API key. " +
            "Outside Europe, pick static and enter your tariff. FX rates come from ECB daily.") +
        '</p>';
    },

    after: function (ctx) {
      var bodyEl = ctx.bodyEl;

      if (!zones) {
        ctx.apiFetch("/api/prices/zones")
          .then(function (r) { return r.json(); })
          .then(function (j) {
            if (!j || !Array.isArray(j.zones) || !j.zones.length) return;
            zones = j.zones;
            ctx.captureCurrentTab();
            ctx.renderTab("price");
          })
          .catch(function () { /* fallback list stays */ });
      }

      var countryEl = bodyEl.querySelector("#price-country");
      var zoneEl = bodyEl.querySelector("#price-zone-select");
      var providerEl = bodyEl.querySelector('[data-path="price.provider"]');
      var currencyEl = bodyEl.querySelector('[data-path="price.currency"]');

      function rerender(override) {
        ctx.captureCurrentTab();
        if (override) override();
        ctx.renderTab("price");
      }

      if (countryEl) {
        countryEl.addEventListener("change", function () {
          var first = (zones || FALLBACK).filter(function (z) {
            return z.country === countryEl.value;
          })[0];
          rerender(function () {
            if (!first) return;
            ctx.setByPath(ctx.config, "price.zone", first.code);
            ctx.setByPath(ctx.config, "price.currency", first.currency);
          });
        });
      }
      if (zoneEl) {
        zoneEl.addEventListener("change", function () {
          var z = zoneByCode(zoneEl.value);
          rerender(function () {
            if (z) ctx.setByPath(ctx.config, "price.currency", z.currency);
          });
        });
      }
      if (providerEl) {
        providerEl.addEventListener("change", function () {
          rerender(function () {
            if (providerEl.value === "static") {
              ctx.setByPath(ctx.config, "price.zone", "STATIC");
            }
          });
        });
      }
      if (currencyEl) currencyEl.addEventListener("change", function () { rerender(); });

      bindTOU(ctx);

      var input = bodyEl.querySelector('[data-path="price.grid_tariff_ore_kwh"]');
      var warn = bodyEl.querySelector("#tariff-warning");
      var note = bodyEl.querySelector("#country-note");
      var currency = (ctx.config.price && ctx.config.price.currency) || "SEK";
      var vat = ctx.getByPath(ctx.config, "price.vat_percent", 25);
      var provider = (ctx.config.price && ctx.config.price.provider) || "sourceful";
      if (note) {
        note.style.display = (provider === "static" || currency !== "SEK") && Number(vat) === 25 ? "block" : "none";
      }
      if (!input || !warn) return;
      function check() {
        var v = parseFloat(input.value);
        var low = currency === "SEK" && !isNaN(v) && v < 60;
        warn.style.display = low ? "block" : "none";
        input.classList.toggle("field-warn", low);
      }
      input.addEventListener("input", check);
      check();
    },
  };
})();
