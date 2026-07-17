// Settings → Price tab: spot price provider + grid tariff + VAT.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  S.tabs.price = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField;
      if (!ctx.config.price) ctx.config.price = {};
      return '<fieldset><legend>Spot price</legend>' +
        selectField("Provider", "price.provider", ["sourceful", "elprisetjustnu", "entsoe", "none"], "sourceful") +
        selectField("Zone", "price.zone", ["SE1", "SE2", "SE3", "SE4", "NO1", "NO2", "NO3", "NO4", "NO5", "DK1", "DK2", "FI", "DE"], "SE3") +
        selectField("Currency", "price.currency", ["SEK", "NOK", "DKK", "EUR"], "SEK") +
        '<div class="field-row"><div>' +
        field("Grid tariff excl. VAT (öre/kWh)", "price.grid_tariff_ore_kwh", "number", 60,
          "Per-kWh network/distribution fee from your DSO (elnätsavgift), excluding VAT. This is the cost of moving electricity over the wire, independent of the spot price.") +
        '</div><div>' +
        field("VAT (%)", "price.vat_percent", "number", 25) +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Export bonus (öre/kWh)", "price.export_bonus_ore_kwh", "number", 0) +
        '</div><div>' +
        field("Export fee (öre/kWh)", "price.export_fee_ore_kwh", "number", 0) +
        '</div></div>' +
        '<p id="tariff-warning" class="tariff-warning" style="display:none">' +
        '⚠ Grid tariff below ~60 öre/kWh (0.06 €/kWh) is unusually low. ' +
        'Underestimating it will make the MPC planner over-charge from the grid — you may lose money. ' +
        'Include DSO transmission fee + any fixed taxes.</p>' +
        field("API key (ENTSO-E only)", "price.api_key", "text", "") +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'Sourceful is the keyless default and covers European bidding zones. elprisetjustnu.se remains a Sweden-only alternative. ' +
        'Direct ENTSO-E access needs an API key. Currency applies to ENTSO-E only; FX rates come from ECB daily.' +
        '</p>';
    },
    after: function (ctx) {
      var input = ctx.bodyEl.querySelector('[data-path="price.grid_tariff_ore_kwh"]');
      var warn = document.getElementById('tariff-warning');
      if (!input || !warn) return;
      function check() {
        var v = parseFloat(input.value);
        if (!isNaN(v) && v < 60) {
          warn.style.display = 'block';
          input.classList.add('field-warn');
        } else {
          warn.style.display = 'none';
          input.classList.remove('field-warn');
        }
      }
      input.addEventListener('input', check);
      check();
    },
  };
})();
