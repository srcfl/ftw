// Settings → FTW app → Anonymous statistics: the one switch for both daily
// reports, and the messages this box adds to FTW's fleet and energy totals.
//
// This tab is not a privacy policy. A policy is a promise; this screen shows
// the actual payload, built by the same call the sender uses, so somebody
// deciding whether to leave it on can read what leaves rather than trust a
// paragraph about it. The whole design in go/internal/fleetping rests on the
// claim being checkable here.
//
// Three consequences for this file:
//
//   - the rows are built from the payload's own keys, never from a list kept
//     here. If the box ever started sending a field it should not, this screen
//     shows it instead of quietly dropping it;
//   - the exact JSON body is printed underneath the readable version, because
//     a friendly rendering is a rendering, and this is the one place where the
//     literal bytes are the point;
//   - the paragraph names every field and concedes what the message cannot
//     hide. It is the one part of this screen that can go stale without
//     anything breaking, so a test holds the count in it to the number of
//     fields the box sends.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // Labels for the fields the payload carries today. A field with no label
  // here is shown under its own name — see the note above.
  var LABELS = {
    ftw_version: "FTW version",
    channel: "Release channel",
    drivers: "Driver types",
    battery_kwh: "Battery size",
    price_zone: "Price zone",
    install_age: "Time since install",
    plan_solve: "Slowest plan, last day",
    plan_fallbacks: "Fallback plans, last day",
    driver_errors: "Failed driver reads, last day",
    update: "Newest update, last week",
  };

  // The health fields, each a few words wide.
  var HEALTH = {
    plan_solve: { none: "no plan", "0-1s": "under a second", "1-5s": "one to five seconds", "5s+": "five seconds or more" },
    plan_fallbacks: { none: "no plan", "0": "none", "1-3": "one to three", "4+": "four or more" },
    driver_errors: { none: "too few reads to say", "0": "none", "0-1%": "under 1 in 100", "1%+": "1 in 100 or more" },
    update: { none: "none", ok: "succeeded", failed: "failed" },
  };

  var BATTERY = {
    none: "no battery",
    "0-5": "under 5 kWh",
    "5-15": "5 to 15 kWh",
    "15-30": "15 to 30 kWh",
    "30+": "over 30 kWh",
  };

  var AGE = {
    unknown: "not known",
    "0-1m": "under a month",
    "1-6m": "one to six months",
    "6-12m": "six to twelve months",
    "1y+": "over a year",
  };

  function describe(key, value) {
    if (Array.isArray(value)) return value.length ? value.join(", ") : "none";
    if (key === "battery_kwh" && BATTERY[value]) return BATTERY[value];
    if (key === "install_age" && AGE[value]) return AGE[value];
    if (HEALTH[key] && HEALTH[key][value]) return HEALTH[key][value];
    if (value === "unknown") return "not known";
    return String(value);
  }

  // payloadRows turns the payload into what the table shows. Driven by the
  // payload's keys, so nothing the box sends can be missing from this screen.
  // `schema` is dropped: it names the message format, not the household.
  function payloadRows(payload) {
    if (!payload || typeof payload !== "object") return [];
    return Object.keys(payload)
      .filter(function (key) { return key !== "schema"; })
      .map(function (key) {
        return { key: key, label: LABELS[key] || key, value: describe(key, payload[key]) };
      });
  }

  // whereLine says what happens to the message underneath it. Branched on the
  // switch, because "Sent to https://… once a day" printed under a ping that
  // is switched off is a claim this screen cannot keep — and not making claims
  // it cannot keep is the only reason this tab exists.
  //
  // Which is why the send time is described and not sold. It is a random walk
  // in six-hour steps, so it takes about four days to work its way round the
  // clock, and until then one box's arrival times are still loosely related.
  function whereLine(body) {
    var endpoint = (body && body.endpoint) || "nowhere";
    var relayed = !!(body && body.through_relay);
    var where = relayed ? "through " + endpoint + ", an independent relay, to FTW Cloud" : "to " + endpoint;
    var handling = relayed
      ? " The relay sees this box's address but not the report. FTW Cloud reads the report but not the address, adds it to that day's totals and keeps nothing of it. These are reports, not unique boxes."
      : body && body.ftw_relay
        ? " FTW Cloud adds the report to that day's totals, then drops it and does not save the source address. These are reports, not unique boxes."
        : " This box uses a custom collector. FTW cannot say what that service stores or whether it counts unique boxes.";
    if (body && body.waits_for_relay) {
      return "Not sent yet. This box shares statistics without having said yes, so this report goes to FTW Cloud only through an independent relay, which hides this box's address, and this release names none yet.";
    }
    if (body && body.enabled) {
      return "Sent " + where +
        ", once a day, at a time that moves from day to day — it takes a few days to work its way round the clock." +
        handling;
    }
    return "Nothing is sent. This is the message that would go " + where +
      ", once a day, if statistics were on." + handling;
  }

  // shareLine says what the one statistics switch means right now. With FTW
  // Cloud it is part of Cloud; without, it is the owner's to turn off.
  function shareLine(status) {
    if (!status) return "Could not read this box's statistics answer.";
    if (status.statisticsRequired) {
      return "Part of FTW Cloud, with no separate off switch. To stop, turn FTW Cloud off, then turn statistics off here.";
    }
    if (status.statisticsNeedTerms) {
      return "FTW Cloud is on under earlier terms, which did not include statistics. Nothing is sent until you accept the current terms.";
    }
    return status.statistics
      ? "On. Turning it off takes effect at once, with no restart, and the box keeps the answer."
      : "Off. Nothing is sent. Turning it on takes effect at once.";
  }

  // Labels for the daily energy report. As above, a field with no label is
  // shown under its own name.
  var ENERGY_LABELS = {
    date: "Day (UTC)",
    pv_generation_kwh: "Solar generation",
    grid_import_kwh: "Grid import",
    grid_export_kwh: "Grid export",
    battery_charge_kwh: "Battery charge",
    battery_discharge_kwh: "Battery discharge",
    vehicle_charge_kwh: "Vehicle charge",
  };

  // energyRows is payloadRows for the energy report: driven by its keys, so
  // nothing it carries can be missing from this screen.
  function energyRows(payload) {
    if (!payload || typeof payload !== "object") return [];
    return Object.keys(payload)
      .filter(function (key) { return key !== "schema"; })
      .map(function (key) {
        var value = payload[key];
        var text = /_kwh$/.test(key) && typeof value === "number" ? value + " kWh" : String(value);
        return { key: key, label: ENERGY_LABELS[key] || key, value: text };
      });
  }

  // energyLine is whereLine for the energy report. It follows the box's own
  // answer, and says so when this release has no relay to send through.
  //
  // Without a report to show, it does not point at one.
  function energyLine(energy) {
    var shown = !!(energy && energy.payload);
    if (!energy || !energy.relay) {
      return "This release has no relay yet, so no energy report is sent." +
        (shown ? " This is the report it would send." : "");
    }
    var how = " The relay sees this box's address but not the report. FTW Cloud reads the report but not the address, adds it to that day's totals and keeps nothing of it.";
    if (energy.sending) {
      return "Sent through " + energy.relay + ", an independent relay, once a day, 1 to 12 hours after the UTC day ends." + how;
    }
    return "Nothing is sent. " + (shown ? "This is the report that would go" : "A report would go") + " through " +
      energy.relay + ", an independent relay, once a day, if sharing were on." + how;
  }

  function row(label, value) {
    var el = document.createElement("div");
    el.className = "fleet-ping-row";

    var name = document.createElement("span");
    name.className = "fleet-ping-label";
    name.textContent = label;
    el.appendChild(name);

    var val = document.createElement("span");
    val.className = "fleet-ping-value";
    val.textContent = value;
    el.appendChild(val);

    return el;
  }

  function paint(body) {
    var slot = document.getElementById("fleet-ping-payload");
    if (!slot) return;
    slot.textContent = "";

    var rows = payloadRows(body && body.payload);
    if (rows.length === 0) {
      var none = document.createElement("p");
      none.className = "hint";
      none.textContent = "This box has nothing to send yet.";
      slot.appendChild(none);
      return;
    }
    rows.forEach(function (r) { slot.appendChild(row(r.label, r.value)); });

    var where = document.createElement("p");
    where.className = "hint";
    where.textContent = whereLine(body);
    slot.appendChild(where);

    var exact = document.createElement("pre");
    exact.className = "fleet-ping-json";
    exact.textContent = JSON.stringify(body.payload, null, 2);
    slot.appendChild(exact);
    if (body.through_relay) sealedRows(slot);
  }

  // sealedRows shows what goes beside a message sent through the relay,
  // inside the sealed request: drawn when it is sent, so not shown as values.
  function sealedRows(slot) {
    slot.appendChild(row("Report id", "a random number, new for each report"));
    slot.appendChild(row("Send day", "the UTC day it is sent"));
  }

  function paintEnergy(energy) {
    var slot = document.getElementById("fleet-energy-payload");
    if (!slot) return;
    slot.textContent = "";
    var rows = energyRows(energy && energy.payload);
    var note = document.createElement("p");
    note.className = "hint fleet-energy-note";
    if (!energy) {
      note.textContent = "This box has no energy report.";
      slot.appendChild(note);
      return;
    }
    if (rows.length === 0) {
      note.textContent = "The energy record for " + energy.day + " has a gap, so this box has no report for that day. A day with a gap is skipped, never sent in part.";
      slot.appendChild(note);
    }
    rows.forEach(function (r) { slot.appendChild(row(r.label, r.value)); });

    var where = document.createElement("p");
    where.className = "hint fleet-energy-note";
    where.textContent = energyLine(energy);
    slot.appendChild(where);

    if (rows.length === 0) return;
    var exact = document.createElement("pre");
    exact.className = "fleet-ping-json";
    exact.textContent = JSON.stringify(energy.payload, null, 2);
    slot.appendChild(exact);
    sealedRows(slot);
  }

  var sharePolicy = null;

  // refreshShare reads the box's one statistics answer into the switch.
  function refreshShare() {
    var box = document.getElementById("statistics-share");
    var note = document.getElementById("statistics-share-note");
    if (!box || !note) return;
    Promise.all([fetch("/api/cloud"), fetch("/api/cloud/stats-policy")])
      .then(function (rs) { return Promise.all(rs.map(function (r) { return r.ok ? r.json() : null; })); })
      .then(function (v) {
        sharePolicy = v[1];
        box.checked = !!(v[0] && v[0].statistics);
        box.disabled = !v[0] || !v[1] || !!v[0].statisticsRequired || !!v[0].statisticsNeedTerms;
        note.textContent = shareLine(v[0]);
      })
      .catch(function () { note.textContent = shareLine(null); });
  }

  // The switch writes at once, as the Cloud terms do, and not on Save: it is
  // the box's answer, not part of the config document.
  function changeShare(box) {
    var note = document.getElementById("statistics-share-note");
    box.disabled = true;
    var write = box.checked
      ? fetch("/api/cloud/stats", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ policy: sharePolicy && sharePolicy.policy }) })
      : fetch("/api/cloud/stats", { method: "DELETE" });
    write
      .then(function (r) { return r.json().then(function (v) { if (!r.ok) throw Error(v.error || "The box refused the change."); }); })
      .then(function () { refreshShare(); refresh(); })
      .catch(function (e) {
        refreshShare();
        if (note) note.textContent = e.message || "The change could not be saved. Try again.";
      });
  }

  function refresh() {
    var slot = document.getElementById("fleet-ping-payload");
    if (!slot) return;
    fetch("/api/fleet-ping")
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (body) {
        if (!body) {
          slot.textContent = "";
          var err = document.createElement("p");
          err.className = "hint";
          err.textContent = "Fleet statistics are not running on this box, so there is nothing to show.";
          slot.appendChild(err);
          return;
        }
        paint(body);
        paintEnergy(body.energy);
      })
      .catch(function () {
        slot.textContent = "";
        var err = document.createElement("p");
        err.className = "hint";
        err.textContent = "Could not reach the box.";
        slot.appendChild(err);
      });
  }

  // The FTW app and statistics share a settings page, but not a setting: a
  // box shares its anonymous reports whether or not it uses the app. Keep
  // this as a reusable panel instead of a tab so the payloads and privacy
  // wording stay in one place.
  S.fleetPing = {
    _pure: { payloadRows: payloadRows, describe: describe, whereLine: whereLine, shareLine: shareLine, energyRows: energyRows, energyLine: energyLine },

    // Saving the config does not change the answer, but the payload under it
    // is built from the config, so read both again.
    afterSave: function () { refresh(); refreshShare(); },

    render: function () {
      setTimeout(function () {
        var box = document.getElementById("statistics-share");
        if (box) box.addEventListener("change", function () { changeShare(box); });
        refreshShare();
        refresh();
      }, 0);

      return (
        "<fieldset><legend>Anonymous statistics</legend>" +
        '<p class="hint">This box sends two anonymous reports a day, each on its own and at its own time: ' +
        "a fleet report and an energy report. With FTW Cloud they are part of Cloud. Without Cloud they " +
        "are on until you turn them off here.</p>" +
        '<label><input type="checkbox" id="statistics-share" disabled> Share anonymous statistics</label>' +
        '<p id="statistics-share-note" class="hint" role="status">Checking…</p>' +
        '<h4 class="fleet-energy-title">Daily fleet report</h4>' +
        '<p class="hint">The fleet report carries ten things once a day: its ' +
        "FTW version, its release channel, the kinds of device it talks to, " +
        "roughly how much battery, its price zone, roughly how old the " +
        "install is, and four rough marks of how it ran: how slow its slowest " +
        "plan of the last day was, how many plans fell back, how often its " +
        "drivers failed to read, and whether its newest update of the last week " +
        "failed. Nothing else — no id, no key, no serial, no site name, no " +
        "counter, no timestamp — so nothing in the message says which box sent " +
        "it. The message itself cannot prove how many unique boxes sent reports. " +
        "Sent through the relay, two things go with it, sealed: a random number, " +
        "drawn new for each report so FTW Cloud counts it once, and the UTC day it " +
        "was sent. Neither says which box sent it.</p>" +
        '<p class="hint fleet-ping-limits">Two limits, plainly. Those ten things still describe ' +
        "you: a beta box in a small price zone with a big battery may be the " +
        "only one of its kind. And without a relay the receiving service sees the address during " +
        "the request, the way any website does. What it keeps depends on the " +
        "endpoint shown below. Here is the whole message, as this box would send it right now, " +
        "with what goes beside it.</p>" +
        '<div id="fleet-ping-payload"></div>' +
        '<h4 class="fleet-energy-title">Daily energy report</h4>' +
        '<p class="hint">The other report: the date, and the ' +
        "day's solar generation, grid import and export, battery charge and " +
        "discharge, and vehicle charge, each in whole kWh. It carries no box id and " +
        "nothing from the fleet report, and leaves at its own time, so the two " +
        "cannot be matched. They never travel together: device kinds and a price " +
        "zone can point at one home, and the energy beside them would then be " +
        "that home's. It goes through an independent relay, so FTW Cloud reads " +
        "the numbers without this box's address, and the relay sees the address " +
        "without the numbers. Sealed with it go a random number, drawn new for " +
        "each report so FTW Cloud counts it once, and the UTC day it was sent. " +
        "A day with a gap in the energy record is skipped.</p>" +
        '<div id="fleet-energy-payload"></div>' +
        "</fieldset>"
      );
    },
  };
})();
