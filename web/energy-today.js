const ENERGY_FIELDS = {
  "e-import": "import_wh",
  "e-export": "export_wh",
  "e-pv": "pv_wh",
  "e-charged": "bat_charged_wh",
  "e-discharged": "bat_discharged_wh",
  "e-load": "load_wh",
  "overview-e-import": "import_wh",
  "overview-e-export": "export_wh",
  "overview-e-pv": "pv_wh",
};

function knownWh(value) {
  return typeof value === "number" && Number.isFinite(value) && value >= 0;
}

export function energyTodayReading(today, key) {
  const total = today?.[key];
  if (knownWh(total)) return ((key === "load_wh" && (today?.history_quality === "estimated" || today?.load_quality === "estimated")) || (key === "pv_wh" && today?.pv_quality === "estimated"))
    ? {wh: total, partial: false, estimated: true} : {wh: total, partial: false};
  const measured = today?.measured?.[key];
  if (knownWh(measured)) return {wh: measured, partial: true};
  const subtotal = today?.load_subtotal;
  if (key === "load_wh" && knownWh(subtotal?.wh) && subtotal.quality === "estimated" &&
      subtotal.method === "accepted_interval_balance_v1" &&
      Number.isFinite(subtotal.coverage_ms) && Number.isFinite(subtotal.period_ms) &&
      subtotal.coverage_ms > 0 && subtotal.coverage_ms <= subtotal.period_ms) {
    return {wh: subtotal.wh, partial: true, estimated: true,
      coverageMs: subtotal.coverage_ms, periodMs: subtotal.period_ms};
  }
  return {wh: null, partial: false};
}

export function formatEnergyToday(today, key, withUnit = true) {
  const {wh, partial, estimated} = energyTodayReading(today, key);
  if (wh === null) return "Unknown";
  const kwh = wh / 1000;
  const digits = kwh >= 100 ? 0 : kwh >= 10 ? 1 : 2;
  return `${estimated ? "≈ " : partial ? "≥ " : ""}${kwh.toFixed(digits)}${withUnit ? " kWh" : ""}${estimated && partial ? " (partial)" : ""}`;
}

export function updateEnergyToday(today, root = document) {
  for (const [id, key] of Object.entries(ENERGY_FIELDS)) {
    const el = root.getElementById(id);
    if (!el) continue;
    const {partial, estimated} = energyTodayReading(today, key);
    el.textContent = formatEnergyToday(today, key);
    el.title = estimated && partial ? "Estimated subtotal from accepted energy intervals only; gaps are excluded."
      : estimated ? "Estimated from source energy; may include audited historical corrections."
      : partial ? "Recorded energy only; missing readings are excluded." : "";
  }
  const group = root.getElementById("e-pv")?.closest(".energy-group");
  if (!group) return;
  let note = root.getElementById("energy-today-quality");
  if (!note) {
    note = root.createElement("p");
    note.id = "energy-today-quality";
    note.className = "card-sub";
    group.append(note);
  }
  const keys = new Set(Object.values(ENERGY_FIELDS));
  const incomplete = [...keys].some(key => !knownWh(today?.[key]));
  const estimated = today?.history_quality === "estimated" || today?.load_quality === "estimated";
  note.hidden = !incomplete && !estimated;
  note.textContent = today
    ? (incomplete ? "Gaps in today's readings. ≥ shows recorded energy only." : "")
    : "Today's energy is not available yet.";
  if (estimated) note.textContent = (incomplete ? note.textContent + " " : "") + "Home use includes estimates from source energy.";
  if (today?.pv_quality === "estimated") {note.hidden = false; note.textContent += (note.textContent ? " " : "") + "Solar includes estimated energy across historical gaps.";}
  const home = energyTodayReading(today, "load_wh");
  if (home.estimated && home.partial) {
    note.hidden = false;
    note.textContent += ` Home use: estimated subtotal for ${Math.floor(home.coverageMs / 60000)} of ${Math.ceil(home.periodMs / 60000)} min; gaps excluded.`;
  }
}
