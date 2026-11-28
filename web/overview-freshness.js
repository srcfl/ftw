// The overview's LIVE pill and the devices that do not answer.
//
// Freshness is two facts, as in the apps: the carrier (this page's poll of
// the box answers) and each device's source state. Core grades the source
// state with the apps' own ladder and sends it as drivers[name].src_state on
// /api/status, so this page and the apps call the same device live. LIVE is
// lit only while both say live. A device the box cannot read is named, with
// what Core knows about it.

const LIVE = "live";

// A page served by an older Core has no src_state; read the emit status.
function sourceState(d) {
  if (typeof d.src_state === "string") return d.src_state;
  if (d.not_running) return "never";
  return d.status === "offline" || d.status === "fault" ? "down" : LIVE;
}

function clip(text) {
  const s = String(text || "").replace(/\s+/g, " ").replace(/^driver_(poll|init): /, "").trim();
  return s.length > 160 ? s.slice(0, 159) + "…" : s;
}

function span(ms) {
  const min = Math.max(1, Math.round(ms / 60000));
  if (min < 90) return min + " min";
  const h = Math.round(min / 60);
  if (h < 48) return h + " h";
  return Math.round(h / 24) + " days";
}

function quietRow(name, d, state) {
  if (d.not_running) return { name, says: "did not start", detail: clip(d.last_error) };
  if (state === "never") {
    // The first poll has not finished yet: not yet a fact about the device.
    if (!(d.consecutive_errors > 0) && d.status !== "offline") return null;
    return { name, says: "has never answered", detail: clip(d.last_error) };
  }
  // Lagging is a late reading, not yet a quiet device; LIVE stays dark.
  if (state !== "stale" && state !== "down") return null;
  // A device whose reads succeed but that cannot act (DeviceFault) is down
  // too. Failing reads come first: a device that does not answer cannot
  // take a command either.
  if ((d.device_fault || d.status === "fault") && !(d.consecutive_errors > 0)) {
    return { name, says: "answers but cannot act", detail: clip(d.device_fault_reason) };
  }
  const age = Number(d.last_ok_age_ms);
  return {
    name,
    says: Number.isFinite(age) ? "has not answered for " + span(age) : "has stopped answering",
    detail: clip(d.last_error),
  };
}

// live: every configured device answers now. quiet: the ones to name.
export function deviceFreshness(status) {
  const drivers = (status && status.drivers) || {};
  let live = true;
  const quiet = [];
  for (const name of Object.keys(drivers).sort()) {
    const d = drivers[name] || {};
    if (d.disabled || d.status === "disabled") continue;
    const state = sourceState(d);
    if (state !== LIVE) live = false;
    const row = quietRow(name, d, state);
    if (row) quiet.push(row);
  }
  return { live, quiet };
}

function paintQuiet(doc, card, rows, openDevices) {
  const key = JSON.stringify(rows);
  if (card.dataset.key === key) return;
  card.dataset.key = key;
  card.hidden = rows.length === 0;
  if (!rows.length) {
    card.replaceChildren();
    return;
  }
  const list = doc.createElement("ul");
  for (const row of rows) {
    const item = doc.createElement("li");
    const name = doc.createElement("strong");
    name.textContent = row.name;
    item.append(name, " " + row.says);
    if (row.detail) {
      const detail = doc.createElement("small");
      detail.textContent = row.detail;
      item.append(detail);
    }
    list.append(item);
  }
  const parts = [list];
  if (openDevices) {
    const button = doc.createElement("button");
    button.type = "button";
    button.textContent = "Check devices";
    button.addEventListener("click", openDevices);
    parts.push(button);
  }
  card.replaceChildren(...parts);
}

// Paints the pill and the card from one /api/status answer that arrived.
export function paintOverviewFreshness(doc, status, openDevices) {
  const fresh = deviceFreshness(status);
  const pill = doc.getElementById("overview-health");
  const label = doc.getElementById("overview-health-label");
  const card = doc.getElementById("overview-quiet");
  if (pill) {
    pill.classList.add("is-connected");
    pill.classList.toggle("is-live", fresh.live);
  }
  if (label) label.textContent = fresh.live ? "Live" : "Not live";
  if (card) paintQuiet(doc, card, fresh.quiet, openDevices);
  return fresh;
}
