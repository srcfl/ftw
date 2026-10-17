// Settings → Cars: the owner's cars on this box (docs/cars.md).
//
// A car belongs to its owner, not to a charger. This tab lists the cars the
// box keeps, with how the box knows each one: its VIN, which ties a level
// source such as Home Assistant to it, the tags a charger may name it by,
// and those sources. The owner renames a car, sets its battery size, adds
// one and removes one. Each button asks the box at once with the car routes;
// nothing here is part of the settings the Save button writes.
//
// Names come from the box and the owner, and only ever go into the page as
// text.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // A refused op, in words, by its reason.
  var REFUSED = {
    duplicate: "Another car already has that VIN or tag.",
    invalid: "Check the name (up to 64 characters), the battery size (1–300 kWh) and the VIN (17 characters).",
    cars: "This box keeps at most 32 cars. Remove one first.",
    car: "The box no longer has this car. The list is read again.",
    persistence_unconfirmed: "The box could not confirm it saved this. Check the list before trying again.",
  };

  var state = { cars: null, redacted: false, problem: "" };

  // newCarId mints a car's id as the owner's app does: 128 random bits as 32
  // lowercase hex digits.
  function newCarId(random) {
    var bytes = new Uint8Array(16);
    (random || function (b) { return crypto.getRandomValues(b); })(bytes);
    return Array.prototype.map.call(bytes, function (b) { return (b < 16 ? "0" : "") + b.toString(16); }).join("");
  }

  // kWh reads a battery size typed in kWh, with a comma or a point, as Wh.
  // null for anything the box would refuse.
  function kWh(text) {
    var v = parseFloat(String(text == null ? "" : text).trim().replace(",", "."));
    if (!isFinite(v) || v < 1 || v > 300) return null;
    return Math.round(v * 1000);
  }

  function kWhText(wh) {
    if (!(wh > 0)) return "";
    var v = wh / 1000;
    return String(Math.round(v * 10) / 10);
  }

  function refusalText(body) {
    return (body && REFUSED[body.reason]) || (body && body.error) || "The box did not take that. Try again.";
  }

  // The add form's body. A VIN is optional; an empty one is left out.
  function addBody(name, capacity, vin, random) {
    var body = { id: newCarId(random), name: String(name || "").trim(), capacity_wh: kWh(capacity) };
    var v = String(vin || "").trim().toUpperCase();
    if (v) body.vin = v;
    return body;
  }

  // The car that already has a VIN. Adding a car with a VIN the box has is
  // how the owner's app takes over a car the box made, so here it would
  // quietly replace that car; the page says so instead.
  function vinTakenBy(cars, vin) {
    var v = String(vin || "").trim().toUpperCase();
    if (!v) return null;
    for (var i = 0; i < (cars || []).length; i++) {
      if (cars[i].vin && cars[i].vin.toUpperCase() === v) return cars[i];
    }
    return null;
  }

  // What changed in a car's edit form, or null when nothing did.
  function updateBody(car, name, capacity) {
    var body = {};
    var n = String(name || "").trim();
    if (n && n !== car.name) body.name = n;
    var wh = kWh(capacity);
    if (wh != null && wh !== car.capacity_wh) body.capacity_wh = wh;
    return Object.keys(body).length ? body : null;
  }

  // How the box knows a car, as label and value pairs. redacted is the
  // box's word that it left these out for this device, which has not signed
  // in; then they are hidden, not missing.
  function knownBy(car, redacted) {
    if (redacted) {
      var hidden = "Hidden until this device signs in with the house password";
      return [["VIN", hidden], ["Level from", hidden], ["Charger tags", hidden]];
    }
    var rows = [];
    rows.push(["VIN", car.vin || "Not set"]);
    if (car.make) rows.push(["Make", car.make]);
    rows.push(["Level from", car.sources && car.sources.length ? car.sources.join(", ")
      : "No source here. The level comes from the charger or from you."]);
    rows.push(["Charger tags", car.identifiers && car.identifiers.length ? car.identifiers.join(", ") : "None"]);
    return rows;
  }

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === "text") node.textContent = attrs[k];
      else if (k === "className") node.className = attrs[k];
      else if (k.indexOf("on") === 0) node.addEventListener(k.slice(2), attrs[k]);
      else node.setAttribute(k, attrs[k]);
    });
    (children || []).forEach(function (c) { if (c) node.appendChild(c); });
    return node;
  }

  function send(ctx, method, path, body) {
    var opts = { method: method };
    if (body) {
      opts.headers = { "Content-Type": "application/json" };
      opts.body = JSON.stringify(body);
    }
    return ctx.apiFetch(path, opts).then(function (r) {
      return r.json().catch(function () { return null; }).then(function (data) {
        return { ok: r.ok, data: data };
      });
    });
  }

  function refresh(ctx) {
    return ctx.apiFetch("/api/cars")
      .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); })
      .then(function (data) {
        state.cars = (data && data.cars) || [];
        state.redacted = !!(data && data.redacted);
        state.problem = "";
      })
      .catch(function () { state.problem = "The box did not answer. Try again in a moment."; })
      .then(function () { drawList(ctx); });
  }

  function drawList(ctx) {
    var host = document.getElementById("cars-list");
    if (!host) return;
    host.textContent = "";
    if (state.problem) {
      host.appendChild(el("p", { className: "cars-error", role: "alert", text: state.problem }));
      return;
    }
    if (!state.cars) {
      host.appendChild(el("p", { className: "cars-quiet", text: "Checking the box…" }));
      return;
    }
    if (!state.cars.length) {
      host.appendChild(el("p", { className: "cars-quiet", text: "No cars yet. Add the cars you charge here below." }));
      return;
    }
    state.cars.forEach(function (car) { host.appendChild(carRow(ctx, car)); });
  }

  function carRow(ctx, car) {
    var note = el("p", { className: "cars-note", "aria-live": "polite" });
    var name = el("input", { type: "text", maxlength: "64", value: car.name, "aria-label": "Name of " + car.name });
    var size = el("input", { type: "text", inputmode: "decimal", value: kWhText(car.capacity_wh),
      "aria-label": "Battery size of " + car.name + " in kWh" });
    var save = el("button", { type: "submit", className: "btn-send", text: "Save" });
    var form = el("form", { className: "cars-edit" }, [
      el("label", {}, [el("span", { text: "Name" }), name]),
      el("label", {}, [el("span", { text: "Battery, kWh" }), size]),
      save,
    ]);
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      if (kWh(size.value) == null && String(size.value).trim() !== "") {
        note.className = "cars-error";
        note.textContent = "Battery size is 1–300 kWh.";
        return;
      }
      var body = updateBody(car, name.value, size.value);
      if (!body) {
        note.className = "cars-note";
        note.textContent = "Nothing changed.";
        return;
      }
      save.disabled = true;
      send(ctx, "PATCH", "/api/cars/" + encodeURIComponent(car.id), body).then(function (res) {
        save.disabled = false;
        if (!res.ok) {
          note.className = "cars-error";
          note.textContent = refusalText(res.data);
          if (res.data && res.data.reason === "car") refresh(ctx);
          return;
        }
        Object.assign(car, res.data && res.data.car);
        note.className = "cars-note";
        note.textContent = "Saved.";
      }).catch(function () {
        save.disabled = false;
        note.className = "cars-error";
        note.textContent = "The box did not answer. Try again.";
      });
    });

    var facts = el("dl", { className: "cars-facts" });
    knownBy(car, state.redacted).forEach(function (row) {
      facts.appendChild(el("dt", { text: row[0] }));
      facts.appendChild(el("dd", { text: row[1] }));
    });

    var remove = el("button", { type: "button", className: "cars-remove", text: "Remove car" });
    remove.addEventListener("click", function () {
      if (!window.confirm("Remove " + car.name + " from this box? If it is charging now, the box charges it as a visitor's car.")) return;
      remove.disabled = true;
      send(ctx, "DELETE", "/api/cars/" + encodeURIComponent(car.id)).then(function (res) {
        remove.disabled = false;
        if (!res.ok && !(res.data && res.data.reason === "car")) {
          note.className = "cars-error";
          note.textContent = refusalText(res.data);
          return;
        }
        refresh(ctx);
      }).catch(function () {
        remove.disabled = false;
        note.className = "cars-error";
        note.textContent = "The box did not answer. Try again.";
      });
    });

    return el("div", { className: "cars-row" }, [
      form,
      facts,
      car.box_minted ? el("p", { className: "cars-quiet",
        text: "Made by this box from a charger's settings. The FTW app takes it over when it adds the same car." }) : null,
      el("div", { className: "cars-actions" }, [remove]),
      note,
    ]);
  }

  function wireAdd(ctx) {
    var form = document.getElementById("cars-add");
    if (!form) return;
    var note = document.getElementById("cars-add-note");
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var name = form.querySelector('[name="name"]').value;
      var size = form.querySelector('[name="capacity"]').value;
      var vin = form.querySelector('[name="vin"]').value;
      if (!String(name).trim() || kWh(size) == null) {
        note.className = "cars-error";
        note.textContent = "Give the car a name and a battery size of 1–300 kWh.";
        return;
      }
      var taken = vinTakenBy(state.cars, vin);
      if (taken) {
        note.className = "cars-error";
        note.textContent = taken.name + " already has that VIN. Change that car above instead.";
        return;
      }
      var button = form.querySelector('button[type="submit"]');
      button.disabled = true;
      send(ctx, "POST", "/api/cars", addBody(name, size, vin)).then(function (res) {
        button.disabled = false;
        if (!res.ok) {
          note.className = "cars-error";
          note.textContent = refusalText(res.data);
          return;
        }
        form.reset();
        note.className = "cars-note";
        note.textContent = "Added " + ((res.data && res.data.car && res.data.car.name) || "the car") + ".";
        refresh(ctx);
      }).catch(function () {
        button.disabled = false;
        note.className = "cars-error";
        note.textContent = "The box did not answer. Try again.";
      });
    });
  }

  S.tabs.cars = {
    render: function () {
      return (
        '<fieldset class="cars-settings">' +
        "<legend>Cars</legend>" +
        '<p class="cars-lede">The cars you charge here. A car is yours, not the charger\'s: ' +
        "FTW plans for the car that is plugged in, with its own level, battery and goal. " +
        "With more than one car, the box asks which car it is when it cannot tell.</p>" +
        '<div id="cars-list"><p class="cars-quiet">Checking the box…</p></div>' +
        "</fieldset>" +
        "<fieldset>" +
        "<legend>Add a car</legend>" +
        '<form id="cars-add" class="cars-edit" autocomplete="off">' +
        '<label for="cars-add-name"><span>Name</span><input type="text" id="cars-add-name" name="name" maxlength="64" placeholder="Model Y"></label>' +
        '<label for="cars-add-capacity"><span>Battery, kWh</span><input type="text" id="cars-add-capacity" name="capacity" inputmode="decimal" placeholder="75"></label>' +
        '<label for="cars-add-vin"><span>VIN, optional</span><input type="text" id="cars-add-vin" name="vin" class="mono" maxlength="17" ' +
        'autocapitalize="characters" spellcheck="false"></label>' +
        '<button type="submit" class="btn-send">Add car</button>' +
        "</form>" +
        '<p class="cars-quiet">The VIN lets a source that reports it, such as Home Assistant or the maker\'s ' +
        "service, give this car its level.</p>" +
        '<p id="cars-add-note" class="cars-note" aria-live="polite"></p>' +
        "</fieldset>"
      );
    },
    after: function (ctx) {
      wireAdd(ctx);
      drawList(ctx);
      refresh(ctx);
    },
  };

  S.tabs.cars._pure = {
    REFUSED: REFUSED,
    newCarId: newCarId,
    kWh: kWh,
    kWhText: kWhText,
    refusalText: refusalText,
    addBody: addBody,
    updateBody: updateBody,
    vinTakenBy: vinTakenBy,
    knownBy: knownBy,
  };
})();
