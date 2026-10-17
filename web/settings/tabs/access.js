// Settings → Access: let an AI assistant reach this home, with no phone app.
//
// An assistant such as Claude or ChatGPT connects to FTW Cloud's MCP door,
// and FTW Cloud's consent page shows a short code. The owner types it here.
// This page asks FTW Cloud's code lookup, from the owner's browser, for the
// signed offer the code stands for, and asks the box to read it. The box
// checks FTW Cloud's signature against the keys built into it, so what the
// owner sees here is what the box read, not what a web page claimed. The
// owner then grants it, or less of it, never more.
//
// Granting needs another device on the home network, signed in with the
// house password. With it off, every device on the Wi-Fi is an owner with no
// sign-in, and letting the outside world in must not be one click from any of
// them; on the box itself the password does not count. The box refuses; this
// page says so before it asks for a code, because a code works once and
// should not be spent on a refusal. Removing access and declining a request
// stay open, as removing a phone does.
//
// Names that come from outside — the assistant's name, its host, its own
// words in a request — only ever go into the page as text.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // What each scope lets an assistant do, in this page's words. The box names
  // the scopes and says which change something; a name with no words here
  // still shows, as itself.
  var SCOPE_WORDS = {
    "ftw.live.read": "See power, battery and the car right now",
    "ftw.plan.read": "See the plan and why it looks that way",
    "ftw.history.read": "See past readings",
    "ftw.health.read": "See how the box and its devices are doing",
    "ftw.events.read": "See alerts and events",
    "ftw.logs.read": "Read the box's logs, with names and secrets taken out",
    "ftw.config.read": "Read the settings, with names and secrets taken out",
    "ftw.diag.run": "Run checks that change nothing",
    "ftw.dispatch.hold": "Act at once, for a few minutes at a time, within these limits",
    "ftw.goals.request": "Ask for lasting changes, each waiting for your yes here",
  };

  var OP_WORDS = {
    "loadpoint.hold": "Charge the car now",
    "site.mode.set": "Change the operating mode",
    "planner.prefs.set": "Change how the plan works",
    "loadpoint.soc.set": "Correct the car's charge level",
    "loadpoint.surplus_only.set": "Charge the car from solar only",
    "car.goal.set": "Set how and when a car charges",
    "car.soc.set": "Set a car's charge level",
  };

  var DAY_S = 24 * 3600;

  // The box's answers, kept while Settings is open: the grant on screen
  // holds a code that worked once, and switching tabs must not lose it.
  var state = {
    list: null,      // GET /api/agents
    unavailable: "", // why the box has no agents, when it has none
    auth: null,      // GET /api/auth/status
    held: null,      // {raw, view, lookup, fetchedAt}
    note: null,      // {text, kind} under the code field
    done: null,      // what the last grant gave
    editing: "",     // the row being changed
  };

  // ---- pure helpers, tested in access.test.mjs ----

  // A code as an owner may type it: any case, with a dash or spaces.
  function normalizeCode(s) {
    var code = String(s || "").toUpperCase().replace(/[\s-]/g, "");
    return /^[A-Z0-9]{8}$/.test(code) ? code : "";
  }

  // Text from outside the box. Control, direction, joining, variation and tag
  // marks go, so it cannot turn the page's own words around it or hide
  // letters, and it is cut to max characters.
  function cleanText(s, max) {
    var text = String(s || "")
      .replace(/[\u0000-\u001f\u007f-\u009f\u00ad\u034f\u061c\u200b-\u200f\u2028-\u202e\u2060-\u206f\ufe00-\ufe0f\ufeff\u{e0000}-\u{e007f}]/gu, "")
      .trim();
    // By code point, so a cut never splits an emoji into half a character.
    var chars = Array.from(text);
    return chars.length > max ? chars.slice(0, max - 1).join("") + "…" : text;
  }

  // An assistant names itself, at a length a row can hold. Double quote marks
  // go too: the page puts the name in quotes, and one in the name could end
  // them early and make the page's own words look like the name's.
  function cleanName(s) { return cleanText(String(s || "").replace(/["“”„‟«»″＂]/g, ""), 64); }

  // A name like Sourceful's or FTW's, from a client FTW Cloud does not know.
  function borrowsOurName(name, verified) {
    return !verified && /sourceful|ftw/i.test(name);
  }

  function scopeWords(scope) { return SCOPE_WORDS[scope] || scope; }
  function opWords(op) { return OP_WORDS[op] || op; }

  // The host a row's client id names, when the id is a metadata document's
  // address. A client that registered itself has an id that names nothing.
  function clientHost(clientId) {
    var m = /^https:\/\/([^/?#:]+)/i.exec(String(clientId || ""));
    return m ? m[1].toLowerCase() : "";
  }

  // Splits what an offer or a row holds into what it reads and what it may
  // change, by the box's own list of scopes that change something. Control
  // ops go under the scope that opens them; a control scope with no op
  // grants nothing, so it is left out.
  function splitGrant(scopes, ops, registry) {
    var write = (registry && registry.writeScopes) || [];
    var agentOps = (registry && registry.agentOps) || {};
    var reads = [], groups = [];
    // In the page's order, which runs from now to the past to the box itself;
    // a scope the page has no words for goes last.
    var order = Object.keys(SCOPE_WORDS);
    var rank = function (scope) { var i = order.indexOf(scope); return i < 0 ? order.length : i; };
    (scopes || []).slice().sort(function (a, b) { return rank(a) - rank(b); }).forEach(function (scope) {
      if (write.indexOf(scope) < 0) {
        reads.push(scope);
        return;
      }
      var opOrder = Object.keys(OP_WORDS);
      var opRank = function (op) { var i = opOrder.indexOf(op); return i < 0 ? opOrder.length : i; };
      var under = (ops || []).filter(function (op) { return agentOps[op] === scope; })
        .sort(function (a, b) { return opRank(a) - opRank(b); });
      if (under.length) groups.push({ scope: scope, ops: under });
    });
    return { reads: reads, groups: groups };
  }

  // An op runs at once, under the grant's caps, when one of its arguments is
  // bounded by a cap; the others wait for the owner.
  function capped(op, registry) {
    var args = ((registry && registry.agentArgs) || {})[op] || [];
    return args.some(function (a) { return !!a.cap; });
  }

  function sortedUnique(list) {
    var out = [];
    list.slice().sort().forEach(function (v) { if (out[out.length - 1] !== v) out.push(v); });
    return out;
  }

  // The grant the owner chose, as POST /api/agents and /widen take it. choice
  // is {reads, ops, holdMin, maxKW, ttlS}. The scopes are the reads ticked and
  // the scope of each op ticked; holds need caps, and caps without holds are
  // zero, as the box insists.
  function chosenGrant(choice, perMin, registry) {
    var agentOps = (registry && registry.agentOps) || {};
    var ops = sortedUnique(choice.ops || []);
    var scopes = (choice.reads || []).slice();
    ops.forEach(function (op) { if (agentOps[op]) scopes.push(agentOps[op]); });
    scopes = sortedUnique(scopes);
    var caps = { holdS: 0, maxW: 0, perMin: perMin || 0 };
    if (ops.some(function (op) { return capped(op, registry); })) {
      caps.holdS = Math.round((Number(choice.holdMin) || 0) * 60);
      caps.maxW = Math.round((Number(choice.maxKW) || 0) * 1000);
    }
    return { scopes: scopes, ops: ops, caps: caps, ttlS: Math.floor(Number(choice.ttlS) || 0) };
  }

  // What is wrong with a choice before the box is asked, or "".
  function choiceProblem(grant, limits) {
    if (!grant.scopes.length) return "Tick at least one thing it may do.";
    if (grant.caps.holdS || grant.caps.maxW) {
      if (grant.caps.holdS < 60 || grant.caps.holdS > limits.holdS) {
        return "A hold lasts from 1 to " + Math.floor(limits.holdS / 60) + " minutes.";
      }
      if (grant.caps.maxW < 1 || grant.caps.maxW > limits.maxW) {
        return "The power goes up to " + kwExact(limits.maxW) + " kW.";
      }
    }
    if (grant.ttlS < 60) return "Choose how long it may last.";
    return "";
  }

  function kw(w) {
    var v = (Number(w) || 0) / 1000;
    return v % 1 === 0 ? String(v) : v.toFixed(1);
  }

  // A power cap in kW, exact to the watt, for a field the owner edits: a cap
  // of 3,680 W must read back as 3,680 W, not as 3.7 kW the box refuses.
  function kwExact(w) {
    return String(Math.round(Number(w) || 0) / 1000);
  }

  // How long a grant may last: the lengths the page offers, none longer than
  // the offer asked for.
  function lengthOptions(maxS) {
    var out = [DAY_S, 7 * DAY_S, 30 * DAY_S].filter(function (s) { return s <= maxS; });
    if (!out.length && maxS >= 60) out.push(maxS);
    return out;
  }

  // The length chosen until the owner picks another: a week, or less when the
  // offer asks for less. A week is long enough to try an assistant and short
  // enough that one forgotten here ends; renewing is a button on this page.
  function defaultLength(options) {
    var week = options.filter(function (s) { return s <= 7 * DAY_S; });
    return week.length ? week[week.length - 1] : options[0];
  }

  function lengthText(s) {
    if (s % DAY_S === 0) return s === DAY_S ? "1 day" : s / DAY_S + " days";
    if (s >= 3600) return Math.round(s / 3600) + " hours";
    return Math.round(s / 60) + " minutes";
  }

  function whenText(ms) {
    if (!ms) return "—";
    return new Date(ms).toLocaleString(undefined, {
      day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit",
    });
  }

  function clockText(ms) {
    return new Date(ms).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }

  function agoText(ms, nowMs) {
    if (!ms) return "not used yet";
    var d = (nowMs || Date.now()) - ms;
    if (d < 90 * 1000) return "used just now";
    if (d < 90 * 60 * 1000) return "used " + Math.round(d / 60000) + " min ago";
    if (d < 36 * 3600 * 1000) return "used " + Math.round(d / 3600000) + " h ago";
    return "used " + Math.round(d / 86400000) + " days ago";
  }

  // What stops a grant before the owner types a code, or null. The order is
  // the order to fix them in.
  function readiness(list, unavailable, auth) {
    if (unavailable) return { kind: "unavailable" };
    if (!list) return { kind: "loading" };
    if (!auth || !auth.lan_auth || !auth.configured) return { kind: "password" };
    var box = list.box || {};
    // The box's answer for this very request: on the box itself, or through
    // an SSH tunnel to it, the house password does not count.
    if (!box.widen) return { kind: "here" };
    if (!box.offers) return { kind: "keys" };
    if (box.clock === "behind") return { kind: "behind", nowMs: box.nowMs, markMs: box.markMs };
    if (box.clock !== "ok") return { kind: "clock" };
    if (!box.lookup) return { kind: "keys" };
    return null;
  }

  var KEYS_TEXT = "This version of FTW cannot let assistants in yet: it does not know FTW " +
    "Cloud's signing key. An update brings it.";
  var HERE_TEXT = "Open this page from another device on your home network, and sign in with " +
    "the house password there. On the box itself, or through a tunnel to it, the password " +
    "does not count, because anything running on the box could use it too.";
  var CLOCK_TEXT = "The box's clock is not set, so it cannot time an assistant's access. It sets " +
    "itself over the internet; check that the box is online.";

  // Why FTW Cloud found nothing for a code, by its answer.
  function lookupFailureText(status) {
    if (status === 404) {
      return "No request is waiting with that code. Check it against the assistant's page. " +
        "A code works once, for ten minutes; if it ran out, connect again from the " +
        "assistant for a new one.";
    }
    if (status === 429) return "Too many tries. Wait a minute, then try again.";
    if (status === 0) {
      // A browser hides why a request to another site failed, so a lookup
      // FTW Cloud refused without saying who may read the answer looks the
      // same as no network at all.
      return "No answer from FTW Cloud. Check the code against the assistant's page, and " +
        "that this device is online, then try again.";
    }
    return "FTW Cloud answered " + status + ". Try again in a moment.";
  }

  // Why the box will not take an offer, from its answer. lookup is what FTW
  // Cloud said with the offer; nowMs is this device's clock.
  function offerRefusalText(body, lookup, nowMs) {
    body = body || {};
    switch (body.reason) {
      case "no_offer_keys":
        return KEYS_TEXT;
      case "unsigned":
        return "FTW Cloud's signature on this offer does not match a key this box knows. If " +
          "FTW is up to date, connect again from the assistant for a new code.";
      case "expired": {
        var cloudEnd = lookup && lookup.expiresAtMs;
        var boxMs = Number(body.boxMs) || 0;
        // FTW Cloud and this device still count the offer as good, and the
        // box reads another time: the box's clock is off, not the offer.
        if (cloudEnd && nowMs < cloudEnd && boxMs && Math.abs(boxMs - nowMs) > 60 * 1000) {
          return "The box's clock reads " + clockText(boxMs) + ", but this device reads " +
            clockText(nowMs) + ", so the box counts the offer as run out. The box sets its " +
            "clock over the internet; check that it is online, then connect again from the " +
            "assistant for a new code.";
        }
        return "This offer has run out. Connect again from the assistant for a new code.";
      }
      case "other_home":
        return "This code is for another FTW home. Type it on the page of the home you meant " +
          "to connect.";
      case "malformed":
        return "The box cannot read this offer. Connect again from the assistant for a new " +
          "code; if it happens again, FTW may need an update.";
    }
    return refusalText(body);
  }

  // A refusal the box gave as a code, or a sentence.
  function refusalText(body) {
    body = body || {};
    var args = body.args || {};
    switch (body.error) {
      case "E_UNAVAILABLE":
        return args.subsystem === "clock" ? CLOCK_TEXT : "The box cannot do that right now.";
      case "E_CAP_EXCEEDED":
        if (args.cap === "agents") return "This box already lets in " + args.max + " assistants. Remove one first.";
        if (args.cap === "expiresAtMs") return "That is longer than this kind of access may last.";
        if (args.cap === "holdS") return "A hold lasts from 1 to " + Math.floor((args.max || 1800) / 60) + " minutes.";
        if (args.cap === "maxW") return "The power is out of range.";
        return "That is past a limit the box keeps.";
      case "E_UNKNOWN_OP":
        return args.field === "key"
          ? "This assistant already has access here."
          : "The box did not accept that grant.";
      case "E_NOT_FOUND":
        return "That is gone already. The list below is up to date.";
      case "E_CONFLICT":
        return "Someone changed this access a moment ago. Look at it again, then change it.";
      case "E_SCOPE_DENIED":
      case "E_OP_NOT_GRANTED":
        return "The box did not accept that grant.";
    }
    return typeof body.error === "string" && body.error ? body.error : "That did not work.";
  }

  // A request's arguments, in the registry's order and by the labels it
  // gives them, as the box sent them. A charger or car goes by the name the
  // box gave it, and by its id only when the box no longer knows it. One the
  // registry does not name comes last, by its own name.
  function requestArgs(req, registry) {
    var known = ((registry && registry.agentArgs) || {})[req.op] || [];
    var args = req.args || {};
    var names = req.names || {};
    var out = [];
    known.forEach(function (a) {
      if (!Object.prototype.hasOwnProperty.call(args, a.name)) return;
      var named = (a.kind === "loadpoint" || a.kind === "car") && Object.prototype.hasOwnProperty.call(names, a.name);
      var name = named ? cleanText(names[a.name], 80) : "";
      out.push({ label: a.label || a.name, value: name || argValue(args[a.name], a) });
    });
    Object.keys(args).sort().forEach(function (name) {
      if (!known.some(function (a) { return a.name === name; })) out.push({ label: cleanText(name, 40), value: argValue(args[name], {}) });
    });
    return out;
  }

  function argValue(v, arg) {
    if (typeof v === "boolean") return v ? "yes" : "no";
    if (typeof v !== "number") return cleanText(v, 80);
    if (!Number.isFinite(v)) return "?";
    if (arg.kind === "ratio") return Math.round(v * 100) + " %";
    if (arg.unit === "W") return kw(v) + " kW";
    if (arg.unit === "s") return Math.round(v / 60) + " min";
    // The box's convention: a time of day is minutes after midnight, in an
    // argument named _min.
    if (/_min$/.test(arg.name || "") && v >= 0 && v < 1440) {
      return String(Math.floor(v / 60)).padStart(2, "0") + ":" + String(v % 60).padStart(2, "0");
    }
    return String(v);
  }

  // How a request the owner approved ended, from the box's command result.
  function resultText(answer) {
    answer = answer || {};
    if (answer.state === "declined") return "Declined. The assistant hears that you said no.";
    var r = answer.result || {};
    var st = r.State || r.state || "";
    var err = r.Error || r.error || {};
    if (st === "applied") return "Approved. The box made the change.";
    if (st === "rejected") return "Approved, but the box refused the change" + (err.Code || err.code ? " (" + (err.Code || err.code) + ")." : ".");
    if (st === "unconfirmed") return "Approved. The box sent the change but could not confirm it took.";
    return "Approved." + (st ? " Outcome: " + st + "." : "");
  }

  // ---- talking to the box ----

  function readJSON(r) {
    return r.json().catch(function () { return {}; });
  }

  function load() {
    return Promise.all([
      fetch("/api/agents").then(function (r) {
        return readJSON(r).then(function (body) {
          if (r.status === 503) return { unavailable: true };
          if (!r.ok) throw new Error(body.error || "HTTP " + r.status);
          return body;
        });
      }),
      fetch("/api/auth/status").then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; }),
    ]).then(function (got) {
      state.unavailable = got[0].unavailable ? "off" : "";
      state.list = got[0].unavailable ? null : got[0];
      state.auth = got[1];
    });
  }

  function postJSON(path, body) {
    return fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  // ---- drawing ----

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

  function quoted(name) {
    return "“" + (cleanName(name) || "An assistant") + "”";
  }

  function draw(ctx) {
    drawReady(ctx);
    drawHeld();
    drawRequests();
    drawRows();
  }

  function drawReady(ctx) {
    var slot = document.getElementById("access-ready");
    var input = document.getElementById("access-code");
    var button = document.getElementById("access-lookup");
    if (!slot) return;
    slot.textContent = "";
    var problem = readiness(state.list, state.unavailable, state.auth);
    var blocked = !!problem;
    if (input) input.disabled = blocked;
    if (button) button.disabled = blocked;
    if (!problem) return;

    var box = el("div", { className: "access-blocked", role: "status" });
    slot.appendChild(box);
    switch (problem.kind) {
      case "loading":
        box.className = "access-quiet";
        box.textContent = "Checking the box…";
        return;
      case "unavailable":
        box.appendChild(el("p", { text: "Assistants reach this box over the same encrypted link as the " +
          "FTW app, and it is off. Turn on “Let the FTW app connect to this box” under FTW app, " +
          "save, and restart." }));
        box.appendChild(el("button", { type: "button", className: "btn-send", text: "Open FTW app settings",
          onclick: function () { ctx.navigateTab("app"); } }));
        return;
      case "password":
        box.appendChild(el("p", { text: "Turn on the house password first. Without it, any device on " +
          "your Wi-Fi could let an assistant into your home." }));
        box.appendChild(el("button", { type: "button", className: "btn-send", text: "Set a house password",
          onclick: function () { ctx.navigateTab("system"); } }));
        box.appendChild(el("p", { className: "access-quiet", text: "It is under System, LAN password. " +
          "Come back here once it is on." }));
        return;
      case "here":
        box.appendChild(el("p", { text: HERE_TEXT }));
        return;
      case "keys":
        box.appendChild(el("p", { text: KEYS_TEXT }));
        return;
      case "clock":
        box.appendChild(el("p", { text: CLOCK_TEXT }));
        return;
      case "behind":
        box.appendChild(el("p", { text: "The box's clock reads " + whenText(problem.nowMs) +
          ", which is before " + whenText(problem.markMs) + ", a time it has already seen. " +
          "Assistants stay locked out until the clock passes that time, so a clock set back " +
          "cannot bring back access that ran out." }));
        box.appendChild(el("p", { text: "If the clock is right now, reset the mark. Every assistant " +
          "loses its access and must be let in again." }));
        var reset = el("button", { type: "button", className: "btn-ghost", text: "The clock is right: reset the mark" });
        reset.addEventListener("click", function () {
          if (!confirm("Reset the clock mark? Every assistant loses its access now.")) return;
          reset.disabled = true;
          postJSON("/api/agents/clock-reset", {}).then(function (r) {
            if (r.ok) return refresh(ctx);
            return readJSON(r).then(function (body) {
              box.appendChild(el("p", { className: "access-error", text: refusalText(body) }));
              reset.disabled = false;
            });
          }).catch(function () { reset.disabled = false; });
        });
        box.appendChild(reset);
        return;
    }
  }

  function setNote(text, kind) {
    state.note = text ? { text: text, kind: kind || "" } : null;
    var slot = document.getElementById("access-note");
    if (!slot) return;
    slot.textContent = state.note ? state.note.text : "";
    slot.className = "access-note" + (state.note && state.note.kind ? " access-" + state.note.kind : "");
  }

  function lookUp(ctx) {
    var input = document.getElementById("access-code");
    var button = document.getElementById("access-lookup");
    if (!input || !button || !state.list) return;
    var code = normalizeCode(input.value);
    state.done = null;
    if (!code) {
      setNote("A code has eight letters and digits, like ABCD-EFGH.", "error");
      input.focus();
      return;
    }
    button.disabled = true;
    setNote("Asking FTW Cloud…");
    var lookup = null;
    fetch(state.list.box.lookup + "?code=" + encodeURIComponent(code), {
      cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer",
    })
      .catch(function () { return { ok: false, status: 0 }; })
      .then(function (r) {
        if (!r.ok) throw new Error(lookupFailureText(r.status));
        return r.json();
      })
      .then(function (body) {
        lookup = body || {};
        if (lookup.status && lookup.status !== "pending") {
          throw new Error("That request is " + cleanText(lookup.status, 20) + ". Connect again from the assistant for a new code.");
        }
        if (!lookup.offer) throw new Error("FTW Cloud sent no offer for that code.");
        setNote("Asking the box to check it…");
        return postJSON("/api/agents/offer", { offer: lookup.offer });
      })
      .then(function (r) {
        return readJSON(r).then(function (body) {
          if (!r.ok) throw new Error(offerRefusalText(body, lookup, Date.now()));
          state.held = { raw: lookup.offer, view: body, lookup: lookup, fetchedAt: Date.now() };
          input.value = "";
          setNote("");
          drawHeld();
        });
      })
      .catch(function (e) {
        setNote(e.message || String(e), "error");
      })
      .then(function () {
        button.disabled = !!readiness(state.list, state.unavailable, state.auth);
      });
  }

  function checkRow(name, value, label, checked, extra) {
    var box = el("input", { type: "checkbox", name: name, value: value });
    box.checked = !!checked;
    return el("label", { className: "checkbox-row access-check" }, [box, el("span", { text: label }), extra || null]);
  }

  function numberField(id, label, value, max, min, step, unit) {
    var input = el("input", { type: "number", id: id, min: String(min), max: String(max), step: String(step), value: String(value) });
    return el("div", { className: "access-cap" }, [
      el("label", { for: id, text: label }),
      el("div", { className: "access-cap-input" }, [input, el("span", { className: "access-quiet",
        text: unit + ", at most " + Number(max).toLocaleString(undefined, { maximumFractionDigits: 3 }) })]),
    ]);
  }

  // The choices for a grant: reads ticked, control off until the owner turns
  // it on, caps no higher than given, and a length. limits are the most it
  // may be given; current, when set, is a row being changed.
  function grantForm(form, given, registry, current) {
    var split = splitGrant(given.scopes, given.ops, registry);
    form.appendChild(el("p", { className: "access-heading", text: current ? "It may" : "It asks to" }));
    split.reads.forEach(function (scope) {
      form.appendChild(checkRow("read", scope, scopeWords(scope), true));
    });

    if (split.groups.length) {
      var controlOn = !!current;
      var control = el("input", { type: "checkbox", id: "access-control-" + (current ? current.id : "new") });
      control.checked = controlOn;
      var label = current ? "It may control" : "Also let it control";
      form.appendChild(el("label", { className: "checkbox-row access-control-toggle" }, [control, el("span", { text: label })]));
      if (!current) {
        form.appendChild(el("p", { className: "access-quiet", text: "Off unless you turn it on. A hold is " +
          "short and capped, and a lasting change waits for your yes here." }));
      }
      var detail = el("div", { className: "access-control" });
      split.groups.forEach(function (g) {
        detail.appendChild(el("p", { className: "access-subheading", text: scopeWords(g.scope) }));
        g.ops.forEach(function (op) { detail.appendChild(checkRow("op", op, opWords(op), true)); });
        if (g.ops.some(function (op) { return capped(op, registry); })) {
          var holdMin = Math.floor((given.caps.holdS || 0) / 60);
          detail.appendChild(numberField("access-hold-" + (current ? current.id : "new"), "Longest hold",
            holdMin, holdMin, 1, 1, "minutes"));
          detail.appendChild(numberField("access-power-" + (current ? current.id : "new"), "Highest power",
            kwExact(given.caps.maxW), kwExact(given.caps.maxW), 0.001, "any", "kW"));
        }
      });
      if (given.caps.perMin) {
        detail.appendChild(el("p", { className: "access-quiet", text: "At most " + given.caps.perMin +
          " holds or requests a minute." }));
      }
      detail.hidden = !controlOn;
      control.addEventListener("change", function () { detail.hidden = !control.checked; });
      form.appendChild(detail);
    }
    return { perMin: given.caps.perMin || 0 };
  }

  function readChoice(form, current) {
    var reads = [], ops = [];
    form.querySelectorAll('input[name="read"]').forEach(function (b) { if (b.checked) reads.push(b.value); });
    var control = form.querySelector('[id^="access-control-"]');
    if (!control || control.checked) {
      form.querySelectorAll('input[name="op"]').forEach(function (b) { if (b.checked) ops.push(b.value); });
    }
    var suffix = current ? current.id : "new";
    var hold = document.getElementById("access-hold-" + suffix);
    var power = document.getElementById("access-power-" + suffix);
    var length = form.querySelector('select[name="length"]');
    return {
      reads: reads, ops: ops,
      holdMin: hold ? hold.value : 0, maxKW: power ? power.value : 0,
      length: length ? length.value : "",
    };
  }

  function drawHeld() {
    var slot = document.getElementById("access-offer");
    if (!slot) return;
    slot.textContent = "";
    if (state.done) {
      slot.appendChild(el("div", { className: "access-card access-done", role: "status" }, [
        el("p", { text: quoted(state.done.client) + " can reach this home until " + whenText(state.done.until) + "." }),
        el("p", { className: "access-quiet", text: "The assistant's page moves on by itself once FTW Cloud " +
          "hears from this box. It is listed below, where you can change or remove it." }),
      ]));
      return;
    }
    var held = state.held;
    if (!held) return;
    var v = held.view;
    var registry = (state.list && state.list.registry) || {};
    var name = cleanName(v.client);
    var card = el("form", { className: "access-card" });
    slot.appendChild(card);

    var title = v.role === "support"
      ? "Let Sourceful support into your home, for ticket " + cleanName(v.ticket) + "?"
      : "Let " + quoted(name) + " into your home?";
    card.appendChild(el("h3", { className: "access-title", text: title }));
    card.appendChild(el("p", {
      className: v.verified ? "access-quiet" : "access-warn",
      text: v.verified
        ? "A client FTW Cloud knows, from " + (v.clientHost || "an address it checked") + "."
        : "FTW Cloud does not know this client. The name is what it calls itself" +
          (v.clientHost ? "; it comes from " + v.clientHost : "") + ". Let it in only if you just connected it.",
    }));
    if (borrowsOurName(name, v.verified)) {
      card.appendChild(el("p", { className: "access-warn", text: "It uses a name like Sourceful's or FTW's. " +
        "Neither asks for access this way." }));
    }
    card.appendChild(el("p", { className: "access-quiet", text: "Code " + (v.code ? v.code.slice(0, 4) + "-" + v.code.slice(4) : "") +
      ". Check that it matches the assistant's page." }));

    var form = grantForm(card, v, registry, null);
    var lengths = lengthOptions(v.ttlS || 0);
    var chosen = defaultLength(lengths);
    var select = el("select", { name: "length", id: "access-length" });
    lengths.forEach(function (s) {
      var opt = el("option", { value: String(s), text: lengthText(s) });
      if (s === chosen) opt.selected = true;
      select.appendChild(opt);
    });
    card.appendChild(el("label", { for: "access-length", text: "For how long" }));
    card.appendChild(select);
    card.appendChild(el("p", { className: "access-quiet", text: "Decide before " + clockText(v.expMs) +
      ", or ask the assistant for a new code. You can remove it here at any time." }));

    var err = el("p", { className: "access-error", role: "alert" });
    var give = el("button", { type: "submit", className: "btn-send", text: "Give access" });
    // The offer stays while something above stops a grant, such as the house
    // password turned off since: its code worked once.
    give.disabled = !!readiness(state.list, state.unavailable, state.auth);
    var not = el("button", { type: "button", className: "btn-ghost", text: "Not now" });
    card.appendChild(el("div", { className: "access-actions" }, [give, not]));
    card.appendChild(err);
    not.addEventListener("click", function () {
      state.held = null;
      drawHeld();
    });
    card.addEventListener("submit", function (e) {
      e.preventDefault();
      var c = readChoice(card, null);
      var grant = chosenGrant({ reads: c.reads, ops: c.ops, holdMin: c.holdMin, maxKW: c.maxKW, ttlS: c.length },
        form.perMin, registry);
      var why = choiceProblem(grant, v.caps);
      if (why) {
        err.textContent = why;
        return;
      }
      err.textContent = "";
      give.disabled = true;
      postJSON("/api/agents", { offer: held.raw, scopes: grant.scopes, ops: grant.ops, caps: grant.caps, ttlS: grant.ttlS })
        .then(function (r) {
          return readJSON(r).then(function (body) {
            if (!r.ok) throw new Error(body.reason ? offerRefusalText(body, held.lookup, Date.now()) : refusalText(body));
            state.held = null;
            state.done = { client: v.role === "support" ? "Sourceful support" : name, until: body.expiresAtMs };
            return refresh();
          });
        })
        .catch(function (e) {
          err.textContent = e.message || String(e);
          give.disabled = false;
        });
    });
  }

  function rowName(row) {
    var p = row.principal || {};
    if (row.role === "support") return "Sourceful support" + (p.ticket ? ", ticket " + cleanName(p.ticket) : "");
    if (row.role === "release") return "The release check";
    return quoted(p.client);
  }

  function boxNow() {
    var box = (state.list && state.list.box) || {};
    return box.nowMs ? box.nowMs + (Date.now() - (state.list._at || Date.now())) : Date.now();
  }

  // Renewing, narrowing and approving need the house password, as granting
  // does. Without it the buttons say so rather than meet a refusal.
  function canWiden() {
    return !!(state.list && state.list.box && state.list.box.widen);
  }

  function drawRows() {
    var slot = document.getElementById("access-rows");
    if (!slot) return;
    slot.textContent = "";
    var rows = (state.list && state.list.agents) || [];
    var section = document.getElementById("access-rows-section");
    if (section) section.hidden = !state.list;
    if (!rows.length) {
      slot.appendChild(el("p", { className: "access-quiet", text: "No assistant has access." }));
      return;
    }
    var registry = state.list.registry || {};
    var now = boxNow();
    rows.slice().sort(function (a, b) { return (b.lastSeenMs || 0) - (a.lastSeenMs || 0); }).forEach(function (row) {
      var split = splitGrant(row.scopes, row.ops, registry);
      var item = el("div", { className: "access-row" });
      var host = clientHost((row.principal || {}).clientId);
      item.appendChild(el("div", { className: "access-row-head" }, [
        el("span", { className: "access-row-name", text: rowName(row) }),
        host ? el("span", { className: "access-quiet mono", text: host }) : null,
      ]));
      var ended = row.expiresAtMs && row.expiresAtMs <= now;
      item.appendChild(el("p", { className: "access-quiet", text: [
        split.groups.length ? "Reads and controls" : "Reads",
        ended ? "ran out " + whenText(row.expiresAtMs) : "until " + whenText(row.expiresAtMs),
        agoText(row.lastSeenMs, now),
        row.approvedBy === "lan" ? "let in on this page" : "let in from a phone",
      ].join(" · ") }));

      var list = el("ul", { className: "access-may" });
      split.reads.forEach(function (s) { list.appendChild(el("li", { text: scopeWords(s) })); });
      split.groups.forEach(function (g) {
        g.ops.forEach(function (op) { list.appendChild(el("li", { text: opWords(op) })); });
      });
      item.appendChild(el("details", {}, [el("summary", { text: "What it may do" }), list]));

      var actions = el("div", { className: "access-actions" });
      var change = el("button", { type: "button", className: "btn-ghost", text: state.editing === row.id ? "Close" : "Renew or narrow" });
      change.disabled = !canWiden();
      if (change.disabled) change.title = "Changing access needs another device on your network, signed in with the house password.";
      change.addEventListener("click", function () {
        state.editing = state.editing === row.id ? "" : row.id;
        drawRows();
      });
      var remove = el("button", { type: "button", className: "btn-remove", text: "Remove" });
      remove.addEventListener("click", function () {
        if (!confirm("Remove " + rowName(row) + "? It loses access at once, and any hold it set ends.")) return;
        remove.disabled = true;
        fetch("/api/agents/" + encodeURIComponent(row.id), { method: "DELETE" }).then(function (r) {
          if (r.ok || r.status === 404) return refresh();
          return readJSON(r).then(function (body) {
            item.appendChild(el("p", { className: "access-error", text: refusalText(body) }));
            remove.disabled = false;
          });
        }).catch(function () { remove.disabled = false; });
      });
      actions.appendChild(change);
      actions.appendChild(remove);
      item.appendChild(actions);
      if (state.editing === row.id) item.appendChild(editForm(row, registry, now));
      slot.appendChild(item);
    });
  }

  // Renew or narrow one row. It may keep what it has or less of it, for as
  // long as before or from now. Widening past what it has is an offer's job.
  function editForm(row, registry, now) {
    var form = el("form", { className: "access-card access-edit" });
    var given = { scopes: row.scopes || [], ops: row.ops || [], caps: row.caps || {} };
    var f = grantForm(form, given, registry, row);
    var select = el("select", { name: "length", id: "access-length-" + row.id });
    var left = Math.floor((row.expiresAtMs - now) / 1000);
    if (left > 60) select.appendChild(el("option", { value: "keep", text: "Keep the end, " + whenText(row.expiresAtMs) }));
    [DAY_S, 7 * DAY_S, 30 * DAY_S].forEach(function (s) {
      select.appendChild(el("option", { value: String(s), text: lengthText(s) + " from now" }));
    });
    form.appendChild(el("label", { for: select.id, text: "How long" }));
    form.appendChild(select);
    var err = el("p", { className: "access-error", role: "alert" });
    var save = el("button", { type: "submit", className: "btn-send", text: "Save" });
    form.appendChild(el("div", { className: "access-actions" }, [save]));
    form.appendChild(err);
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var c = readChoice(form, row);
      var ttlS = c.length === "keep" ? Math.floor((row.expiresAtMs - boxNow()) / 1000) : Number(c.length);
      var grant = chosenGrant({ reads: c.reads, ops: c.ops, holdMin: c.holdMin, maxKW: c.maxKW, ttlS: ttlS },
        f.perMin, registry);
      var why = choiceProblem(grant, { holdS: (row.caps || {}).holdS || 0, maxW: (row.caps || {}).maxW || 0 });
      if (why) {
        err.textContent = why;
        return;
      }
      save.disabled = true;
      postJSON("/api/agents/" + encodeURIComponent(row.id) + "/widen", {
        epoch: row.epoch, scopes: grant.scopes, ops: grant.ops, caps: grant.caps, ttlS: grant.ttlS,
      }).then(function (r) {
        return readJSON(r).then(function (body) {
          if (!r.ok) throw new Error(refusalText(body));
          state.editing = "";
          return refresh();
        });
      }).catch(function (e2) {
        err.textContent = e2.message || String(e2);
        save.disabled = false;
      });
    });
    return form;
  }

  function drawRequests() {
    var slot = document.getElementById("access-requests");
    var section = document.getElementById("access-requests-section");
    if (!slot || !section) return;
    slot.textContent = "";
    var pending = ((state.list && state.list.requests) || []).filter(function (r) { return r.state === "pending"; });
    section.hidden = pending.length === 0;
    var registry = (state.list && state.list.registry) || {};
    var rows = (state.list && state.list.agents) || [];
    pending.forEach(function (req) {
      var row = rows.filter(function (a) { return a.id === req.agent; })[0];
      // Two assistants may call themselves the same, so the host goes beside
      // the name, as in the list below.
      var host = row ? clientHost((row.principal || {}).clientId) : "";
      var who = (row ? rowName(row) : "An assistant") + (host ? " (" + host + ")" : "");
      var item = el("div", { className: "access-row access-request" });
      item.appendChild(el("p", { className: "access-row-name", text: who + " asks to: " + opWords(req.op).replace(/^./, function (c) { return c.toLowerCase(); }) }));
      var args = requestArgs(req, registry);
      if (args.length) {
        var dl = el("dl", { className: "access-args" });
        args.forEach(function (a) {
          dl.appendChild(el("dt", { text: a.label }));
          dl.appendChild(el("dd", { text: a.value }));
        });
        item.appendChild(dl);
      }
      if (req.why) {
        item.appendChild(el("p", { className: "access-why" }, [
          el("span", { className: "access-quiet", text: "In its own words: " }),
          el("q", { text: cleanText(req.why, 280) }),
        ]));
      }
      item.appendChild(el("p", { className: "access-quiet", text: "Waits until " + whenText(req.expiresAtMs) + "." }));
      var answer = el("p", { className: "access-quiet", role: "status" });
      var approve = el("button", { type: "button", className: "btn-send", text: "Approve" });
      var decline = el("button", { type: "button", className: "btn-ghost", text: "Decline" });
      approve.disabled = !canWiden();
      if (approve.disabled) approve.title = "Approving needs another device on your network, signed in with the house password.";
      function decide(yes) {
        approve.disabled = decline.disabled = true;
        postJSON("/api/agent-requests/" + encodeURIComponent(req.reqId), { approve: yes }).then(function (r) {
          return readJSON(r).then(function (body) {
            if (r.status === 404) {
              answer.className = "access-quiet";
              answer.textContent = "That request is no longer waiting: it was answered or ran out.";
              setTimeout(function () { refresh(); }, 2500);
              return;
            }
            if (!r.ok) throw new Error(refusalText(body));
            answer.className = "access-done";
            answer.textContent = resultText(body);
            setTimeout(function () { refresh(); }, 2500);
          });
        }).catch(function (e) {
          answer.className = "access-error";
          answer.textContent = e.message || String(e);
          approve.disabled = !canWiden();
          decline.disabled = false;
        });
      }
      approve.addEventListener("click", function () { decide(true); });
      decline.addEventListener("click", function () { decide(false); });
      item.appendChild(el("div", { className: "access-actions" }, [approve, decline]));
      item.appendChild(answer);
      slot.appendChild(item);
    });
  }

  var lastCtx = null;

  function refresh(ctx) {
    if (ctx) lastCtx = ctx;
    return load().then(function () {
      if (state.list) state.list._at = Date.now();
      if (document.getElementById("access-ready")) draw(lastCtx);
    }).catch(function (e) {
      var slot = document.getElementById("access-ready");
      if (slot) slot.textContent = "Could not read the box: " + (e.message || e);
    });
  }

  S.tabs.access = {
    render: function () {
      return (
        '<fieldset class="access">' +
        "<legend>Assistants</legend>" +
        '<p class="access-lede">Let an AI assistant, such as Claude or ChatGPT, read this home and, ' +
        "if you allow it, control it within limits. It connects through FTW Cloud. This box " +
        "decides what it may do and checks every call. No phone app is needed.</p>" +
        '<div id="access-ready"></div>' +
        '<form id="access-code-form" class="access-code-form" autocomplete="off">' +
        '<label for="access-code">Code from the assistant\'s page</label>' +
        '<div class="access-code-row">' +
        '<input type="text" id="access-code" class="mono" inputmode="text" autocapitalize="characters" ' +
        'spellcheck="false" maxlength="11" placeholder="ABCD-EFGH" disabled>' +
        '<button type="submit" id="access-lookup" class="btn-send" disabled>Look up</button>' +
        "</div>" +
        '<p class="access-quiet">Connect FTW in your assistant. FTW Cloud then shows a page with a ' +
        "code. Type it here. A code works once, for ten minutes.</p>" +
        '<p id="access-note" class="access-note" aria-live="polite"></p>' +
        "</form>" +
        '<div id="access-offer"></div>' +
        "</fieldset>" +
        '<fieldset id="access-requests-section" hidden>' +
        "<legend>Waiting for your yes</legend>" +
        '<div id="access-requests"></div>' +
        "</fieldset>" +
        '<fieldset id="access-rows-section">' +
        "<legend>Assistants with access</legend>" +
        '<div id="access-rows"><p class="access-quiet">Checking the box…</p></div>' +
        "</fieldset>"
      );
    },

    after: function (ctx) {
      var form = document.getElementById("access-code-form");
      if (form) {
        form.addEventListener("submit", function (e) {
          e.preventDefault();
          lookUp(ctx);
        });
      }
      if (state.note) setNote(state.note.text, state.note.kind);
      draw(ctx);
      refresh(ctx);
    },
  };

  S.tabs.access._pure = {
    SCOPE_WORDS: SCOPE_WORDS,
    OP_WORDS: OP_WORDS,
    normalizeCode: normalizeCode,
    cleanName: cleanName,
    capped: capped,
    borrowsOurName: borrowsOurName,
    clientHost: clientHost,
    splitGrant: splitGrant,
    chosenGrant: chosenGrant,
    choiceProblem: choiceProblem,
    lengthOptions: lengthOptions,
    defaultLength: defaultLength,
    kwExact: kwExact,
    readiness: readiness,
    lookupFailureText: lookupFailureText,
    offerRefusalText: offerRefusalText,
    refusalText: refusalText,
    requestArgs: requestArgs,
    resultText: resultText,
  };
})();
