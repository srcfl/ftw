// plan.js — MPC plan + prices + forecast visualization.
// Renders a stacked canvas chart: price bars on top, battery+grid bars in
// the middle, SoC + PV line on bottom. Refreshes every 30s.

(function () {
  'use strict';

  const PLAN_REFRESH_MS = 30000;

  // ownerFetch routes owner API calls over the STRICT P2P transport on the public
  // home route. Wired in p2p.js to the shared fail-closed strict function; falls
  // back to plain fetch only where p2p.js never loaded (genuine LAN / tests).
  function ownerFetch(path, opts) {
    if (typeof window.ownerFetch === 'function') return window.ownerFetch(path, opts);
    return fetch(path, opts);
  }

  function escapeHTML(value) {
    return String(value)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // Horizon controls the x-axis bounds; mirrors the price chart's
  // 3-position pill so operators have a consistent affordance across
  // both charts. Persisted in localStorage so a user who prefers
  // "Today only" doesn't have to re-pick on every reload.
  //
  // Defined ABOVE `state` because state's initializer calls
  // readHorizonPref(); even though function declarations hoist, the
  // const HORIZON_PREF_KEY would be in its temporal dead zone at that
  // point and the module would throw a ReferenceError.
  const HORIZON_PREF_KEY = "ftw.planChart.horizon";
  function readHorizonPref() {
    try {
      const v = localStorage.getItem(HORIZON_PREF_KEY);
      return (v === "today" || v === "all" || v === "tomorrow") ? v : "all";
    } catch (e) { return "all"; }
  }

  const state = {
    prices: null,
    forecast: null,
    plan: null,
    fuse: null,         // { max_amps, phases, voltage } — drives the power y-axis
    lastUpdate: null,
    horizon: readHorizonPref(),  // "today" | "all" | "tomorrow"
  };
  function writeHorizonPref(v) {
    try { localStorage.setItem(HORIZON_PREF_KEY, v); } catch (e) {}
  }
  function localMidnight(offsetDays) {
    const d = new Date();
    d.setHours(0, 0, 0, 0);
    return d.getTime() + (offsetDays || 0) * 24 * 60 * 60 * 1000;
  }
  function horizonBounds(horizon) {
    const now = Date.now();
    if (horizon === "today") {
      return { tMin: Math.max(localMidnight(0), now - 30 * 60 * 1000), tMax: localMidnight(1) };
    }
    if (horizon === "tomorrow") {
      return { tMin: localMidnight(1), tMax: localMidnight(2) };
    }
    // "all" — current default: now-30 min through next 48 h.
    return { tMin: now - 30 * 60 * 1000, tMax: now + 48 * 60 * 60 * 1000 };
  }
  function chartTickStepMs(tMin, tMax) {
    const span = Math.max(1, tMax - tMin);
    if (span <= 26 * 3600 * 1000) return 6 * 3600 * 1000;
    if (span <= 54 * 3600 * 1000) return 12 * 3600 * 1000;
    return 24 * 3600 * 1000;
  }
  function firstChartTick(tMin, stepMs) {
    const d = new Date(tMin);
    d.setMinutes(0, 0, 0);
    const stepHours = Math.max(1, Math.round(stepMs / 3600000));
    const hour = d.getHours();
    const addHours = (stepHours - (hour % stepHours)) % stepHours;
    if (addHours > 0 || d.getTime() < tMin) d.setHours(hour + addHours, 0, 0, 0);
    return d.getTime();
  }

  async function fetchAll() {
    const [p, f, m, c] = await Promise.all([
      ownerFetch('/api/prices').then(r => r.json()).catch(() => ({})),
      ownerFetch('/api/forecast').then(r => r.json()).catch(() => ({})),
      ownerFetch('/api/mpc/plan').then(r => r.json()).catch(() => ({})),
      ownerFetch('/api/config').then(r => r.json()).catch(() => ({})),
    ]);
    state.prices = (p && p.items) || [];
    state.forecast = (f && f.items) || [];
    state.plan = (m && m.plan) || null;
    state.planMeta = (m && m.meta) || null;
    state.fuse = (c && c.fuse) || null;
    // Tariff breakdown pulled from /api/config so the price bars can be
    // stacked as spot + grid tariff + VAT instead of one opaque number.
    state.priceCfg = (c && c.price) || null;
    state.enabled = {
      prices: p && p.enabled,
      forecast: f && f.enabled,
      mpc: m && m.enabled,
    };
    state.lastUpdate = new Date();
    render();
  }

  async function replan() {
    try {
      const r = await ownerFetch('/api/mpc/replan', { method: 'POST' });
      const j = await r.json();
      if (j && j.plan) state.plan = j.plan;
      render();
    } catch (e) { /* ignore */ }
  }

  function fmtHHMM(ts) {
    const d = new Date(ts);
    return d.getHours().toString().padStart(2, '0') + ':' +
           d.getMinutes().toString().padStart(2, '0');
  }

  function render() {
    const canvas = document.getElementById('plan-chart');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    const cssW = canvas.clientWidth || 800;
    const cssH = 320;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = cssW * dpr;
    canvas.height = cssH * dpr;
    canvas.style.height = cssH + 'px';
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    const pad = { l: 44, r: 44, t: 16, b: 28 };
    const plotW = cssW - pad.l - pad.r;
    const plotH = cssH - pad.t - pad.b;

    // X range — driven by the operator-chosen horizon (today / +tomorrow
    // / tomorrow only). "all" preserves the original now→+48h default.
    const now = Date.now();
    const { tMin, tMax } = horizonBounds(state.horizon);
    const xScale = t => pad.l + (t - tMin) / (tMax - tMin) * plotW;

    // Layout: price bars (top) | mode band (thin strip) | power bars (middle) | SoC (bottom)
    const modeBandH = 10;

    // Price range
    const prices = (state.prices || []).filter(p => p.slot_ts_ms >= tMin && p.slot_ts_ms <= tMax);
    const totals = prices.map(p => p.total_ore_kwh);
    const priceMin = totals.length ? Math.min(0, ...totals) : 0;
    const priceMax = totals.length ? Math.max(...totals, 1) : 200;
    const priceRange = priceMax - priceMin;

    // Price band on top
    const priceY0 = pad.t;
    const priceH = plotH * 0.29;
    const priceY = v => priceY0 + priceH - (v - priceMin) / priceRange * priceH;

    // Mode band — thin strip below price bars showing which EMS mode
    // is active per slot. Color-coded so operators see the schedule at a
    // glance without reading per-slot tooltips.
    const modeBandY0 = priceY0 + priceH + 2;

    // Power band in middle — covers battery + grid.
    // Several later sections ("Plan battery bars", "Load forecast",
    // predicted-zone shade, etc.) reference `plan` directly. Keep this
    // alias — removing it leaves those `plan` references undefined and
    // the whole render throws, wiping the chart.
    const plan = state.plan;
    const powerY0 = modeBandY0 + modeBandH + 4;
    const powerH = plotH * 0.42;
    // Scale off the fuse (what the site can *physically* deliver) plus a
    // 15% headroom so peak transients don't clip. e.g. 16 A × 3 φ × 230 V
    // ≈ 11 kW → y-axis spans ±12.7 kW. A fixed scale makes it easier to
    // eyeball plan magnitudes across runs instead of re-interpreting the
    // axis every time the max sample changes.
    const fuse = state.fuse || {};
    const fuseMaxW = (fuse.max_amps || 16) * (fuse.phases || 3) * (fuse.voltage || 230);
    let pMagMax = fuseMaxW * 1.15;
    const powerYCenter = powerY0 + powerH / 2;
    const powerY = w => powerYCenter - (w / pMagMax) * (powerH / 2);

    // SoC line on bottom band
    const socY0 = powerY0 + powerH + 4;
    const socH = plotH * 0.18;
    const socY = p => socY0 + socH - (p / 100) * socH;

    // ---- Grid ticks (hours) ----
    ctx.strokeStyle = 'rgba(255,255,255,0.08)';
    ctx.lineWidth = 1;
    ctx.fillStyle = 'rgba(255,255,255,0.45)';
    ctx.font = '11px system-ui, sans-serif';
    ctx.textAlign = 'center';
    const tickStep = chartTickStepMs(tMin, tMax);
    for (let t = firstChartTick(tMin, tickStep); t <= tMax + 1000; t += tickStep) {
      const x = xScale(t);
      ctx.beginPath();
      ctx.moveTo(x, pad.t);
      ctx.lineTo(x, pad.t + plotH);
      ctx.stroke();
      ctx.fillText(fmtHHMM(t), x, cssH - 10);
    }
    // Now-line
    if (now >= tMin && now <= tMax) {
      const xNow = xScale(now);
      ctx.strokeStyle = '#ef4444';
      ctx.lineWidth = 1.2;
      ctx.setLineDash([3, 3]);
      ctx.beginPath();
      ctx.moveTo(xNow, pad.t);
      ctx.lineTo(xNow, pad.t + plotH);
      ctx.stroke();
      ctx.setLineDash([]);
    }

    // ---- Predicted-zone shade + boundary ----
    // Find the first ML-forecasted action. Everything at or past that
    // point gets a translucent band and a "predicted" label, so the
    // uncertain portion reads as visually different — not just dimmer
    // bars but a whole different region.
    if (plan && plan.actions && plan.actions.length) {
      const firstPred = plan.actions.find(a => a.confidence != null && a.confidence < 1.0);
      if (firstPred) {
        const xPred = Math.max(xScale(firstPred.slot_start_ms), pad.l);
        const xEnd = pad.l + plotW;
        if (xPred < xEnd) {
          // Shaded band behind everything in the plot area — strong
          // enough to read as "this zone is different".
          ctx.fillStyle = 'rgba(251,191,36,0.10)';
          ctx.fillRect(xPred, pad.t, xEnd - xPred, plotH);
          // Boundary line
          ctx.strokeStyle = 'rgba(251,191,36,0.65)';
          ctx.lineWidth = 1.2;
          ctx.setLineDash([4, 4]);
          ctx.beginPath();
          ctx.moveTo(xPred, pad.t);
          ctx.lineTo(xPred, pad.t + plotH);
          ctx.stroke();
          ctx.setLineDash([]);
          // Label "predicted →"
          ctx.fillStyle = 'rgba(251,191,36,0.9)';
          ctx.font = '10px system-ui, sans-serif';
          ctx.textAlign = 'left';
          ctx.fillText('predicted →', xPred + 4, pad.t + 10);
        }
      }
    }

    // ---- Price bars ----
    // Stacked: spot (bottom, tercile-colored) + grid tariff (middle,
    // neutral slate) + VAT (top, lighter slate). Reads grid tariff +
    // VAT % from /api/config so the split matches the real tariff
    // engine (prices.Applier). Spot does all the visual work —
    // it's the part that drives the planner's timing decisions —
    // while the fixed portions stay quiet in neutral slate so they
    // don't distract from the cheap/expensive signal. Falls back to
    // one opaque bar when spot_ore isn't available (legacy price
    // points from before the Action.SpotOre surface, or a tariff
    // engine configured with zero grid tariff + VAT).
    const sortedTotals = [...totals].sort((a, b) => a - b);
    const p25 = sortedTotals[Math.floor(sortedTotals.length * 0.25)] || priceMin;
    const p75 = sortedTotals[Math.floor(sortedTotals.length * 0.75)] || priceMax;
    const gridTariff = (state.priceCfg && state.priceCfg.grid_tariff_ore_kwh) || 0;
    const vatPct     = (state.priceCfg && state.priceCfg.vat_percent) || 0;
    state.priceBarBounds = []; // {x0,x1,yMinPx,yMaxPx, action} for hover hit-test
    const barSource = (plan && plan.actions && plan.actions.length) ? plan.actions : prices;
    for (const bar of barSource) {
      const ts = bar.slot_ts_ms ?? bar.slot_start_ms;
      const len = bar.slot_len_min;
      const priceVal = bar.total_ore_kwh ?? bar.price_ore;
      if (ts == null || priceVal == null) continue;
      if (ts + len * 60 * 1000 < tMin || ts > tMax) continue;
      const x0 = xScale(ts);
      const x1 = xScale(ts + len * 60 * 1000);
      const zero = priceY(Math.max(0, priceMin));
      const isPredicted = bar.confidence != null && bar.confidence < 1.0;
      // Component breakdown. When we have spot_ore AND at least one
      // of the fixed portions is non-zero, stack three segments so
      // the bar reads as a breakdown. Otherwise render a single flat
      // tercile-colored bar (legacy behavior).
      const spotOre = bar.spot_ore ?? bar.spot_ore_kwh ?? null;
      let parts; // [{ore, rgb, alpha, label}] bottom→top
      if (spotOre != null && (gridTariff > 0 || vatPct > 0)) {
        const vatOre = Math.max(0, (spotOre + gridTariff) * (vatPct / 100));
        // Tercile color is applied to the SPOT portion only — that's
        // the number the planner is actually deciding against.
        let spotRgb;
        if (priceVal <= p25) spotRgb = '34,197,94';       // green
        else if (priceVal >= p75) spotRgb = '239,68,68';  // red
        else spotRgb = '148,163,184';                     // slate
        parts = [
          { ore: spotOre,    rgb: spotRgb,       alpha: 0.72, label: 'spot' },
          { ore: gridTariff, rgb: '100,116,139', alpha: 0.45, label: 'grid' },
          { ore: vatOre,     rgb: '100,116,139', alpha: 0.25, label: 'vat' },
        ];
      } else {
        let baseRgb;
        if (priceVal <= p25) baseRgb = '34,197,94';
        else if (priceVal >= p75) baseRgb = '239,68,68';
        else baseRgb = '148,163,184';
        parts = [{ ore: priceVal, rgb: baseRgb, alpha: 0.60, label: 'price' }];
      }
      const rectX = x0;
      const rectW = Math.max(1, x1 - x0 - 1);
      // Stack from zero upward. runningOre accumulates in öre/kWh and
      // we re-project each segment's top edge through priceY so the
      // stacked bar lines up pixel-perfect with the axis grid.
      let runningOre = 0;
      const topY = priceY(priceVal);
      for (const part of parts) {
        if (part.ore <= 0) continue;
        const segBottomY = priceY(runningOre);
        const segTopY    = priceY(runningOre + part.ore);
        const segY = Math.min(segBottomY, segTopY);
        const segH = Math.abs(segBottomY - segTopY);
        const alpha = isPredicted ? part.alpha * 0.2 : part.alpha;
        ctx.fillStyle = `rgba(${part.rgb},${alpha})`;
        ctx.fillRect(rectX, segY, rectW, segH);
        runningOre += part.ore;
      }
      if (isPredicted) {
        // Dashed outline across the whole bar so predicted slots still
        // read as "uncertain ghost" regardless of how it's stacked.
        const outlineRgb = parts[0].rgb;
        ctx.strokeStyle = `rgba(${outlineRgb},0.75)`;
        ctx.lineWidth = 1;
        ctx.setLineDash([3, 3]);
        ctx.strokeRect(rectX + 0.5, Math.min(topY, zero) + 0.5, rectW - 1, Math.abs(topY - zero) - 1);
        ctx.setLineDash([]);
      }
      // Track for hover hit-test.
      state.priceBarBounds.push({
        x0: x0, x1: x1,
        ts: ts, len: len,
        action: bar, // either PricePoint or Action
      });
    }
    // Price axis labels
    ctx.fillStyle = 'rgba(255,255,255,0.55)';
    ctx.textAlign = 'right';
    ctx.fillText(priceMax.toFixed(0) + ' öre', pad.l - 6, priceY0 + 10);
    ctx.fillText(priceMin.toFixed(0), pad.l - 6, priceY0 + priceH);
    ctx.textAlign = 'left';
    ctx.fillText('Price', pad.l + 4, priceY0 + 12);

    // ---- PV line (negative = generation, site sign) ----
    // Prefer the plan's own per-slot pv_w when the optimiser is running
    // — that's the number that drove the charge/idle/discharge
    // decisions, and it's what you want to compare against reality when
    // the battery behaves unexpectedly (e.g. plan says 0.8 kW PV,
    // reality is 4.6 kW, so the battery absorbs the unforecast surplus).
    // Fall back to the raw weather forecast when there's no plan.
    ctx.strokeStyle = 'rgba(34,197,94,0.9)';
    ctx.lineWidth = 2;
    ctx.beginPath();
    let first = true;
    if (plan && plan.actions && plan.actions.length) {
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        if (a.pv_w == null) continue;
        const x = xScale(a.slot_start_ms);
        const y = powerY(a.pv_w); // plan.pv_w is already site-signed
        if (first) { ctx.moveTo(x, y); first = false; }
        else ctx.lineTo(x, y);
      }
    } else {
      for (const f of state.forecast || []) {
        if (f.slot_ts_ms > tMax || !f.pv_w_estimated) continue;
        const x = xScale(f.slot_ts_ms);
        const y = powerY(-f.pv_w_estimated); // flip forecast → site sign
        if (first) { ctx.moveTo(x, y); first = false; }
        else ctx.lineTo(x, y);
      }
    }
    ctx.stroke();

    // Load forecast from the plan's per-slot predictions (twin-driven).
    // Rendered above the PV curve as a pale-yellow dashed line so we can
    // see what the optimizer expects the house to consume each slot.
    if (plan && plan.actions && plan.actions.length) {
      ctx.strokeStyle = '#fde68a';
      ctx.lineWidth = 1.8;
      ctx.setLineDash([4, 5]);
      ctx.beginPath();
      let f2 = true;
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        if (a.load_w == null) continue;
        const x = xScale(a.slot_start_ms);
        const y = powerY(a.load_w);
        if (f2) { ctx.moveTo(x, y); f2 = false; }
        else ctx.lineTo(x, y);
      }
      ctx.stroke();
      ctx.setLineDash([]);
    }

    // Planned EV charging — site-signed load (always ≥ 0, plotted above
    // zero). Solid cyan so it's distinguishable from the dashed amber
    // load forecast and the green PV trace. Only drawn when the plan
    // carries a loadpoint dimension (loadpoint_w field present).
    if (plan && plan.actions && plan.actions.some(a => a.loadpoint_w != null)) {
      ctx.strokeStyle = 'rgba(34,211,238,0.95)';
      ctx.lineWidth = 1.8;
      ctx.beginPath();
      let fEv = true;
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        if (a.loadpoint_w == null) continue;
        const x = xScale(a.slot_start_ms);
        const y = powerY(a.loadpoint_w);
        if (fEv) { ctx.moveTo(x, y); fEv = false; }
        else ctx.lineTo(x, y);
      }
      ctx.stroke();
      // EV step-fill at low opacity makes the on/off slots readable at a
      // glance — the line alone hides the "off between two on" slots.
      ctx.fillStyle = 'rgba(34,211,238,0.12)';
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        if (!a.loadpoint_w || a.loadpoint_w <= 0) continue;
        const x0 = xScale(a.slot_start_ms);
        const x1 = xScale(a.slot_start_ms + a.slot_len_min * 60 * 1000);
        const yTop = powerY(a.loadpoint_w);
        ctx.fillRect(x0, yTop, Math.max(1, x1 - x0 - 1), powerYCenter - yTop);
      }
    }

    // Power zero-line
    ctx.strokeStyle = 'rgba(255,255,255,0.25)';
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(pad.l, powerYCenter);
    ctx.lineTo(pad.l + plotW, powerYCenter);
    ctx.stroke();
    ctx.fillStyle = 'rgba(255,255,255,0.55)';
    ctx.textAlign = 'right';
    ctx.fillText('+' + (pMagMax / 1000).toFixed(1) + 'kW', pad.l - 6, powerY(pMagMax) + 4);
    ctx.fillText('−' + (pMagMax / 1000).toFixed(1) + 'kW', pad.l - 6, powerY(-pMagMax) + 4);
    ctx.textAlign = 'left';
    // "Power" heading + tiny sign-convention legend so readers don't have
    // to remember that positive means "into the site". Placed just below
    // the heading at lower opacity to read as a subtitle.
    ctx.fillText('Power', pad.l + 4, powerY0 + 12);
    ctx.fillStyle = 'rgba(255,255,255,0.35)';
    ctx.font = '9px system-ui, sans-serif';
    ctx.fillText('+ import / charge   − export / discharge', pad.l + 40, powerY0 + 12);
    ctx.font = '11px system-ui, sans-serif';

    // Skip every battery-related draw layer (action band, bars, SoC
    // line + axis labels) when the site has no battery reporter.
    // next-app.js flips body.no-battery on the same /api/status tick
    // that drives this chart, so the two signals stay in sync.
    const noBattery = document.body.classList.contains('no-battery');

    // ---- Battery action band — colored strip showing charge/discharge/idle per slot ----
    if (!noBattery && plan && plan.actions) {
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        const x0 = xScale(a.slot_start_ms);
        const x1 = xScale(a.slot_start_ms + a.slot_len_min * 60 * 1000);
        let color;
        if (a.battery_w > 100)       color = 'rgba(245,158,11,0.6)';   // amber = charging
        else if (a.battery_w < -100) color = 'rgba(139,92,246,0.6)';   // purple = discharging
        else                         color = 'rgba(100,116,139,0.2)';  // slate = idle
        ctx.fillStyle = color;
        ctx.fillRect(x0, modeBandY0, Math.max(1, x1 - x0 - 1), modeBandH);
      }
      ctx.fillStyle = 'rgba(255,255,255,0.45)';
      ctx.font = '9px system-ui, sans-serif';
      ctx.textAlign = 'left';
      ctx.fillText('Battery', pad.l + 4, modeBandY0 + modeBandH - 2);
    }

    // ---- Plan battery bars ----
    if (!noBattery && plan && plan.actions) {
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        const x0 = xScale(a.slot_start_ms);
        const x1 = xScale(a.slot_start_ms + a.slot_len_min * 60 * 1000);
        const y = powerY(a.battery_w);
        const color = a.battery_w >= 0 ? 'rgba(245,158,11,0.65)' : 'rgba(139,92,246,0.65)';
        ctx.fillStyle = color;
        ctx.fillRect(x0, Math.min(y, powerYCenter), Math.max(1, x1 - x0 - 1), Math.abs(y - powerYCenter));
      }
      // SoC line
      ctx.strokeStyle = 'rgba(96,165,250,0.95)';
      ctx.lineWidth = 2;
      ctx.beginPath();
      first = true;
      // Anchor at start SoC at now
      if (plan.initial_soc_pct != null) {
        ctx.moveTo(xScale(now), socY(plan.initial_soc_pct));
        first = false;
      }
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        const x = xScale(a.slot_start_ms + a.slot_len_min * 60 * 1000);
        const y = socY(a.soc_pct);
        if (first) { ctx.moveTo(x, y); first = false; }
        else ctx.lineTo(x, y);
      }
      ctx.stroke();
      // SoC axis labels: right-align flush against the plot's right edge
      // so they read as part of the chart frame instead of floating off
      // in whitespace.
      ctx.fillStyle = 'rgba(255,255,255,0.55)';
      ctx.textAlign = 'right';
      ctx.fillText('100%', cssW - pad.r - 4, socY(100) + 4);
      ctx.fillText('0%',   cssW - pad.r - 4, socY(0)   + 4);
      ctx.textAlign = 'left';
      ctx.fillText('SoC', pad.l + 4, socY0 + 12);
    }

    // ---- Summary ----
    // Structured as labeled pieces with `title=` tooltips so every number
    // in the header is self-explanatory on hover. Was a single flat string
    // which left operators guessing what (e.g.) "SoC 17% → 123.09 SEK"
    // meant.
    const summary = document.getElementById('plan-summary');
    if (summary) {
      if (!state.enabled || !state.enabled.mpc) {
        summary.textContent = 'MPC planner disabled';
      } else if (!plan) {
        const visibleInputs = [];
        if (state.prices && state.prices.length) visibleInputs.push('prices');
        if (state.forecast && state.forecast.length) visibleInputs.push('forecast');
        summary.textContent = visibleInputs.length
          ? 'Showing ' + visibleInputs.join(' + ') + ' · waiting for first plan…'
          : 'Waiting for price data…';
      } else {
        const slotMin = plan.actions[0] ? plan.actions[0].slot_len_min : 15;
        const hh = plan.horizon_slots * slotMin / 60;
        const cost = plan.total_cost_ore / 100;
        const costLabel = cost >= 0 ? 'expected cost' : 'expected earnings';
        const parts = [];
        // The /api/mpc/plan response carries the INTERNAL mpc.Mode (e.g.
        // "self_consumption", "passive_arbitrage", "arbitrage"). Map to the
        // operator-facing label so the badge matches the Strategy button
        // the operator picked — "self_consumption" alone reads as the
        // manual mode, not the Smart SC (legacy) planner setting that
        // currently drives the plan.
        const PLAN_MODE_LABEL = {
          self_consumption: 'Smart self-consumption (legacy)',
          cheap_charge: 'Cheap charging (legacy)',
          passive_arbitrage: 'Passive arbitrage',
          arbitrage: 'Active arbitrage',
        };
        const modeLabel = PLAN_MODE_LABEL[plan.mode] || plan.mode;
        parts.push(
          `<span title="Active planner strategy — choose from the Mode picker">` +
          `<span class="s-value">${modeLabel}</span></span>`
        );
        parts.push(
          `<span title="How far ahead the planner is optimising">` +
          `<span class="s-value">${hh.toFixed(0)}h horizon</span></span>`
        );
        parts.push(
          `<span title="Number of ${slotMin}-minute slots inside the horizon">` +
          `<span class="s-value">${plan.horizon_slots} slots</span></span>`
        );
        if (plan.solver) {
          const solver = plan.solver;
          const solverLabel = solver.fallback
            ? `${solver.engine} fallback`
            : [solver.engine, solver.backend].filter(Boolean).join(' / ');
          const solverTitle = solver.fallback
            ? `Primary optimizer failed: ${solver.fallback_reason || 'unknown error'}`
            : `Solver status: ${solver.status || 'unknown'}; formulation: ${solver.formulation || 'unknown'}; solve: ${(solver.solve_ms || 0).toFixed(1)} ms`;
          parts.push(
            `<span title="${escapeHTML(solverTitle)}"><span class="s-label">solver </span>` +
            `<span class="s-value">${escapeHTML(solverLabel)}</span></span>`
          );
        }
        if (plan.dp_shadow) {
          const shadow = plan.dp_shadow;
          const deltaSek = (shadow.active_minus_shadow_ore || 0) / 100;
          const comparison = deltaSek <= 0
            ? `${Math.abs(deltaSek).toFixed(2)} SEK below DP`
            : `${deltaSek.toFixed(2)} SEK above DP`;
          const shadowTitle =
            `Legacy DP shadow over ${shadow.compared_slots || 0} slots; ` +
            `mean battery difference ${(shadow.mean_abs_battery_delta_w || 0).toFixed(0)} W; ` +
            `direction disagreements ${shadow.direction_disagreements || 0}; ` +
            `basis: ${shadow.forecast_basis || 'unknown'}. DP does not drive dispatch.`;
          parts.push(
            `<span title="${escapeHTML(shadowTitle)}"><span class="s-label">DP shadow </span>` +
            `<span class="s-value">${escapeHTML(comparison)}</span></span>`
          );
        }
        parts.push(
          `<span title="Battery state of charge right now — the plan starts from here">` +
          `<span class="s-label">start SoC </span>` +
          `<span class="s-value">${plan.initial_soc_pct.toFixed(0)}%</span></span>`
        );
        parts.push(
          `<span title="Total grid spend the plan expects over the full ${hh.toFixed(0)} h horizon. Negative means the plan expects to earn money (net export).">` +
          `<span class="s-label">${costLabel} </span>` +
          `<span class="s-value">${cost.toFixed(2)} SEK</span></span>`
        );
        if (state.planMeta && state.planMeta.last_replan_ms) {
          const age = Math.round((Date.now() - state.planMeta.last_replan_ms) / 1000);
          const reason = state.planMeta.last_replan_reason || '';
          const ageTxt = age < 60 ? `${age}s` : `${Math.round(age/60)}m`;
          parts.push(
            `<span title="Time since the last optimisation pass. Reason: ${reason}. Click Replan to force a fresh pass.">` +
            `<span class="s-label">replanned </span>` +
            `<span class="s-value">${ageTxt} ago</span>` +
            `<span class="s-label"> (${reason})</span></span>`
          );
        }
        summary.innerHTML = parts.join('<span class="s-sep">·</span>');
      }
    }

  }

  // Hover tooltip: hit-tests the x-coordinate against the cached
  // priceBarBounds, pops a floating panel with slot details.
  function setupHover() {
    const canvas = document.getElementById('plan-chart');
    let tip = document.getElementById('plan-tip');
    if (!tip) {
      tip = document.createElement('div');
      tip.id = 'plan-tip';
      tip.className = 'plan-tip';
      tip.style.display = 'none';
      document.body.appendChild(tip);
    }
    // Vertical hover line — mirrors the Live chart's drawHoverOverlay
    // line. Implemented as an absolutely-positioned <div> over the
    // canvas instead of a canvas-redraw, so the plan-chart's existing
    // single-pass draw model stays untouched. Parented to the canvas's
    // offset parent so it scrolls/resizes with it.
    let hoverLine = document.getElementById('plan-hover-line');
    if (canvas && !hoverLine) {
      hoverLine = document.createElement('div');
      hoverLine.id = 'plan-hover-line';
      hoverLine.style.cssText =
        'position:absolute;top:0;width:1px;height:100%;' +
        'background:rgba(255,255,255,0.3);' +
        'border-left:1px dashed rgba(255,255,255,0.45);' +
        'pointer-events:none;display:none;z-index:2';
      const host = canvas.parentElement;
      if (host) {
        if (getComputedStyle(host).position === 'static') host.style.position = 'relative';
        host.appendChild(hoverLine);
      }
    }
    if (!canvas) return;

    // Single render path used by both mouse-hover and touch-scrub.
    // Returns true when a slot was matched (for the touch path so it
    // can decide whether to keep blocking the page scroll).
    function showTipAtClient(clientX, clientY) {
      if (!state.priceBarBounds || state.priceBarBounds.length === 0) {
        hideTipAndLine();
        return false;
      }
      const rect = canvas.getBoundingClientRect();
      const cx = clientX - rect.left;
      // Hover line tracks the pointer continuously across the canvas,
      // even in the gutters between 15-minute bars.
      if (hoverLine) {
        hoverLine.style.left = cx + 'px';
        hoverLine.style.display = 'block';
      }
      let found = null;
      for (const b of state.priceBarBounds) {
        if (cx >= b.x0 && cx <= b.x1) { found = b; break; }
      }
      if (!found) { tip.style.display = 'none'; return false; }
      const a = found.action;
      const d = new Date(found.ts);
      const hh = d.getHours().toString().padStart(2, '0') + ':' + d.getMinutes().toString().padStart(2, '0');
      const dayStr = d.toLocaleDateString(undefined, { weekday: 'short' });
      const predicted = a.confidence != null && a.confidence < 1.0;
      const price = a.total_ore_kwh ?? a.price_ore;
      // PV is site-signed internally (generation = negative). Flip it for
      // display so the tooltip reads as a positive production number —
      // that's what everyone expects when they see the word "PV".
      const lines = [
        `<div class="tip-head">${dayStr} ${hh}${predicted ? ' <span class="tip-pred">predicted</span>' : ''}</div>`,
        `<div class="tip-row"><span title="Consumer total: spot + grid tariff + VAT — the actual öre/kWh you pay during this 15-minute slot">Price</span><b>${price.toFixed(1)} öre/kWh</b></div>`,
      ];
      // Price breakdown: show where the consumer total comes from.
      // Same stacking model the chart uses, so hover numbers match
      // the colored segments one-for-one.
      const tipSpot = a.spot_ore ?? a.spot_ore_kwh ?? null;
      const tipGrid = (state.priceCfg && state.priceCfg.grid_tariff_ore_kwh) || 0;
      const tipVat  = (state.priceCfg && state.priceCfg.vat_percent) || 0;
      if (tipSpot != null && (tipGrid > 0 || tipVat > 0)) {
        const tipVatOre = Math.max(0, (tipSpot + tipGrid) * (tipVat / 100));
        lines.push(
          `<div class="tip-breakdown">` +
            `<div class="tip-break-row"><span class="tip-break-sw" style="background:rgba(148,163,184,0.72)"></span>` +
              `<span title="Raw Nord Pool wholesale price — the part that varies hour by hour and drives the planner's timing decisions">spot</span>` +
              `<b>${tipSpot.toFixed(1)} öre</b></div>` +
            `<div class="tip-break-row"><span class="tip-break-sw" style="background:rgba(100,116,139,0.45)"></span>` +
              `<span title="Fixed transport / network fee added by the grid operator — doesn't change hour to hour">grid tariff</span>` +
              `<b>+${tipGrid.toFixed(1)} öre</b></div>` +
            `<div class="tip-break-row"><span class="tip-break-sw" style="background:rgba(100,116,139,0.25)"></span>` +
              `<span title="Value-added tax (moms) applied on spot + grid tariff">VAT ${tipVat.toFixed(0)}%</span>` +
              `<b>+${tipVatOre.toFixed(1)} öre</b></div>` +
          `</div>`
        );
      }
      if (a.pv_w != null) {
        const pvGen = Math.max(0, -a.pv_w) / 1000;
        lines.push(`<div class="tip-row"><span title="Solar generation the plan assumes for this slot">PV forecast</span><b>${pvGen.toFixed(1)} kW</b></div>`);
      }
      if (a.load_w != null) lines.push(`<div class="tip-row"><span title="Household consumption the plan assumes for this slot">Load forecast</span><b>${(a.load_w / 1000).toFixed(1)} kW</b></div>`);
      if (a.loadpoint_w != null && a.loadpoint_w > 0) {
        const evSoc = a.loadpoint_soc_pct != null ? ` → ${a.loadpoint_soc_pct.toFixed(0)}%` : '';
        lines.push(`<div class="tip-row"><span title="Planned EV charging power for this slot">EV charging</span><b>${(a.loadpoint_w / 1000).toFixed(1)} kW${evSoc}</b></div>`);
      }
      if (a.battery_w != null) {
        const dir = a.battery_w > 100 ? 'charge' : a.battery_w < -100 ? 'discharge' : 'idle';
        lines.push(`<div class="tip-row"><span title="Planned battery power. + = charging, − = discharging">Battery</span><b>${(a.battery_w / 1000).toFixed(1)} kW (${dir})</b></div>`);
      }
      if (a.grid_w != null) {
        const gdir = a.grid_w > 0 ? 'import' : 'export';
        lines.push(`<div class="tip-row"><span title="Net grid flow the plan expects. Import = buy from grid, export = sell back">Grid</span><b>${(Math.abs(a.grid_w) / 1000).toFixed(1)} kW ${gdir}</b></div>`);
      }
      if (a.soc_pct != null) lines.push(`<div class="tip-row"><span title="Battery state of charge at the end of this slot">SoC (end)</span><b>${a.soc_pct.toFixed(0)}%</b></div>`);
      if (a.battery_w != null) {
        let action, actionHint;
        if (a.battery_w > 100) { action = 'Charging'; actionHint = 'import to cover load + top up battery'; }
        else if (a.battery_w < -100) { action = 'Discharging'; actionHint = 'battery covers load (and may export)'; }
        else { action = 'Idle'; actionHint = 'battery neither charges nor discharges'; }
        lines.push(`<div class="tip-row"><span title="Battery action this slot">Plan</span><b>${action}</b></div>`);
        lines.push(`<div class="tip-reason">${a.reason ? a.reason : `${action.toLowerCase()} — ${actionHint}${predicted ? ' (predicted)' : ''}`}</div>`);
      } else if (a.reason) {
        lines.push(`<div class="tip-reason">${a.reason}</div>`);
      }
      tip.innerHTML = lines.join('');
      // Touch scrub on a phone has no cursor, so the tooltip is positioned
      // relative to the canvas (above the touch point) rather than offset
      // from it — fingers occlude the slot otherwise. Mouse path keeps
      // the original "near the cursor" placement.
      if (isTouching) {
        const r = canvas.getBoundingClientRect();
        const left = Math.min(window.innerWidth - 8 - 280, Math.max(8, clientX - 140));
        const top  = Math.max(8, r.top - 8 + window.scrollY - tip.offsetHeight);
        tip.style.left = left + 'px';
        tip.style.top  = top  + 'px';
      } else {
        tip.style.left = (clientX + 14) + 'px';
        tip.style.top  = (clientY + 14) + 'px';
      }
      tip.style.display = 'block';
      return true;
    }

    function hideTipAndLine() {
      tip.style.display = 'none';
      if (hoverLine) hoverLine.style.display = 'none';
    }

    canvas.addEventListener('mousemove', function (e) {
      if (isTouching) return; // touch path owns the tooltip
      showTipAtClient(e.clientX, e.clientY);
    });
    canvas.addEventListener('mouseleave', function () {
      if (!isTouching) hideTipAndLine();
    });

    // Touch — long-press to enter scrub mode, then drag to walk the
    // tooltip across slots. 250 ms threshold lets a vertical
    // swipe-to-scroll pass through unmolested; if the finger moves
    // > 10 px before the timer fires the press is cancelled (gesture
    // is a scroll, not a press). Mirrors ftw-price-chart.js's
    // implementation so phone users get the same affordance on both
    // charts.
    let isTouching = false;
    let pressTimer = null;
    let scrubbing = false;
    let startX = 0, startY = 0;
    const SCRUB_DELAY_MS = 250;
    const SCRUB_TOLERANCE_PX = 10;
    const cancelPress = () => {
      if (pressTimer) { clearTimeout(pressTimer); pressTimer = null; }
    };
    const enterScrub = () => {
      pressTimer = null;
      scrubbing = true;
      if (navigator.vibrate) { try { navigator.vibrate(8); } catch (_) {} }
      showTipAtClient(startX, startY);
    };
    const endTouch = () => {
      cancelPress();
      if (scrubbing) { scrubbing = false; hideTipAndLine(); }
      // Defer clearing isTouching past the synthesized mouse events
      // that fire after touchend on iOS/Android — without this the
      // tooltip flashes back open as the page settles.
      setTimeout(() => { isTouching = false; }, 400);
    };
    canvas.addEventListener('touchstart', function (e) {
      if (e.touches.length !== 1) { cancelPress(); return; }
      const t = e.touches[0];
      startX = t.clientX; startY = t.clientY;
      isTouching = true;
      cancelPress();
      pressTimer = setTimeout(enterScrub, SCRUB_DELAY_MS);
    }, { passive: true });
    canvas.addEventListener('touchmove', function (e) {
      if (e.touches.length !== 1) return;
      const t = e.touches[0];
      if (!scrubbing) {
        if (Math.hypot(t.clientX - startX, t.clientY - startY) > SCRUB_TOLERANCE_PX) cancelPress();
        return;
      }
      // In scrub mode — block page scroll so the chart owns the gesture.
      e.preventDefault();
      showTipAtClient(t.clientX, t.clientY);
    }, { passive: false });
    canvas.addEventListener('touchend', endTouch);
    canvas.addEventListener('touchcancel', endTouch);
  }

  // Strategy explanation — surfaces one-sentence logic for the current mode.
  const STRATEGY_DESC = {
    planner_passive_arbitrage: 'Passive arbitrage. Charges the battery from the cheapest available energy each slot — PV when sunny, grid during cheap night hours — for your own use. Never exports from the battery. Subsumes smart self-consumption (summer behavior) and cheap charging (winter behavior); the planner picks per slot.',
    planner_arbitrage: 'Active arbitrage. Full freedom: charges in the cheapest slots, discharges into the most expensive slots including export to grid. Biggest savings on volatile days; respects battery efficiency + SoC bounds.',
    planner_self: 'Legacy: smart self-consumption. Forecast-aware grid-zero control with no grid-charge. Superseded by Passive arbitrage as of v0.82.',
    planner_cheap: 'Legacy: cheap charging. Imports during cheap hours; never exports via battery. Superseded by Passive arbitrage as of v0.82.',
    self_consumption: 'Self (manual). Simple grid-zero controller with no planner; charges surplus and discharges to cover local import.',
    peak_shaving: 'Manual peak shaving. Limits grid import to the peak-limit setting.',
    charge: 'Manual full charge — forces the battery to charge regardless of price.',
    idle: 'Battery idle — no dispatch.',
  };
  function renderStrategyHint() {
    ownerFetch('/api/status')
      .then(function (r) { return r.json(); })
      .then(function (d) {
        const el = document.getElementById('strategy-hint');
        if (!el) return;
        el.textContent = STRATEGY_DESC[d.mode] || '';
      })
      .catch(function () {});
  }

  function init() {
    fetchAll();
    setupHover();
    renderStrategyHint();
    setInterval(fetchAll, PLAN_REFRESH_MS);
    setInterval(renderStrategyHint, 5000);
    window.addEventListener('resize', render);
    const btn = document.getElementById('plan-replan');
    if (btn) btn.addEventListener('click', replan);
    if (window.ftwP2P && typeof window.ftwP2P.onState === 'function') {
      var waitingForDirect = false;
      window.ftwP2P.onState(function (s) {
        if (s !== 'direct') {
          waitingForDirect = true;
          return;
        }
        if (!waitingForDirect) return;
        waitingForDirect = false;
        fetchAll();
        renderStrategyHint();
      });
    }

    // Horizon toggle wiring. Each click flips state.horizon, persists
    // the choice, marks the right button active, and re-renders. The
    // visual style (.toggle .active) is shared with the VAT pill.
    const horizonRoot = document.getElementById('plan-horizon');
    if (horizonRoot) {
      // Reflect the persisted preference on first paint.
      horizonRoot.setAttribute('data-horizon', state.horizon);
      horizonRoot.querySelectorAll('button[data-horizon]').forEach(b => {
        const isActive = b.dataset.horizon === state.horizon;
        b.classList.toggle('active', isActive);
        b.setAttribute('aria-selected', isActive ? 'true' : 'false');
      });
      horizonRoot.addEventListener('click', function (e) {
        const target = e.target.closest('button[data-horizon]');
        if (!target) return;
        const next = target.dataset.horizon;
        if (next !== 'today' && next !== 'all' && next !== 'tomorrow') return;
        if (next === state.horizon) return;
        state.horizon = next;
        writeHorizonPref(next);
        horizonRoot.setAttribute('data-horizon', next);
        horizonRoot.querySelectorAll('button[data-horizon]').forEach(b => {
          const isActive = b.dataset.horizon === next;
          b.classList.toggle('active', isActive);
          b.setAttribute('aria-selected', isActive ? 'true' : 'false');
        });
        render();
      });
    }
    const helpBtn = document.getElementById('plan-help-btn');
    const helpModal = document.getElementById('plan-help-modal');
    if (helpBtn && helpModal) {
      helpBtn.addEventListener('click', function () {
        if (typeof helpModal.open === 'function') helpModal.open();
        else helpModal.setAttribute('open', '');
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
