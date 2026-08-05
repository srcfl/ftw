// Settings → Price tab: spot price provider + zone + grid tariff + VAT.
//
// The zone list comes from /api/prices/zones, the same table the fetchers
// use, so the picker can't offer a zone the providers don't know. Country
// first, then zone — most countries have one, and nobody knows off-hand
// that Belgium's area code is "BE" while Italy has six.
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

  // elprisetjustnu is a Swedish service and 404s for anything else, so it
  // gets the Swedish zones and nothing more.
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

  S.tabs.price = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField, escHtml = ctx.escHtml;
      if (!ctx.config.price) ctx.config.price = {};
      var cfg = ctx.config.price;
      var provider = cfg.provider || "sourceful";
      var zoneCode = cfg.zone || "SE3";
      // Switching to a Sweden-only provider while sitting on a foreign
      // zone would leave both selects empty, so the zone follows the
      // provider back into range.
      var available = zonesFor(provider);
      if (!available.some(function (z) { return z.code === zoneCode; }) && available.length) {
        zoneCode = available[0].code;
        ctx.setByPath(ctx.config, "price.zone", zoneCode);
      }
      var zone = zoneByCode(zoneCode);
      var country = zone ? zone.country : "Sweden";
      var currency = cfg.currency || (zone ? zone.currency : "SEK");
      var unit = window.FTWUnits ? window.FTWUnits.unitLabel(currency) : "öre";

      var countryOpts = countriesFor(provider).map(function (c) {
        return opt(escHtml(c), escHtml(c), c === country);
      }).join("");
      var inCountry = zonesFor(provider).filter(function (z) { return z.country === country; });
      var zoneOpts = inCountry.map(function (z) {
        // "Sweden — Stockholm" under the Sweden heading reads as
        // "SE3 · Stockholm"; a country with one zone is just its code.
        var sub = z.name && z.name.indexOf(" — ") > 0 ? z.name.split(" — ")[1] : "";
        return opt(escHtml(z.code), escHtml(sub ? z.code + " · " + sub : z.code), z.code === zoneCode);
      }).join("");

      return '<fieldset><legend>Spot price</legend>' +
        selectField("Provider", "price.provider", ["sourceful", "elprisetjustnu", "entsoe", "none"], "sourceful") +
        '<label>Country</label>' +
        '<select id="price-country">' + countryOpts + '</select>' +
        '<label>Bidding zone</label>' +
        '<select id="price-zone-select" data-path="price.zone">' + zoneOpts + '</select>' +
        selectField("Currency", "price.currency",
          ["EUR", "SEK", "NOK", "DKK", "PLN", "CZK", "HUF", "RON", "CHF"], currency,
          "Prices are converted to this currency with ECB daily rates. It follows the country you pick; " +
          "change it only if you are billed in something else. Switching it clears cached prices.") +
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
        field("API key (ENTSO-E only)", "price.api_key", "text", "") +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'Sourceful is the keyless default and covers every European bidding zone ENTSO-E publishes. ' +
        'elprisetjustnu.se remains a Sweden-only alternative. Direct ENTSO-E access needs an API key. ' +
        'FX rates come from ECB daily.' +
        '</p>';
    },

    after: function (ctx) {
      var bodyEl = ctx.bodyEl;

      // Load the catalog once, then re-render so the picker shows every
      // zone rather than the Nordic fallback.
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

      // Changing country or provider changes which zones exist, so both
      // re-render the tab rather than patching the options in place.
      // Capture first, then override: captureCurrentTab reads the visible
      // selects, so anything set before it would be written straight back
      // to the old value.
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
            // The currency follows the country — picking Belgium should
            // not leave the household paying in öre.
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
      if (providerEl) providerEl.addEventListener("change", function () { rerender(); });
      if (currencyEl) currencyEl.addEventListener("change", function () { rerender(); });

      // The 60 öre floor is a Swedish number. Outside SEK we can't name a
      // threshold without inventing one, so the note replaces the warning.
      var input = bodyEl.querySelector('[data-path="price.grid_tariff_ore_kwh"]');
      var warn = bodyEl.querySelector("#tariff-warning");
      var note = bodyEl.querySelector("#country-note");
      var currency = (ctx.config.price && ctx.config.price.currency) || "SEK";
      var vat = ctx.getByPath(ctx.config, "price.vat_percent", 25);
      if (note) {
        note.style.display = currency !== "SEK" && Number(vat) === 25 ? "block" : "none";
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
