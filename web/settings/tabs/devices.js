// Settings → Devices tab: driver catalog picker + per-driver editor.
// Owns its own add/remove/connect button wiring; the Settings shell
// stays driver-agnostic.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  function catalogEntryForLua(lua) {
    return lua ? (S.catalogByLua || {})[lua] : null;
  }

  function catalogHasCapability(lua, capability) {
    var entry = catalogEntryForLua(lua);
    var caps = (entry && entry.capabilities) || [];
    return caps.indexOf(capability) >= 0;
  }

  // Some drivers expose more than one fixed register map. Keep these choices
  // in the UI until the signed driver catalog grows a general config-schema
  // field. Version gating prevents an older bundled GoodWe driver from being
  // offered a profile it cannot load.
  var DRIVER_CONFIG_PROFILES = {
    goodwe: [
      { value: "community-v1", label: "Community v1", unitId: 1 },
      { value: "gw8kn-et-hk3000", label: "GW8KN-ET + HK3000", unitId: 247 }
    ]
  };

  function versionAtLeast(version, minimum) {
    var got = String(version || "").match(/^(\d+)\.(\d+)\.(\d+)/);
    var want = String(minimum || "").match(/^(\d+)\.(\d+)\.(\d+)/);
    if (!got || !want) return false;
    for (var i = 1; i <= 3; i++) {
      var g = parseInt(got[i], 10);
      var w = parseInt(want[i], 10);
      if (g !== w) return g > w;
    }
    return true;
  }

  // DRIVER.verification_status, in the same words the setup wizard uses. A
  // driver must not describe its own testing differently depending on which
  // screen you meet it on.
  function verificationLabel(status) {
    if (status === "production") return "verified on hardware";
    if (status === "beta") return "in testing";
    if (status === "experimental") return "untested";
    return "";
  }

  // Every driver runs locally; they are just fetched from different places.
  // So this describes where a file came from, not a mode the driver is in.
  function originLabel(source) {
    if (source === "local") return "your own file";
    if (source === "bundled") return "official, shipped with this build";
    return "official";
  }

  // What is running, in one line. Kept apart from the DOM so the wording can
  // be tested against a real catalog entry rather than by reading the source.
  function runningSummary(entry) {
    if (!entry) return null;
    var source = entry.source || "bundled";
    // An operator's own file carries no version the channel would recognise,
    // so naming one would read as provenance it does not have. Point at the
    // file instead, which is what they would edit or delete.
    var local = source === "local";
    var detail = [local ? entry.path || "" : originLabel(source),
                  verificationLabel(entry.verification_status)].filter(Boolean);
    return {
      source: source,
      headline: local ? "your own file" : "v" + (entry.installed_version || entry.version || "unknown"),
      detail: detail.join(" · "),
      // An override shadows whatever the channel offers, so installing a newer
      // version does not change what runs. Offering Update there would be a
      // lie the operator only discovers by debugging.
      updatable: !!(entry.update_available && entry.repository_id && !local),
      upstreamVersion: entry.upstream_version || ""
    };
  }

  // GET /versions answers with VersionCandidate: {repository_id, driver:{…},
  // installed?}. The version lives on .driver, and whether a candidate is
  // already on disk is .installed -- not a string match against a second list.
  function versionRows(body) {
    var installed = (body && body.installed) || [];
    var available = (body && body.available) || [];
    var rows = [];
    var seen = {};

    // Two repositories can publish the same version number with different
    // content, and ActivateInstalled tells them apart by hash. Keying on the
    // version alone would drop one of them -- possibly the active one. The
    // hash is the artifact's identity, so prefer it: the same file listed as
    // both installed and available is one row, not two.
    function push(row) {
      var key = row.sha256 || row.version + "@" + (row.repositoryID || "");
      if (!row.version || seen[key]) return;
      seen[key] = true;
      rows.push(row);
    }

    available.forEach(function (candidate) {
      if (!candidate) return;
      var driver = candidate.driver || {};
      var onDisk = candidate.installed || null;
      push({
        version: driver.version || "",
        sha256: (onDisk && onDisk.sha256) || driver.sha256 || "",
        // POST /install refuses a request without one; a version by itself
        // does not say which repository signed it.
        repositoryID: candidate.repository_id || "",
        downloaded: !!onDisk,
        active: !!(onDisk && onDisk.active),
        verification: verificationLabel((driver.metadata || {}).verification_status)
      });
    });

    // Anything on disk the channel no longer lists -- an older version whose
    // manifest entry has since been dropped -- is still runnable, and is
    // exactly what someone reaches for when a new driver misbehaves.
    installed.forEach(function (item) {
      if (!item) return;
      push({
        version: item.version || "",
        sha256: item.sha256 || "",
        repositoryID: item.repo_id || "",
        downloaded: true,
        active: !!item.active,
        verification: ""
      });
    });

    return rows;
  }

  // The version list as its own surface, so a test can drive it with a real
  // /versions payload. The previous version of this code read the wrong field
  // and rendered nothing; the test that covered it matched the source with a
  // regex, so it passed while the panel was empty on screen.
  // Everything an operator might type: the manufacturer, the driver's name,
  // and the models it has been run against. tested_models is the one that
  // matters -- you know you own an SH10RT, not that you need "Sungrow SH
  // Hybrid Inverter".
  function catalogHaystack(e) {
    return [e.name, e.manufacturer, e.id, e.filename]
      .concat(e.tested_models || [])
      .concat(e.protocols || [])
      .join(" ").toLowerCase();
  }

  // Proven first. Four of thirty-seven have run on customer hardware and the
  // rest have not; alphabetical order buries that.
  var VERIFICATION_RANK = {production: 0, beta: 1, experimental: 2};

  function catalogRank(e) {
    var rank = VERIFICATION_RANK[e.verification_status];
    return rank === undefined ? 3 : rank;
  }

  // Every term has to match somewhere, so "sungrow hybrid" narrows rather
  // than widens.
  function searchCatalog(entries, query) {
    var terms = String(query || "").trim().toLowerCase().split(/\s+/).filter(Boolean);
    var matches = (entries || []).filter(function (e) {
      if (terms.length === 0) return true;
      var hay = catalogHaystack(e);
      return terms.every(function (t) { return hay.indexOf(t) >= 0; });
    });
    return matches.sort(function (a, b) {
      var byRank = catalogRank(a) - catalogRank(b);
      if (byRank !== 0) return byRank;
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
  }

  // Several drivers already begin with the manufacturer, which read as
  // "CTEK CTEK Chargestorm".
  function catalogTitle(e) {
    var name = e.name || e.filename || e.id || "";
    var maker = e.manufacturer || "";
    if (!maker || name.toLowerCase().indexOf(maker.toLowerCase()) === 0) return name;
    return maker + " " + name;
  }

  S.driverVersions = {
    runningSummary: runningSummary,
    verificationLabel: verificationLabel,
    versionRows: versionRows,
    render: function (panel, driverID, body, opts) {
      return renderVersionPicker(panel, driverID, body, opts);
    },
    renderSource: function (panel, body) { return renderDriverSource(panel, body); },
    unifiedDiff: function (before, after, context) { return unifiedDiff(before, after, context); },
    suggestUpstreamURL: function (body, edited) { return suggestUpstreamURL(body, edited); },
    maxSuggestionURL: function () { return MAX_SUGGESTION_URL; },
    searchCatalog: function (entries, query) { return searchCatalog(entries, query); },
    catalogTitle: function (entry) { return catalogTitle(entry); }
  };

  // One list of everything this driver could run, and one click to switch.
  // Switching back matters as much as switching: a version that has been
  // downloaded stays on disk, so undo is just activating it again.
  function renderVersionPicker(panel, driverID, body, opts) {
    var overridden = !!(opts && opts.overridden);
    var rows = versionRows(body);
    panel.textContent = "";

    if (rows.length === 0) {
      panel.textContent = "No versions found for this driver.";
      return;
    }

    if (overridden) {
      var note = document.createElement("div");
      note.style.color = "var(--text-dim)";
      note.style.fontSize = "0.75rem";
      note.style.marginBottom = "6px";
      note.textContent = "Your own file runs while it is there. Downloading a " +
        "version here keeps it ready without taking over.";
      panel.appendChild(note);
    }

    rows.forEach(function (row) {
      renderVersionRow(panel, driverID, row, rows, opts, overridden);
    });

    // The bundled copy is not an install, so /versions never lists it and no
    // amount of activating reaches it. Once a channel version is running it is
    // the only thing left to go back to, and it has to stay reachable after
    // this panel closes -- not just as an undo that lives for one switch.
    if (!overridden && opts && opts.runningSource !== "bundled") {
      renderBundledRow(panel, driverID, opts);
    }
  }

  function renderVersionRow(panel, driverID, row, rows, opts, overridden) {
    var line = document.createElement("div");
    line.style.display = "flex";
    line.style.alignItems = "center";
    line.style.gap = "8px";
    line.style.marginTop = "4px";

    var label = document.createElement("span");
    label.className = "creds-badge";
    label.textContent = "v" + row.version;
    line.appendChild(label);

    var detail = document.createElement("span");
    detail.className = "drv-version-detail";
    var facts = [];
    if (row.active && !overridden) facts.push("running now");
    else if (row.downloaded) facts.push("on disk");
    if (row.verification) facts.push(row.verification);
    detail.textContent = facts.join(" · ");
    line.appendChild(detail);

    var status = document.createElement("span");
    status.className = "drv-version-status";

    if (!row.active || overridden) {
      var action = document.createElement("button");
      action.type = "button";
      action.className = "btn-add";
      // An override means nothing here can take over, so the button must not
      // claim it will. Otherwise: on disk switches instantly, anything else
      // is fetched from the channel first.
      action.textContent = overridden
        ? (row.downloaded ? "Downloaded" : "Download")
        : "Use this";
      if (overridden && row.downloaded) action.disabled = true;
      action.addEventListener("click", function () {
        action.disabled = true;
        var endpoint = row.downloaded ? "/activate" : "/install";
        var payload = row.downloaded
          ? {version: row.version, sha256: row.sha256 || ""}
          : {version: row.version, repository_id: row.repositoryID || ""};
        status.textContent = row.downloaded ? "Switching…" : "Fetching…";
        apiFetch("/api/device_repository/drivers/" + encodeURIComponent(driverID) + endpoint, {
          method: "POST",
          headers: {"Content-Type": "application/json"},
          body: JSON.stringify(payload)
        }).then(function (r) {
          return r.json().then(function (b) {
            if (!r.ok) throw new Error(b.error || "could not switch version");
            return b;
          });
        }).then(function () {
          // It came down from the channel and is kept on disk now. Without
          // this the next attempt would fetch it again, which fails offline
          // for a file that is already there.
          row.downloaded = true;
          if (overridden) {
            status.textContent = "Downloaded. Your own file still runs.";
            action.textContent = "Downloaded";
            action.disabled = true;
            return;
          }
          status.textContent = "v" + row.version + " is running.";
          // The line above the panel still named the old version, and the
          // other rows still claimed to be running, so all three contradicted
          // each other until the tab was re-rendered.
          refreshSummary(opts, driverID);
          markRunning(panel, row.version);
          // Trying a driver and putting the old one back is the loop that
          // makes testing safe. It must not need a second trip through the
          // list, and it has to be repeatable -- undo re-arms this button so
          // the next attempt is one click, not a reopened panel.
          offerUndo(line, status, driverID, rows, opts, action, panel);
        }).catch(function (err) {
          status.textContent = err.message;
          action.disabled = false;
        });
      });
      line.appendChild(action);
    }

    line.appendChild(status);
    panel.appendChild(line);
  }

  // "Back to what shipped with this build" as a standing choice, not a
  // transient undo. POST /use_bundled refuses when no bundled copy exists
  // rather than stopping the driver it was meant to revert.
  function renderBundledRow(panel, driverID, opts) {
    var line = document.createElement("div");
    line.style.display = "flex";
    line.style.alignItems = "center";
    line.style.gap = "8px";
    line.style.marginTop = "4px";

    var label = document.createElement("span");
    label.className = "creds-badge";
    label.textContent = "bundled";
    line.appendChild(label);

    var detail = document.createElement("span");
    detail.className = "drv-version-detail";
    detail.textContent = "the copy shipped with this build";
    line.appendChild(detail);

    var status = document.createElement("span");
    status.className = "drv-version-status";

    var action = document.createElement("button");
    action.type = "button";
    action.className = "btn-add";
    action.textContent = "Use this";
    action.addEventListener("click", function () {
      action.disabled = true;
      status.textContent = "Switching…";
      useBundled(driverID, opts).then(function () {
        status.textContent = "The bundled driver is running.";
        refreshSummary(opts, driverID);
        markRunning(panel, null);
        action.textContent = "Running";
      }).catch(function (err) {
        status.textContent = err.message;
        action.disabled = false;
      });
    });

    line.appendChild(action);
    line.appendChild(status);
    panel.appendChild(line);
  }

  function useBundled(driverID, opts) {
    return apiFetch("/api/device_repository/drivers/" + encodeURIComponent(driverID) + "/use_bundled", {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify({logical_path: (opts && opts.logicalPath) || ""})
    }).then(function (r) {
      return r.json().then(function (b) {
        if (!r.ok) throw new Error(b.error || "could not switch to the bundled driver");
        return b;
      });
    });
  }

  // Keep the summary above the panel honest after a switch. Re-rendering the
  // whole tab would say the same thing but close the panel, which is the
  // opposite of what someone testing two versions against each other wants.
  //
  // It re-reads the catalog rather than editing the text in place: which
  // version runs, where it came from, how well tested it is and whether it may
  // control anything are all properties of the artifact now running, and
  // guessing any of them from the old line gets one of them wrong. The
  // telemetry-only badge in particular differs between versions of the same
  // driver.
  function refreshSummary(opts, driverID) {
    var badge = opts && opts.headlineEl;
    if (!badge || !opts.logicalPath) return Promise.resolve();
    return apiFetch("/api/drivers/catalog")
      .then(function (r) { return r.json(); })
      .then(function (data) {
        var entries = (data && data.entries) || [];
        var entry = null;
        entries.forEach(function (e) { if (e && e.path === opts.logicalPath) entry = e; });
        if (!entry) return;
        var summary = runningSummary(entry);
        badge.textContent = summary.headline;
        if (opts.detailEl) opts.detailEl.textContent = summary.detail;
        if (opts.readOnlyEl) opts.readOnlyEl.style.display = entry.read_only ? "" : "none";
        // The Update shortcut offers one specific version; once that version
        // is running it is an invitation to install what is already installed.
        if (opts.updateEl) {
          opts.updateEl.style.display = summary.updatable &&
            opts.updateEl.dataset.version === entry.upstream_version ? "" : "none";
        }
      })
      .catch(function () { /* the switch itself already reported its result */ });
  }

  // Only one row can be running. Leaving the old one marked would have the
  // panel contradict the line above it and itself. Nothing else in the row is
  // touched: whether a version is on disk did not change by switching away
  // from it.
  function markRunning(panel, version) {
    if (!panel) return;
    var wanted = version === null ? "bundled" : "v" + version;
    Array.prototype.forEach.call(panel.children, function (line) {
      var label = line.querySelector ? line.querySelector(".creds-badge") : null;
      var detail = line.querySelector ? line.querySelector(".drv-version-detail") : null;
      if (!label || !detail) return;
      var facts = detail.textContent.split(" · ").filter(function (fact) {
        return fact !== "running now";
      });
      if (label.textContent === wanted) facts.unshift("running now");
      detail.textContent = facts.join(" · ");
    });
  }

  // The Lua that is actually running, and where it came from. Built as DOM:
  // driver source is a file on disk that an operator may have written, and
  // setting it as innerHTML would execute whatever is in it.
  function renderDriverSource(panel, body) {
    panel.textContent = "";

    var header = document.createElement("div");
    header.style.display = "flex";
    header.style.alignItems = "center";
    header.style.flexWrap = "wrap";
    header.style.gap = "8px";
    header.style.marginBottom = "6px";

    var badge = document.createElement("span");
    badge.className = "creds-badge";
    badge.textContent = body.version ? "v" + body.version : body.filename || "driver";
    header.appendChild(badge);

    var detail = document.createElement("span");
    detail.className = "drv-version-detail";
    detail.textContent = [originLabel(body.source), body.filename, describeSize(body.bytes)]
      .filter(Boolean).join(" · ");
    header.appendChild(detail);

    // The published artifact carries generated metadata the repository copy
    // does not, so this is a link to the file's history, not to these bytes.
    if (body.repository_url) {
      var link = document.createElement("a");
      link.href = body.repository_url;
      link.target = "_blank";
      link.rel = "noopener";
      link.textContent = "Open in device-drivers";
      link.style.color = "var(--accent-e)";
      link.style.fontSize = "0.75rem";
      header.appendChild(link);
    }
    panel.appendChild(header);

    var pre = document.createElement("pre");
    pre.className = "drv-source-code";
    pre.textContent = body.lua || "";
    panel.appendChild(pre);

    var footer = document.createElement("div");
    footer.style.alignItems = "center";
    footer.style.display = "flex";
    footer.style.flexWrap = "wrap";
    footer.style.gap = "8px";
    footer.style.marginTop = "6px";

    var digest = document.createElement("span");
    digest.className = "drv-version-detail";
    digest.textContent = "sha256 " + String(body.sha256 || "").slice(0, 16) + "…";
    footer.appendChild(digest);

    var edit = document.createElement("button");
    edit.type = "button";
    edit.className = "btn-add";
    edit.textContent = "Edit and try";
    edit.addEventListener("click", function () {
      renderDriverEditor(panel, body);
    });
    footer.appendChild(edit);

    // Everything points at the repository, including from the read view: a
    // driver that is wrong for your hardware is worth reporting even if you
    // have not written the fix yourself.
    appendSuggestButton(footer, body, null);

    var status = document.createElement("span");
    status.className = "drv-draft-status";
    footer.appendChild(status);

    panel.appendChild(footer);

    // A draft may already be running from an earlier visit, or from another
    // tab. The countdown belongs to the gateway, not to this page.
    resumeDraft(panel, body, status);
  }

  // The editor is its own surface now: a driver is tens of kilobytes of Lua,
  // and editing that in a textarea wedged into a device row is squinting, not
  // editing. This hands it the driver and the actions, and knows nothing about
  // how the editor is drawn.
  function renderDriverEditor(panel, body) {
    if (!S.openDriverEditor) {
      panel.textContent = "The editor did not load.";
      return;
    }
    var id = encodeURIComponent(body.id);

    function post(path, payload) {
      return apiFetch("/api/drivers/" + id + path, {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: payload === undefined ? null : JSON.stringify(payload)
      }).then(function (r) {
        return r.json().then(function (b) {
          if (!r.ok) throw new Error(b.error || "the request was refused");
          return b;
        });
      });
    }

    S.openDriverEditor({
      id: body.id,
      filename: body.filename,
      version: body.version,
      bytes: body.bytes,
      lua: body.lua,
      sha256: body.sha256,
      source: body.source,
      sourceLabel: originLabel(body.source),
      repository_url: body.repository_url
    }, {
      runDraft: function (lua, minutes) { return post("/draft", {lua: lua, minutes: minutes}); },
      keepDraft: function () { return post("/draft/keep"); },
      revertDraft: function () { return post("/draft/revert"); },
      draftStatus: function () {
        return apiFetch("/api/drivers/" + id + "/draft").then(function (r) { return r.json(); });
      },
      lint: function (lua) { return post("/lint", {lua: lua}); },
      suggest: function (lua, say) {
        var url = suggestUpstreamURL(body, lua);
        if (url.length > MAX_SUGGESTION_URL) {
          url = suggestUpstreamURL(body, "");
          say("The driver is too large to prefill; paste it into the issue.");
        }
        window.open(url, "_blank", "noopener");
      }
    });
  }

  function resumeDraft(panel, body, status) {
    apiFetch("/api/drivers/" + encodeURIComponent(body.id) + "/draft")
      .then(function (r) { return r.json(); })
      .then(function (b) {
        if (b && b.running) showDraftRunning(panel, body, status, b.expires_at_ms);
      })
      .catch(function () { /* nothing is running, which is the normal case */ });
  }

  // While a draft runs, the only two things worth offering are keeping it and
  // putting it back. Both are one click, and the clock says how long the
  // decision stays open.
  function showDraftRunning(panel, body, status, expiresAtMS) {
    var host = status.parentElement || panel;
    host.querySelectorAll(".drv-draft-action").forEach(function (el) { el.remove(); });

    function tick() {
      var left = Math.max(0, Math.round((expiresAtMS - Date.now()) / 1000));
      if (left <= 0) {
        status.textContent = "The draft expired and the previous driver is back.";
        clearInterval(timer);
        return;
      }
      var minutes = Math.floor(left / 60);
      var seconds = left % 60;
      status.textContent = "Draft running · reverts in " + minutes + ":" +
        (seconds < 10 ? "0" : "") + seconds;
    }
    var timer = setInterval(tick, 1000);
    tick();

    function act(path, label, done) {
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "btn-add drv-draft-action";
      btn.textContent = label;
      btn.addEventListener("click", function () {
        btn.disabled = true;
        apiFetch("/api/drivers/" + encodeURIComponent(body.id) + "/draft/" + path, {method: "POST"})
          .then(function (r) {
            return r.json().then(function (b) {
              if (!r.ok) throw new Error(b.error || "could not " + path + " the draft");
              return b;
            });
          }).then(function () {
            clearInterval(timer);
            status.textContent = done;
            host.querySelectorAll(".drv-draft-action").forEach(function (el) { el.remove(); });
          }).catch(function (err) {
            status.textContent = err.message;
            btn.disabled = false;
          });
      });
      host.insertBefore(btn, status);
    }

    act("keep", "Keep it", "Kept. This is your own file now.");
    act("revert", "Put it back", "Reverted.");
  }

  // A line diff with a little context around each change, which is what fits
  // in a URL and what a maintainer reads anyway.
  //
  // Longest-common-subsequence over lines. A driver is a few thousand lines,
  // so the quadratic table is fine here and worth the exactness: a cheaper
  // heuristic would drift out of alignment after the first edit and report
  // changes that were never made.
  function unifiedDiff(before, after, context) {
    var a = String(before).split("\n");
    var b = String(after).split("\n");
    var contextLines = context === undefined ? 3 : context;

    var lcs = [];
    for (var i = 0; i <= a.length; i++) lcs.push(new Array(b.length + 1).fill(0));
    for (var i = a.length - 1; i >= 0; i--) {
      for (var j = b.length - 1; j >= 0; j--) {
        lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1
          : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
      }
    }

    var ops = [];
    var x = 0, y = 0;
    while (x < a.length && y < b.length) {
      if (a[x] === b[y]) { ops.push([" ", a[x]]); x++; y++; }
      else if (lcs[x + 1][y] >= lcs[x][y + 1]) { ops.push(["-", a[x]]); x++; }
      else { ops.push(["+", b[y]]); y++; }
    }
    while (x < a.length) { ops.push(["-", a[x]]); x++; }
    while (y < b.length) { ops.push(["+", b[y]]); y++; }

    // Only the changed regions, so an unchanged driver body does not fill the
    // issue with lines nobody needs to read.
    var keep = new Array(ops.length).fill(false);
    ops.forEach(function (op, index) {
      if (op[0] === " ") return;
      for (var k = Math.max(0, index - contextLines);
           k <= Math.min(ops.length - 1, index + contextLines); k++) {
        keep[k] = true;
      }
    });

    var out = [];
    var skipping = false;
    ops.forEach(function (op, index) {
      if (!keep[index]) {
        if (!skipping) { out.push("@@"); skipping = true; }
        return;
      }
      skipping = false;
      out.push(op[0] + op[1]);
    });
    return out.join("\n");
  }

  // Suggest the edit back to the repository the driver came from.
  //
  // The gateway holds no GitHub token and needs none: GitHub accepts a
  // pre-filled issue over a URL, and the operator is already signed in to
  // their own browser. One click opens it with the driver, the version, the
  // hardware and the edit already written out.
  function suggestUpstreamURL(body, edited) {
    var title = "[" + (body.id || "driver") + "] ";
    var lines = [
      "What I changed and why:",
      "",
      "",
      "---",
      "Driver: " + (body.id || "") + " " + (body.version ? "v" + body.version : ""),
      "Came from: " + originLabel(body.source),
      "File: " + (body.filename || ""),
      "Original sha256: " + (body.sha256 || "")
    ];
    if (edited && edited !== body.lua) {
      // A diff, not the file. Drivers run to tens of kilobytes and GitHub
      // rejects a URL past about 8k, so sending the whole file meant the code
      // never travelled at all -- while a fix is usually a handful of lines.
      lines.push("", "```diff", unifiedDiff(body.lua || "", edited), "```");
    }
    return "https://github.com/srcfl/device-drivers/issues/new" +
      "?title=" + encodeURIComponent(title) +
      "&body=" + encodeURIComponent(lines.join("\n"));
  }

  // GitHub rejects a URL past roughly 8k, and an edited driver is usually
  // larger than that. Past the limit the issue is opened without the file and
  // says so, rather than opening a page that errors.
  var MAX_SUGGESTION_URL = 8000;

  function appendSuggestButton(host, body, getEdited) {
    var suggest = document.createElement("button");
    suggest.type = "button";
    suggest.className = "btn-add";
    suggest.textContent = "Suggest to repo";
    suggest.addEventListener("click", function () {
      var edited = getEdited ? getEdited() : "";
      var url = suggestUpstreamURL(body, edited);
      var note = host.querySelector(".drv-draft-status");
      if (url.length > MAX_SUGGESTION_URL) {
        url = suggestUpstreamURL(body, "");
        if (note) {
          note.textContent = "The driver is too large to prefill; paste it into the issue.";
        }
      }
      window.open(url, "_blank", "noopener");
    });
    host.appendChild(suggest);
  }

  function describeSize(bytes) {
    if (typeof bytes !== "number" || bytes <= 0) return "";
    if (bytes < 1024) return bytes + " bytes";
    return (bytes / 1024).toFixed(1) + " kB";
  }

  // Put back whatever was running before the switch. Which call does that
  // depends on what it was, not on its version number: a managed artifact of
  // the same version can sit on disk from an earlier trial, and activating
  // that is not the same file as the bundled one -- they can differ in whether
  // the driver is allowed to control anything.
  function offerUndo(line, status, driverID, rows, opts, action, panel) {
    var previousVersion = opts && opts.runningVersion;
    if (!previousVersion) return;
    var wasBundled = opts.runningSource === "bundled";
    var onDisk = null;
    if (!wasBundled) {
      rows.forEach(function (row) {
        if (row.version === previousVersion && row.downloaded) onDisk = row;
      });
    }

    var undo = document.createElement("button");
    undo.type = "button";
    undo.className = "btn-add";
    undo.textContent = wasBundled
      ? "Undo (back to the bundled driver)"
      : "Undo (back to v" + previousVersion + ")";
    undo.addEventListener("click", function () {
      undo.disabled = true;
      status.textContent = "Switching back…";
      var call = wasBundled
        ? useBundled(driverID, opts)
        : switchBack(driverID, previousVersion, onDisk, opts);
      call.then(function () {
        status.textContent = wasBundled
          ? "The bundled driver is running again."
          : "v" + previousVersion + " is running again.";
        refreshSummary(opts, driverID);
        markRunning(panel, wasBundled ? null : previousVersion);
        undo.remove();
        if (action) action.disabled = false;
      }).catch(function (err) {
        status.textContent = err.message;
        undo.disabled = false;
      });
    });
    line.appendChild(undo);
  }

  // A managed version that is still on disk is activated; one that has been
  // pruned has to be rolled back to, which steps to whatever preceded it.
  function switchBack(driverID, version, onDisk, opts) {
    var request = onDisk
      ? {path: "/activate", body: {version: version, sha256: onDisk.sha256 || ""}}
      : {path: "/rollback", body: {logical_path: (opts && opts.logicalPath) || ""}};
    return apiFetch("/api/device_repository/drivers/" + encodeURIComponent(driverID) + request.path, {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify(request.body)
    }).then(function (r) {
      return r.json().then(function (b) {
        if (!r.ok) throw new Error(b.error || "could not switch back");
        return b;
      });
    });
  }

  function configProfilesFor(entry) {
    if (!entry || entry.id !== "goodwe" || !versionAtLeast(entry.version, "1.0.2")) return [];
    return DRIVER_CONFIG_PROFILES.goodwe;
  }

  function profileByValue(profiles, value) {
    for (var i = 0; i < profiles.length; i++) {
      if (profiles[i].value === value) return profiles[i];
    }
    return profiles[0] || null;
  }

  S.tabs.devices = {
    render: function (ctx) {
      var help = ctx.help, escHtml = ctx.escHtml, config = ctx.config;
      if (!config.drivers) config.drivers = [];
      var html = '<fieldset><legend>Add from catalog</legend>' +
        '<div class="field-row"><div>' +
        '<label>Channel</label>' +
        '<select id="driver-catalog-channel"><option value="stable">Stable</option><option value="beta">Beta · test one driver</option></select>' +
        '</div><div>' +
        // You know your hardware, not which driver covers it. The catalog
        // carries tested_models for most drivers, so searching those is what
        // turns "I have an SH10RT" into the right answer.
        '<label>Find your device ' + help('Search by manufacturer, model or driver name. Drivers that have run on real hardware are listed first.') + '</label>' +
        '<input type="search" id="driver-catalog-search" placeholder="e.g. SH10RT, Ferroamp, Chargestorm" autocomplete="off">' +
        '</div></div>' +
        // The picker stays as the selection the rest of the form reads, but
        // the cards are what an operator actually looks at.
        '<select id="driver-catalog-picker" hidden><option value="">Loading catalog…</option></select>' +
        '<div id="driver-catalog-results" class="drv-catalog-results">Loading catalog…</div>' +
        '<div class="field-row"><div>' +
        '<label>Friendly name</label><input type="text" id="driver-catalog-name" placeholder="e.g. ferroamp-house">' +
        '</div><div id="driver-catalog-profile-wrap" hidden>' +
        '<label>Register profile ' + help('Select the register map used by this inverter. The Unit ID remains editable after the driver is added.') + '</label>' +
        '<select id="driver-catalog-profile"></select>' +
        '</div></div>' +
        '<button class="btn-add" id="driver-catalog-add">+ Add selected</button>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:8px 0 0">Beta installs only the selected signed driver. Core and other drivers stay unchanged.</p>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:8px 0 0">' +
        '<a href="https://github.com/srcfl/device-drivers/blob/main/SUPPORT_STATUS.md" target="_blank" rel="noopener" style="color:var(--accent-e)">Driver support and hardware test status</a>' +
        '</p>' +
        '</fieldset>';
      html += '<div class="devices-list">';
      config.drivers.forEach(function (d, idx) {
        var cap = d.capabilities || {};
        var mqtt = cap.mqtt || d.mqtt;
        var modbus = cap.modbus || d.modbus;
        var protocol = mqtt ? "mqtt" : (modbus ? "modbus" : (cap.http ? "http" : "?"));
        var driverFile = d.lua || "(none)";
        var entryForDriver = catalogEntryForLua(d.lua);
        var supportsBattery = catalogHasCapability(d.lua, "battery") &&
          !(entryForDriver && entryForDriver.read_only);
        html += '<div class="device-item">' +
          '<div class="device-item-header">' +
          '<strong>' + escHtml(d.name) + '</strong>' +
          '<span class="device-meta">lua · ' + protocol + ' · ' + escHtml(driverFile) + '</span>' +
          '<span class="driver-module-status" data-drv-lua="' + escHtml(d.lua || '') + '"></span>' +
          '<button class="btn-remove" data-remove-idx="' + idx + '">Remove</button>' +
          '</div>' +
          '<div class="field-row device-core-row' + (supportsBattery ? '' : ' field-row-single') + '"><div>' +
          '<label>Driver file ' + help('Path to the .lua driver. Absolute or relative to the config file directory.') + '</label>' +
          '<input type="text" data-path="drivers.' + idx + '.lua" value="' + escHtml(driverFile) + '">' +
          '</div><div class="driver-battery-capacity" data-drv-lua="' + escHtml(d.lua || '') + '"' + (supportsBattery ? '' : ' hidden style="display:none"') + '>' +
          '<label>Battery capacity (kWh) ' + help('Nameplate storage capacity in kilowatt-hours. Stored internally as Wh.') + '</label>' +
          '<input type="number" step="0.1" data-path="drivers.' + idx + '.battery_capacity_wh" data-unit-scale="1000" value="' + ((d.battery_capacity_wh || 0) / 1000) + '">' +
          '<label class="driver-observe-only" data-drv-lua="' + escHtml(d.lua || '') + '"' + (supportsBattery ? '' : ' hidden style="display:none"') + '>' +
          '<input type="checkbox" data-checkbox-path="drivers.' + idx + '.observe_only"' + (d.observe_only ? ' checked' : '') + '> Observe only ' +
          help('Poll battery telemetry and show it in the dashboard, but never send charge/discharge commands. Use when another party (e.g. a retailer VPP) controls the battery.') +
          '</label>' +
          '</div></div>' +
          '<label><input type="checkbox" data-checkbox-path="drivers.' + idx + '.is_site_meter"' + (d.is_site_meter ? ' checked' : '') + '> Site meter ' + help('Exactly one driver should be the site meter — its grid reading defines the point-of-measurement the PI loop balances.') + '</label>';
        if (mqtt) {
          html += '<fieldset><legend>MQTT</legend>' +
            '<div class="field-row"><div>' +
            '<label>Host ' + help('IP or hostname of the MQTT broker exposing the device data (e.g. the Ferroamp EnergyHub).') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.capabilities.mqtt.host" value="' + escHtml(mqtt.host) + '">' +
            '</div><div>' +
            '<label>Port</label><input type="number" data-path="drivers.' + idx + '.capabilities.mqtt.port" value="' + (mqtt.port || 1883) + '">' +
            '</div></div>' +
            '<div class="field-row"><div>' +
            '<label>Username</label><input type="text" data-path="drivers.' + idx + '.capabilities.mqtt.username" value="' + escHtml(mqtt.username || "") + '">' +
            '</div><div>' +
            '<label>Password</label><input type="password" data-path="drivers.' + idx + '.capabilities.mqtt.password" value="' + escHtml(mqtt.password || "") + '">' +
            '</div></div></fieldset>';
        }
        if (modbus) {
          html += '<fieldset><legend>Modbus TCP</legend>' +
            '<div class="field-row"><div>' +
            '<label>Host ' + help('IP of the Modbus-TCP device (e.g. Sungrow inverter LAN port).') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.capabilities.modbus.host" value="' + escHtml(modbus.host) + '">' +
            '</div><div>' +
            '<label>Port</label><input type="number" data-path="drivers.' + idx + '.capabilities.modbus.port" value="' + (modbus.port || 502) + '">' +
            '</div></div>' +
            '<label>Unit ID ' + help('Slave address. Usually 1 for a single-device setup.') + '</label>' +
            '<input type="number" data-path="drivers.' + idx + '.capabilities.modbus.unit_id" value="' + (modbus.unit_id || 1) + '">' +
            '</fieldset>';
          html += '<div class="drv-profile-slot" data-driver-idx="' + idx + '"></div>';
        }
        // Local-HTTP vs cloud-HTTP vs vehicle-over-proxy detection by
        // declared config shape + catalog capabilities. Vehicle drivers
        // (e.g. tesla_vehicle against a TeslaBLEProxy) expose only
        // {ip, vin} and read no power channel.
        var dcfg = d.config || {};
        var hasHostField = Object.prototype.hasOwnProperty.call(dcfg, 'host');
        var hasAuthField = Object.prototype.hasOwnProperty.call(dcfg, 'email') ||
                           Object.prototype.hasOwnProperty.call(dcfg, 'password');
        var hasApiCredsField = Object.prototype.hasOwnProperty.call(dcfg, 'client_id') ||
                               Object.prototype.hasOwnProperty.call(dcfg, 'client_secret');
        var catalogEntry = (S.catalogByLua || {})[d.lua];
        var caps = (catalogEntry && catalogEntry.capabilities) || [];
        var isVehicleDriver = cap.http != null &&
          (caps.indexOf("vehicle") >= 0 ||
           Object.prototype.hasOwnProperty.call(dcfg, 'vin') ||
           Object.prototype.hasOwnProperty.call(dcfg, 'ip'));
        var isLocalHTTP = !isVehicleDriver && cap.http != null && hasHostField;
        // OAuth2 client_credentials drivers (e.g. MyUplink): identify via client_id/client_secret keys.
        var isApiCredsDriver = !isVehicleDriver && cap.http != null && !hasHostField && hasApiCredsField;
        var isCloudDriver = !isVehicleDriver && !isApiCredsDriver && cap.http != null && !hasHostField &&
          (hasAuthField || Object.keys(dcfg).length === 0);
        if (isVehicleDriver) {
          // TeslaBLEProxy-style drivers only need the LAN IP of the
          // proxy and the VIN it's paired to. "Verify connection"
          // makes the backend issue a one-shot vehicle_data poll so
          // the operator can confirm pairing before saving.
          var vcfg = d.config || {};
          html += '<fieldset><legend>Vehicle</legend>' +
            '<div class="field-row"><div>' +
            '<label>Proxy IP ' + help('LAN address of the TeslaBLEProxy. Bare IP uses port 8080; append ":port" to override (e.g. 192.168.1.50:1234).') + '</label>' +
            '<input type="text" class="tesla-ip-input" data-driver-idx="' + idx + '" data-path="drivers.' + idx + '.config.ip" value="' + escHtml(vcfg.ip || '') + '" placeholder="192.168.1.50 (or 192.168.1.50:1234)">' +
            '</div><div>' +
            '<label>VIN ' + help('Vehicle Identification Number the proxy is paired to.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.vin" value="' + escHtml(vcfg.vin || '') + '" placeholder="5YJ3E1EA1KF000000">' +
            '</div></div>' +
            '<div style="margin-top:8px;display:flex;gap:10px;align-items:center">' +
            '<button class="btn-add tesla-verify-btn" type="button" data-driver-idx="' + idx + '">Verify connection</button>' +
            '<span class="tesla-verify-status" data-driver-idx="' + idx + '" style="font-size:0.82rem;color:var(--text-dim)"></span>' +
            '</div>' +
            '</fieldset>';
        }
        if (isLocalHTTP) {
          var lcfg = d.config || {};
          // NIBE-style local-API drivers (catalog apicreds + a connection port)
          // also need a username + an optional self-signed cert pin; plain
          // local-HTTP gateways (e.g. a P1 meter) keep just the Host field.
          // Backend auto-derives capabilities.http.allowed_hosts from config.host.
          var localCreds = caps.indexOf('apicreds') >= 0;
          var pin = (cap.http && cap.http.tls_pin_sha256) || '';
          // Render overlap controls for local HTTP gateways. The post-fetch
          // pass reveals each one only when the signed catalog says that the
          // gateway reports both the site meter and that DER kind.
          html += '<fieldset><legend>HTTP</legend>' +
            '<label>Host / IP ' + help('Hostname (e.g. zap.local) or IP address of the device. mDNS names work when your OS resolver supports them; otherwise use the LAN IP.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.host" value="' + escHtml(lcfg.host || '') + '" placeholder="zap.local">' +
            '<div class="drv-local-creds" data-drv-lua="' + escHtml(d.lua || '') + '"' + (localCreds ? '' : ' hidden') + '>' +
              '<label style="margin-top:8px">Username ' + help('Username for the device\'s local API (HTTP Basic auth). For NIBE this is the local-API account you set up in the myUplink app.') + '</label>' +
              '<input type="text" autocomplete="off" data-path="drivers.' + idx + '.config.username" value="' + escHtml(lcfg.username || '') + '" placeholder="local-api-user">' +
            '</div>' +
            '<label class="drv-disable-pv" data-drv-lua="' + escHtml(d.lua || '') + '" style="margin-top:8px;display:none;align-items:center;gap:6px;font-weight:normal">' +
              '<input type="checkbox" data-checkbox-path="drivers.' + idx + '.config.disable_pv"' +
              (lcfg.disable_pv ? ' checked' : '') + '>' +
              'Disable PV readings ' +
              help('Use this gateway for the P1 meter only. When another driver already owns PV aggregation, set this so the two drivers don\'t double-count generation.') +
            '</label>' +
            '<label class="drv-disable-battery" data-drv-lua="' + escHtml(d.lua || '') + '" style="margin-top:8px;display:none;align-items:center;gap:6px;font-weight:normal">' +
              '<input type="checkbox" data-checkbox-path="drivers.' + idx + '.config.disable_battery"' +
              (lcfg.disable_battery ? ' checked' : '') + '>' +
              'Disable battery readings ' +
              help('Turn this on when another driver reports the same physical battery. It removes the duplicate battery and prevents Combined from counting its power twice.') +
            '</label>' +
            '<div class="drv-local-creds" data-drv-lua="' + escHtml(d.lua || '') + '"' + (localCreds ? '' : ' hidden') + '>' +
              '<label style="margin-top:8px">Certificate fingerprint (SHA-256) ' + help('Pin the device\'s self-signed HTTPS certificate by its SHA-256 fingerprint (the "fingeravtryck" in the myUplink app, or from "openssl x509 -fingerprint -sha256"). 64 hex chars; colons and case are ignored. Leave empty for normal certificate verification.') + '</label>' +
              '<input type="text" autocomplete="off" data-path="drivers.' + idx + '.capabilities.http.tls_pin_sha256" value="' + escHtml(pin) + '" placeholder="73d1ac81…bd9bf4eb (64 hex)" style="font-family:var(--mono);font-size:0.78rem">' +
            '</div>' +
            '</fieldset>';
        }
        if (isApiCredsDriver) {
          // OAuth2 authorization-code drivers (e.g. MyUplink). Numbered
          // setup steps + the exact Callback URL, then Client Identifier +
          // Client Secret rendered together with the SAME labels as the
          // MyUplink portal (so the two values don't get swapped). The
          // secret is masked (never echoed into the DOM); Connect completes
          // the one-time browser consent and the refresh_token is stored
          // server-side, flipping the badge to Connected.
          var acfg = d.config || {};
          // refresh_token is a config_secret: when saved it round-trips as a
          // masked placeholder (non-empty string), so a non-empty value here
          // means "consent completed".
          var connected = typeof acfg.refresh_token === 'string' && acfg.refresh_token !== '';
          var connBadge = connected
            ? '<span class="creds-badge creds-saved">✓ Connected</span>'
            : '<span class="creds-badge creds-missing">⚠ Not connected</span>';
          var secretSaved = typeof acfg.client_secret === 'string' && acfg.client_secret !== '';
          var secretBadge = secretSaved
            ? '<span class="creds-badge creds-saved">✓ Saved</span>'
            : '<span class="creds-badge creds-missing">⚠ Not saved</span>';
          var callbackURL = location.origin + '/api/oauth/myuplink/callback';
          // Field labels mirror the MyUplink portal exactly ("Client
          // Identifier" / "Client Secret") and sit together — same as the
          // portal's Credentials box — so the two values don't get swapped.
          html += '<fieldset><legend>MyUplink connection</legend>' +
            '<ol style="color:var(--fg-muted);font-size:0.78rem;line-height:1.6;margin:0 0 12px;padding-left:1.2em">' +
            '<li>Open the <a href="https://dev.myuplink.com/apps" target="_blank" rel="noopener" style="color:var(--accent-e)">MyUplink developer portal</a> → <b>Apps</b> → <b>Create new app</b>.</li>' +
            '<li>Set <b>Callback Url</b> to the address shown below (copy it exactly).</li>' +
            '<li>Copy the app\'s <b>Client Identifier</b> and <b>Client Secret</b> into the matching fields below.</li>' +
            '<li><b>Save</b> these settings, then click <b>Connect to MyUplink</b> and sign in.</li>' +
            '</ol>' +
            '<label>Callback URL ' + help('Paste this exact string into the "Callback Url" field of your MyUplink app. It must match the address you use to reach FTW.') + '</label>' +
            '<input type="text" class="myuplink-callback-url" value="' + escHtml(callbackURL) + '" readonly onclick="this.select()" style="font-family:var(--mono);font-size:0.8rem">' +
            '<label style="margin-top:8px">Client Identifier ' + help('The "Client Identifier" from your MyUplink app (a UUID like xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx). NOT the secret.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.client_id" value="' + escHtml(acfg.client_id || '') + '" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx">' +
            '<label style="margin-top:8px">Client Secret ' + secretBadge + ' ' + help('The "Client Secret" from your MyUplink app. Stored masked; leave empty to keep the saved value.') + '</label>' +
            '<input type="password" autocomplete="off" data-path="drivers.' + idx + '.config.client_secret" value="" placeholder="' + (secretSaved ? '•••••••• (leave empty to keep)' : 'paste Client Secret') + '">' +
            '<label style="margin-top:8px">OAuth scope ' + help('Advanced. Default is WRITESYSTEM READSYSTEM offline_access because MyUplink can reject read-only authorization. The driver remains read-only. Use READSYSTEM offline_access only if your MyUplink app accepts it.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.oauth_scope" value="' + escHtml(acfg.oauth_scope || '') + '" placeholder="WRITESYSTEM READSYSTEM offline_access">' +
            '<div style="margin-top:12px;display:flex;gap:10px;align-items:center;flex-wrap:wrap">' +
            '<button class="btn-add myuplink-connect-btn" type="button" data-driver-idx="' + idx + '" data-driver-name="' + escHtml(d.name || '') + '">Connect to MyUplink</button>' +
            connBadge +
            '<span class="myuplink-connect-status" data-driver-idx="' + idx + '" style="font-size:0.82rem;color:var(--text-dim)"></span>' +
            '</div>' +
            // Manual fallback: when the automatic redirect can't reach this
            // device (or the portal rejected an http LAN
            // callback), the operator copies the redirected URL from the
            // address bar and pastes it here. The Pi exchanges the code over
            // its own outbound HTTPS, so no inbound callback is needed.
            '<details style="margin-top:10px">' +
            '<summary style="cursor:pointer;font-size:0.8rem;color:var(--text-dim)">Not redirected back? Paste the URL instead</summary>' +
            '<p style="color:var(--text-dim);font-size:0.72rem;margin:6px 0">After signing in at MyUplink, copy the full address from your browser\'s address bar and paste it here if the page did not return to FTW.</p>' +
            '<input type="text" class="myuplink-manual-url" data-driver-idx="' + idx + '" placeholder=".../api/oauth/myuplink/callback?code=...&amp;state=..." style="font-family:var(--mono);font-size:0.78rem">' +
            '<button class="btn-add myuplink-manual-btn" type="button" data-driver-idx="' + idx + '" style="margin-top:6px">Complete connection</button>' +
            '</details>' +
            '</fieldset>';
        }
        // Slot for catalog-declared config_secrets (e.g. sonnen Auth-Token).
        // Filled by the after() pass once /api/drivers/catalog has resolved.
        html += '<div class="drv-secrets-slot" data-driver-idx="' + idx + '"></div>';
        if (isCloudDriver) {
          var cfg = d.config || {};
          var hasPw = d.has_password === true;
          var pwBadge = hasPw
            ? '<span class="creds-badge creds-saved">✓ Saved</span>'
            : '<span class="creds-badge creds-missing">⚠ Not saved</span>';
          html += '<fieldset><legend>Cloud credentials</legend>' +
            '<div class="field-row"><div>' +
            '<label>Email ' + help('Account email for the cloud service.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.email" value="' + escHtml(cfg.email || '') + '">' +
            '</div><div>' +
            '<label>Password ' + pwBadge + '</label>' +
            '<input type="password" data-path="drivers.' + idx + '.config.password" value="" ' +
              'placeholder="' + (hasPw ? '•••••••• (leave empty to keep)' : 'enter password') + '">' +
            '</div></div>' +
            '<div class="field-row" style="align-items:flex-end"><div style="flex:1">' +
            '<label>Charger ' + help('Click Connect to load chargers from your account.') + '</label>' +
            '<select id="ev-charger-select-' + idx + '" data-path="drivers.' + idx + '.config.serial">' +
            (cfg.serial
              ? '<option value="' + escHtml(cfg.serial) + '" selected>' + escHtml(cfg.serial) + '</option>'
              : '<option value="">(not connected)</option>') +
            '</select>' +
            '</div><div>' +
            '<button class="btn-add ev-connect-btn" type="button" data-driver-idx="' + idx + '">Connect</button>' +
            '</div></div>' +
            '<span id="ev-connect-status-' + idx + '" style="font-size:0.8rem;color:var(--text-dim)"></span>' +
            '</fieldset>';
        }
        html += '<div class="driver-test-panel">' +
          '<button class="btn-add driver-test-btn" type="button" data-driver-idx="' + idx + '">Test connection</button>' +
          '<span class="driver-test-status" data-driver-idx="' + idx + '"></span>' +
          '<div class="driver-test-output" data-driver-idx="' + idx + '" hidden></div>' +
          '</div>';
        html += '</div>';
      });
      html += '</div>' +
        '<a href="/setup?step=3" class="btn-add" style="display:block;text-align:center;text-decoration:none">Add new device&hellip;</a>' +
        '<button class="btn-add" id="add-mqtt">+ Add MQTT device</button>' +
        '<button class="btn-add" id="add-modbus">+ Add Modbus device</button>';
      return html;
    },
    after: function (ctx) {
      var config = ctx.config;
      var bodyEl = ctx.bodyEl;
      var escHtml = ctx.escHtml;

      function fmtW(v) {
        if (!Number.isFinite(v)) return "—";
        return Math.abs(v) >= 1000 ? (v / 1000).toFixed(2) + " kW" : v.toFixed(0) + " W";
      }

      function fmtNum(v) {
        if (!Number.isFinite(v)) return "—";
        return Math.abs(v) >= 100 ? v.toFixed(0) : v.toFixed(2);
      }

      function fmtAge(ms) {
        if (!Number.isFinite(ms) || ms < 0) return "—";
        var s = Math.floor(ms / 1000);
        return s < 60 ? s + "s ago" : Math.floor(s / 60) + "m ago";
      }

      function renderProbeOutput(res) {
        var readings = res.readings || res.Readings || [];
        var metrics = res.metrics || res.Metrics || [];
        var health = res.health || res.Health || {};
        var identity = res.identity || res.Identity || {};
        var html = '<div class="driver-test-kv">';
        html += '<span>status</span><strong>' + escHtml(res.ok ? "connected" : "failed") + '</strong>';
        html += '<span>elapsed</span><strong>' + escHtml(String(res.elapsed_ms || res.ElapsedMs || 0)) + ' ms</strong>';
        if (health.TickCount != null) {
          html += '<span>ticks</span><strong>' + escHtml(String(health.TickCount)) + '</strong>';
        }
        if (identity.make || identity.sn || identity.endpoint) {
          html += '<span>identity</span><strong>' + escHtml([identity.make, identity.sn, identity.endpoint].filter(Boolean).join(" · ")) + '</strong>';
        }
        html += '</div>';
        if (res.error) {
          html += '<div class="driver-test-error">' + escHtml(res.error) + '</div>';
        }
        if (readings.length) {
          html += '<div class="driver-test-values">';
          readings.forEach(function (r) {
            var soc = r.soc != null ? " · SoC " + (r.soc * 100).toFixed(1) + "%" : "";
            var age = r.updated_at_ms ? " · " + fmtAge(Date.now() - r.updated_at_ms) : "";
            html += '<div><span>' + escHtml(r.type) + '</span><strong>' + escHtml(fmtW(r.smoothed_w)) + '</strong><small>raw ' + escHtml(fmtW(r.raw_w)) + soc + age + '</small></div>';
          });
          html += '</div>';
        }
        if (metrics.length) {
          html += '<div class="driver-test-metrics">';
          metrics.slice(0, 12).forEach(function (m) {
            html += '<span>' + escHtml(m.name) + '</span><strong>' + escHtml(fmtNum(m.value)) + '</strong>';
          });
          if (metrics.length > 12) {
            html += '<span>more</span><strong>' + escHtml(String(metrics.length - 12)) + '</strong>';
          }
          html += '</div>';
        }
        if (!readings.length && !metrics.length && !res.error) {
          html += '<div class="driver-test-empty">No values returned.</div>';
        }
        return html;
      }

      // Driver catalog picker — fetch async, render into select.
      function syncCatalogProfilePicker() {
        var picker = document.getElementById("driver-catalog-picker");
        var wrap = document.getElementById("driver-catalog-profile-wrap");
        var profileSelect = document.getElementById("driver-catalog-profile");
        if (!picker || !wrap || !profileSelect) return;
        var chosen = picker.options[picker.selectedIndex];
        var entry = chosen ? { id: chosen.dataset.id, version: chosen.dataset.version } : null;
        var profiles = configProfilesFor(entry);
        wrap.hidden = profiles.length === 0;
        profileSelect.innerHTML = "";
        profiles.forEach(function (profile) {
          var opt = document.createElement("option");
          opt.value = profile.value;
          opt.textContent = profile.label + " · Unit ID " + profile.unitId;
          profileSelect.appendChild(opt);
        });
      }

      function populateCatalogPicker(entries, channel) {
        var sel = document.getElementById("driver-catalog-picker");
        if (!sel) return;
        sel.innerHTML = "";
        if (entries.length === 0) {
          sel.innerHTML = "<option value=''>(no drivers found)</option>";
          return;
        }
        // Nothing is chosen until the operator chooses. A select takes its
        // first option on its own, which with cards means one sits silently
        // outlined and Add would fetch a driver nobody asked for.
        sel.appendChild(document.createElement("option"));
        entries.forEach(function (e) {
          var opt = document.createElement("option");
          opt.value = e.path;
          var protoLabel = (e.protocols || []).join("+");
          opt.textContent = (e.name || e.filename) + "  —  " + (e.manufacturer || "?") + "  [" + protoLabel + "]" + (e.version ? "  v" + e.version : "");
          opt.dataset.protocols = protoLabel;
          opt.dataset.capabilities = JSON.stringify(e.capabilities || []);
          opt.dataset.id = e.id || "";
          opt.dataset.version = e.version || "";
          opt.dataset.channel = channel;
          opt.dataset.httpHosts = (e.http_hosts || []).join(",");
          opt.dataset.connectionHost = (e.connection_defaults && e.connection_defaults.host) || "";
          opt.dataset.connPort = (e.connection_defaults && e.connection_defaults.port) || "";
          opt.dataset.readOnly = e.read_only ? "true" : "false";
          sel.appendChild(opt);
        });
        S.catalogEntries = entries;
        renderCatalogCards(entries);
        syncCatalogProfilePicker();
      }

      function renderCatalogCards(entries) {
        var host = document.getElementById("driver-catalog-results");
        var search = document.getElementById("driver-catalog-search");
        var picker = document.getElementById("driver-catalog-picker");
        if (!host || !picker) return;

        var query = (search && search.value || "").trim();
        var matches = searchCatalog(entries, query);

        host.textContent = "";
        if (matches.length === 0) {
          var empty = document.createElement("div");
          empty.className = "drv-version-detail";
          empty.textContent = "No driver matches “" + query + "”. " +
            "Try the manufacturer, or the model printed on the unit.";
          host.appendChild(empty);
          return;
        }

        matches.forEach(function (e) {
          host.appendChild(catalogCard(e, picker, host, entries));
        });
      }

      function catalogCard(e, picker, host, entries) {
        var card = document.createElement("button");
        card.type = "button";
        card.className = "drv-catalog-card";
        if (picker.value === e.path) card.classList.add("is-chosen");

        var title = document.createElement("div");
        title.className = "drv-catalog-title";
        title.textContent = catalogTitle(e);
        card.appendChild(title);

        var tags = document.createElement("div");
        tags.className = "drv-catalog-tags";
        (e.capabilities || []).forEach(function (cap) {
          var tag = document.createElement("span");
          tag.className = "drv-catalog-tag";
          tag.textContent = cap;
          tags.appendChild(tag);
        });
        var verdict = verificationLabel(e.verification_status);
        if (verdict) {
          var badge = document.createElement("span");
          badge.className = "drv-catalog-tag drv-catalog-" +
            (e.verification_status === "production" ? "proven" : "unproven");
          badge.textContent = verdict;
          tags.appendChild(badge);
        }
        card.appendChild(tags);

        // The models this has actually been run against — the thing an
        // operator can compare with the label on their own unit.
        var models = (e.tested_models || []).slice(0, 3).join(", ");
        var foot = document.createElement("div");
        foot.className = "drv-catalog-foot";
        foot.textContent = [(e.protocols || []).join(" + "), e.version ? "v" + e.version : "", models]
          .filter(Boolean).join(" · ");
        card.appendChild(foot);

        card.addEventListener("click", function () {
          picker.value = e.path;
          // The rest of the form listens to the select, so tell it the value
          // moved rather than only setting it.
          picker.dispatchEvent(new Event("change", {bubbles: true}));
          renderCatalogCards(entries);
        });
        return card;
      }

      apiFetch("/api/drivers/catalog").then(function (r) { return r.json(); }).then(function (data) {
        var entries = (data && data.entries) || [];
        // Capability-driven reveal: show the Disable-PV checkbox only
        // on drivers whose catalog entry advertises BOTH meter and pv.
        // Other drivers can't double-count generation, so the toggle
        // would be meaningless. Looking up by `d.lua` ties the UI to
        // what the driver itself declares, not a hard-coded list.
        var byLua = {};
        entries.forEach(function (e) { if (e && e.path) byLua[e.path] = e; });
        // Cache by-lua so the synchronous render pass can detect
        // catalog-driven driver kinds (e.g. "vehicle") on re-renders
        // without waiting for the fetch to resolve again.
        S.catalogByLua = byLua;
        bodyEl.querySelectorAll(".driver-module-status").forEach(function (slot) {
          var entry = byLua[slot.getAttribute("data-drv-lua")];
          if (!entry) return;
          var source = entry.source || "bundled";
          var version = entry.installed_version || entry.version || "unknown";
          var summary = runningSummary(entry);
          // What is running, in words: which version, where the file came
          // from, and how well tested it is. The catalog has carried
          // verification_status all along and this view never showed it, so an
          // operator could update onto an untested driver without being told.
          var html = '<span class="creds-badge drv-module-headline">' + escHtml(summary.headline) + '</span>' +
            ' <span class="drv-module-detail">' + escHtml(summary.detail) + '</span>';
          // Always present, hidden when it does not apply, so a switch can
          // show or hide it: whether a driver may control anything differs
          // between versions of the same driver.
          html += ' <span class="creds-badge drv-module-readonly"' +
            (entry.read_only ? '' : ' style="display:none"') + '>telemetry only</span>';
          if (summary.updatable) {
            html += ' <button class="btn-add drv-module-update" type="button" data-driver-id="' + escHtml(entry.id) +
              '" data-repository-id="' + escHtml(entry.repository_id) + '" data-version="' + escHtml(entry.upstream_version) + '">Update to v' +
              escHtml(entry.upstream_version) + '</button>';
          }
          // An override shadows the channel, so installing a newer version
          // would not change what runs. Say that, rather than offering an
          // Update button that appears to do nothing.
          if (source === "local" && entry.upstream_version) {
            html += ' <span style="color:var(--text-dim);font-size:0.75rem">' +
              'official v' + escHtml(entry.upstream_version) + ' exists; your file keeps running</span>';
          }
          // One list, including for an override: seeing what else you could
          // run is the whole point when you are testing your own driver.
          // Rollback is gone -- stepping back is picking an older row.
          html += ' <button class="btn-add drv-module-versions" type="button" data-driver-id="' +
            escHtml(entry.id) + '" data-source="' + escHtml(source) + '" data-running-version="' +
            escHtml(version) + '" data-logical-path="' + escHtml(entry.path) + '">Versions</button>';
          // A driver is one Lua file and the repository is the source of
          // truth, but from here there was no way to read the code that is
          // running -- so a fix for someone's inverter stayed on their machine.
          html += ' <button class="btn-add drv-module-source" type="button" data-driver-id="' +
            escHtml(entry.id) + '">Source</button>';
          html += ' <span class="drv-module-action"></span>';
          html += '<div class="drv-module-versions-panel" style="display:none;margin-top:6px"></div>';
          html += '<div class="drv-module-source-panel" style="display:none;margin-top:6px"></div>';
          slot.innerHTML = html;
        });
        bodyEl.querySelectorAll(".drv-module-update").forEach(function (btn) {
          btn.addEventListener("click", function () {
            btn.disabled = true;
            var status = btn.parentElement.querySelector(".drv-module-action");
            if (status) status.textContent = " Validating and activating…";
            apiFetch("/api/device_repository/drivers/" + encodeURIComponent(btn.dataset.driverId) + "/install", {
              method: "POST", headers: {"Content-Type":"application/json"},
              body: JSON.stringify({repository_id: btn.dataset.repositoryId, version: btn.dataset.version})
            }).then(function (r) { return r.json().then(function (body) { if (!r.ok) throw new Error(body.error || "install failed"); return body; }); })
              .then(function () { if (status) status.textContent = " Updated; fresh telemetry verified."; btn.remove(); })
              .catch(function (err) { if (status) status.textContent = " " + err.message; btn.disabled = false; });
          });
        });
        bodyEl.querySelectorAll(".drv-module-versions").forEach(function (btn) {
          btn.addEventListener("click", function () {
            var panel = btn.parentElement.querySelector(".drv-module-versions-panel");
            if (!panel) return;
            if (panel.style.display !== "none") { panel.style.display = "none"; return; }
            panel.style.display = "block";
            panel.textContent = "Loading versions…";
            var id = btn.dataset.driverId;
            apiFetch("/api/device_repository/drivers/" + encodeURIComponent(id) + "/versions")
              .then(function (r) {
                return r.json().then(function (body) {
                  if (!r.ok) throw new Error(body.error || "could not list versions");
                  return body;
                });
              })
              .then(function (body) {
                renderVersionPicker(panel, id, body, {
                  overridden: btn.dataset.source === "local",
                  runningSource: btn.dataset.source,
                  runningVersion: btn.dataset.runningVersion,
                  logicalPath: btn.dataset.logicalPath,
                  // So a switch can correct the summary line above without
                  // re-rendering the tab, which would close this panel.
                  headlineEl: btn.parentElement.querySelector(".drv-module-headline"),
                  detailEl: btn.parentElement.querySelector(".drv-module-detail"),
                  updateEl: btn.parentElement.querySelector(".drv-module-update"),
                  readOnlyEl: btn.parentElement.querySelector(".drv-module-readonly")
                });
              })
              .catch(function (err) { panel.textContent = err.message; });
          });
        });
        bodyEl.querySelectorAll(".drv-module-source").forEach(function (btn) {
          btn.addEventListener("click", function () {
            var panel = btn.parentElement.querySelector(".drv-module-source-panel");
            if (!panel) return;
            if (panel.style.display !== "none") { panel.style.display = "none"; return; }
            panel.style.display = "block";
            panel.textContent = "Loading source…";
            var id = btn.dataset.driverId;
            apiFetch("/api/drivers/" + encodeURIComponent(id) + "/source")
              .then(function (r) {
                return r.json().then(function (body) {
                  if (!r.ok) throw new Error(body.error || "could not read the driver");
                  return body;
                });
              })
              .then(function (body) { renderDriverSource(panel, body); })
              .catch(function (err) { panel.textContent = err.message; });
          });
        });
        bodyEl.querySelectorAll(".drv-profile-slot").forEach(function (slot) {
          var dIdx = parseInt(slot.getAttribute("data-driver-idx"), 10);
          var d = config.drivers[dIdx];
          if (!d || !d.lua) return;
          var entry = byLua[d.lua];
          var profiles = configProfilesFor(entry);
          if (profiles.length === 0) return;
          var current = (d.config && d.config.profile) || profiles[0].value;
          var fs = '<fieldset><legend>Driver profile</legend>' +
            '<label>Register profile ' + help('Choose the register map for this GoodWe model. Changing the profile also fills its usual Unit ID; you can edit that ID before saving.') + '</label>' +
            '<select class="drv-profile-select" data-driver-idx="' + dIdx + '" data-path="drivers.' + dIdx + '.config.profile">';
          profiles.forEach(function (profile) {
            fs += '<option value="' + escHtml(profile.value) + '"' + (profile.value === current ? ' selected' : '') + '>' +
              escHtml(profile.label + " · Unit ID " + profile.unitId) + '</option>';
          });
          fs += '</select></fieldset>';
          slot.innerHTML = fs;
          var select = slot.querySelector(".drv-profile-select");
          if (select) select.addEventListener("change", function () {
            var selected = profileByValue(profiles, select.value);
            var unitInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.capabilities.modbus.unit_id"]');
            if (selected && unitInput) unitInput.value = String(selected.unitId);
          });
        });
        // Populate per-driver secret inputs (api_token, etc.) using the
        // catalog's config_secrets list. Each input uses the standard
        // data-path="drivers.<idx>.config.<key>" so the settings shell
        // saves it back into config.drivers[idx].config[key] like any
        // other form field. Empty existing values render as empty
        // password inputs; the `has_<key>` mirror for masked-saved
        // semantics is intentionally not modeled here — operators can
        // re-enter the token if they need to rotate it.
        bodyEl.querySelectorAll(".drv-secrets-slot").forEach(function (slot) {
          var dIdx = parseInt(slot.getAttribute("data-driver-idx"), 10);
          var d = config.drivers[dIdx];
          if (!d || !d.lua) return;
          var entry = byLua[d.lua];
          var secrets = (entry && entry.config_secrets) || [];
          var dcfg = d.config || {};
          // OAuth apicreds drivers (client_id/client_secret) render the
          // Client Secret in their own "MyUplink connection" fieldset, and
          // the refresh_token is managed by the Connect flow — never hand-
          // entered. Drop both from the generic Secrets section so the
          // secret isn't duplicated and a confusing "Refresh Token" input
          // doesn't appear.
          var isApiCreds = Object.prototype.hasOwnProperty.call(dcfg, 'client_id') ||
                           Object.prototype.hasOwnProperty.call(dcfg, 'client_secret');
          if (isApiCreds) {
            secrets = secrets.filter(function (k) {
              return k !== 'client_secret' && k !== 'refresh_token';
            });
          }
          if (secrets.length === 0) return;
          var fs = '<fieldset><legend>Secrets</legend>';
          secrets.forEach(function (key) {
            // Title-case, keep the raw key for the data-path attribute.
            // BOTH go through ctx.escHtml — config_secrets ultimately
            // comes from driver-authored Lua and a hostile/malformed
            // key containing < or > would otherwise be parsed as
            // markup when we innerHTML this fieldset. Same for the
            // value-readback branch (paranoia: the masked placeholder
            // is server-controlled, but a downstream change might
            // make this user-controlled).
            var label = key.replace(/_/g, " ").replace(/\b\w/g, function (c) { return c.toUpperCase(); });
            // Render the input EMPTY regardless of stored value — the
            // value coming back from /api/config is the masked
            // placeholder anyway (api masks driver config_secrets on
            // GET), but inserting any value into a `value=""` attribute
            // exposes it in the DOM/HTML. Mirror the cloud-password
            // pattern instead: empty input + saved/missing badge.
            // config_secrets come back masked: a non-empty value means the api
            // sent the placeholder (e.g. api_token). For the "password" key the
            // api blanks config.password but sets the driver-level has_password
            // flag, so honour that too — otherwise a saved password reads as unset.
            var saved = (typeof dcfg[key] === "string" && dcfg[key] !== "") ||
                        (key === "password" && d.has_password === true);
            var badge = saved
              ? '<span class="creds-badge creds-saved">✓ Set — not shown here</span>'
              : '<span class="creds-badge creds-missing">⚠ Not set</span>';
            var placeholder = saved
              ? "•••••••• (leave empty to keep)"
              : "Paste from device web UI";
            fs +=
              '<label>' + escHtml(label) + ' ' + badge + '</label>' +
              '<input type="password" autocomplete="off" ' +
              'data-path="drivers.' + dIdx + '.config.' + escHtml(key) + '" ' +
              'value="" placeholder="' + escHtml(placeholder) + '">';
          });
          fs += '</fieldset>';
          slot.innerHTML = fs;
        });
        bodyEl.querySelectorAll(".drv-disable-pv").forEach(function (lbl) {
          var lua = lbl.getAttribute("data-drv-lua");
          var entry = lua && byLua[lua];
          if (!entry) return;
          var caps = entry.capabilities || [];
          if (caps.indexOf("meter") >= 0 && caps.indexOf("pv") >= 0) {
            lbl.style.display = "flex";
          }
        });
        bodyEl.querySelectorAll(".drv-disable-battery").forEach(function (lbl) {
          var lua = lbl.getAttribute("data-drv-lua");
          var entry = lua && byLua[lua];
          if (!entry) return;
          var caps = entry.capabilities || [];
          if (caps.indexOf("meter") >= 0 && caps.indexOf("battery") >= 0) {
            lbl.style.display = "flex";
          }
        });
        bodyEl.querySelectorAll(".driver-battery-capacity").forEach(function (wrap) {
          var lua = wrap.getAttribute("data-drv-lua");
          var entry = lua && byLua[lua];
          var row = wrap.closest(".device-core-row");
          var caps = (entry && entry.capabilities) || [];
          var show = caps.indexOf("battery") >= 0 && !(entry && entry.read_only);
          wrap.hidden = !show;
          wrap.style.display = show ? "" : "none";
          if (row) row.classList.toggle("field-row-single", !show);
        });
        // Local-API credential fields (username + cert pin) reveal only for
        // drivers whose catalog declares apicreds — done post-fetch so a first
        // render before the catalog resolves doesn't drop them.
        bodyEl.querySelectorAll(".drv-local-creds").forEach(function (wrap) {
          var lua = wrap.getAttribute("data-drv-lua");
          var entry = lua && byLua[lua];
          var caps = (entry && entry.capabilities) || [];
          wrap.hidden = caps.indexOf("apicreds") < 0;
        });
        populateCatalogPicker(entries, "stable");
      });

      // Filtering happens over the catalog already fetched, so typing costs
      // nothing and there is no debounce to get wrong.
      var catalogSearch = document.getElementById("driver-catalog-search");
      if (catalogSearch) catalogSearch.addEventListener("input", function () {
        renderCatalogCards(S.catalogEntries || []);
      });

      var channelSelect = document.getElementById("driver-catalog-channel");
      if (channelSelect) channelSelect.addEventListener("change", function () {
        if (channelSelect.value === "stable") {
          apiFetch("/api/drivers/catalog").then(function (r) { return r.json(); }).then(function (data) {
            populateCatalogPicker((data && data.entries) || [], "stable");
          });
          return;
        }
        var sel = document.getElementById("driver-catalog-picker");
        if (sel) sel.innerHTML = "<option value=''>Loading signed beta…</option>";
        apiFetch("/api/device_repository/catalog?channel=beta")
          .then(function (r) { return r.json().then(function (body) { if (!r.ok) throw new Error(body.error || "beta catalog failed"); return body; }); })
          .then(function (data) {
            var entries = ((data && data.entries) || []).map(function (candidate) {
              var signed = (candidate && candidate.driver) || {};
              return Object.assign({}, signed.metadata || {}, {
                id: signed.id,
                path: signed.path,
                filename: signed.filename,
                version: signed.version,
                read_only: signed.read_only
              });
            });
            populateCatalogPicker(entries, "beta");
          })
          .catch(function (err) {
            if (sel) {
              sel.innerHTML = "";
              var opt = document.createElement("option");
              opt.value = "";
              opt.textContent = err.message;
              sel.appendChild(opt);
            }
          });
      });

      var catalogPicker = document.getElementById("driver-catalog-picker");
      if (catalogPicker) catalogPicker.addEventListener("change", syncCatalogProfilePicker);

      var btn = document.getElementById("driver-catalog-add");
      if (btn) btn.addEventListener("click", function () {
        var sel = document.getElementById("driver-catalog-picker");
        var nameEl = document.getElementById("driver-catalog-name");
        if (!sel || !sel.value) {
          // Doing nothing silently reads as a broken button.
          var results = document.getElementById("driver-catalog-results");
          if (results) {
            var say = results.querySelector(".drv-catalog-hint") ||
              results.insertBefore(document.createElement("div"), results.firstChild);
            say.className = "drv-catalog-hint drv-version-detail";
            say.textContent = "Pick a device above first.";
          }
          return;
        }
        ctx.captureCurrentTab();
        var chosen = sel.options[sel.selectedIndex];
        var protocols = (chosen.dataset.protocols || "").split("+");
        var name = (nameEl.value || "").trim() || chosen.dataset.id || ("driver-" + config.drivers.length);
        var driver = { name: name, lua: sel.value };
        driver.capabilities = {};
        if (protocols.indexOf("mqtt") >= 0) driver.capabilities.mqtt = { host: "", port: 1883 };
        if (protocols.indexOf("modbus") >= 0) driver.capabilities.modbus = { host: "", port: 502, unit_id: 1 };
        var profileSelect = document.getElementById("driver-catalog-profile");
        var profiles = configProfilesFor({ id: chosen.dataset.id, version: chosen.dataset.version });
        if (profileSelect && profiles.length > 0) {
          var selectedProfile = profileByValue(profiles, profileSelect.value);
          driver.config = { profile: selectedProfile.value };
          if (driver.capabilities.modbus) driver.capabilities.modbus.unit_id = selectedProfile.unitId;
        }
        if (protocols.indexOf("http") >= 0) {
          var hosts = (chosen.dataset.httpHosts || "").split(",").filter(Boolean);
          driver.capabilities.http = { allowed_hosts: hosts };
          // Vehicle drivers (e.g. tesla_vehicle) take {ip, vin}, not
          // {host} or {email,password,serial}. Detect via catalog
          // capability so existing local-HTTP and cloud branches stay
          // untouched.
          var entry = (S.catalogByLua || {})[sel.value] || {
            capabilities: JSON.parse(chosen.dataset.capabilities || "[]"),
            read_only: chosen.dataset.readOnly === "true"
          };
          var entryCaps = (entry && entry.capabilities) || [];
          if (entry && entry.read_only && entryCaps.indexOf("battery") >= 0) {
            // Admit the battery reading without adding the gateway to the
            // dispatch pool. battery_capacity_wh remains the control opt-in.
            driver.battery_telemetry_only = true;
          }
          var connHost = chosen.dataset.connectionHost || "";
          if (entryCaps.indexOf("vehicle") >= 0) {
            driver.config = { ip: "", vin: "" };
          } else if (connHost) {
            driver.config = { host: connHost };
		  } else if (entryCaps.indexOf("apicreds") >= 0 && (chosen.dataset.connPort || "")) {
            // Local HTTP device with HTTP Basic auth (e.g. NIBE local REST API):
            // host + username + password + an optional self-signed cert pin. The
            // backend auto-derives capabilities.http.allowed_hosts from config.host.
            driver.config = { host: "", username: "", password: "" };
		  } else if (entryCaps.indexOf("apicreds") >= 0) {
            // OAuth2 client_credentials drivers (e.g. MyUplink).
			  driver.config = { client_id: "", client_secret: "" };
		  } else if (entryCaps.indexOf("meter") >= 0 ||
                     entryCaps.indexOf("pv") >= 0 ||
                     entryCaps.indexOf("battery") >= 0) {
            // Local-HTTP meter / PV / battery driver without a canned
            // hostname in connection_defaults (typical for generic
            // ESPHome / DSMR firmwares — every install picks its own
            // mDNS name from `name:` in the YAML, so no hard-coded
            // default would be right). The operator types the IP or
            // hostname into the Host/IP field; empty config is a
            // local-HTTP signal, NOT a cloud one.
			  driver.config = { host: "" };
          } else {
            // Account-based cloud service (easee_cloud, etc.). HTTP
            // capability + no host pre-fill + no meter/pv/battery
            // capability narrows the field down enough to assume
            // {email, password, serial} is the right shape.
            driver.config = { email: "", password: "", serial: "" };
          }
        }
        var finishAdd = function () {
          config.drivers.push(driver);
          ctx.renderTab("devices");
        };
        if (chosen.dataset.channel !== "beta") {
          finishAdd();
          return;
        }
        btn.disabled = true;
        btn.textContent = "Installing signed beta…";
        apiFetch("/api/device_repository/drivers/" + encodeURIComponent(chosen.dataset.id) + "/install", {
          method: "POST", headers: {"Content-Type":"application/json"},
          body: JSON.stringify({channel: "beta", version: chosen.dataset.version})
        }).then(function (r) {
          return r.json().then(function (body) { if (!r.ok) throw new Error(body.error || "beta install failed"); return body; });
        }).then(finishAdd).catch(function (err) {
          window.alert("Beta driver install failed: " + err.message);
          btn.disabled = false;
          btn.textContent = "+ Add selected";
        });
      });

      // Cloud-driver Connect buttons.
      bodyEl.querySelectorAll(".ev-connect-btn").forEach(function (connectBtn) {
        connectBtn.addEventListener("click", function () {
          var dIdx = connectBtn.dataset.driverIdx;
          var statusEl = document.getElementById("ev-connect-status-" + dIdx);
          var sel = document.getElementById("ev-charger-select-" + dIdx);
          var emailInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.email"]');
          var pwInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.password"]');
          var email = emailInput ? emailInput.value : "";
          var pw = pwInput ? pwInput.value : "";
          if (!email) { if (statusEl) statusEl.textContent = "Enter email first"; return; }
          if (statusEl) statusEl.textContent = "Connecting...";
          connectBtn.disabled = true;
          var dCfg = config && config.drivers ? config.drivers[dIdx] : null;
          var provider = "easee";
          if (dCfg && typeof dCfg.lua === "string" && dCfg.lua !== "") {
            provider = dCfg.lua
              .replace(/^.*[\\/]/, "")
              .replace(/\.lua$/i, "")
              .replace(/_cloud$/i, "");
            if (!provider) provider = "easee";
          }
          apiFetch("/api/ev/chargers", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ provider: provider, email: email, password: pw }),
          }).then(function (r) {
            if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || "HTTP " + r.status); });
            return r.json();
          }).then(function (chargers) {
            if (!sel || !Array.isArray(chargers) || chargers.length === 0) {
              if (statusEl) statusEl.textContent = "No chargers found";
              return;
            }
            var d = config.drivers[dIdx];
            var current = (d && d.config && d.config.serial) || "";
            sel.innerHTML = "";
            chargers.forEach(function (ch) {
              var opt = document.createElement("option");
              opt.value = ch.id;
              opt.textContent = ch.id + (ch.name ? "  —  " + ch.name : "");
              if (ch.id === current) opt.selected = true;
              sel.appendChild(opt);
            });
            var selected = sel.value;
            if (d && d.config) d.config.serial = selected;
            if (config.ev_charger) config.ev_charger.serial = selected;
            sel.onchange = function () {
              if (d && d.config) d.config.serial = sel.value;
              if (config.ev_charger) config.ev_charger.serial = sel.value;
            };
            if (statusEl) statusEl.textContent = chargers.length + " charger(s) found";
          }).catch(function (e) {
            if (statusEl) statusEl.textContent = "Error: " + e.message;
          }).finally(function () {
            connectBtn.disabled = false;
          });
        });
      });

      // Tesla "Verify connection" buttons. Issues a backend probe
      // against the configured proxy IP + VIN and renders the result
      // inline. Backend handles SSRF hardening — the UI just collects
      // the two fields and displays the response.
      bodyEl.querySelectorAll(".tesla-verify-btn").forEach(function (vbtn) {
        vbtn.addEventListener("click", function () {
          var dIdx = vbtn.dataset.driverIdx;
          var statusEl = bodyEl.querySelector('.tesla-verify-status[data-driver-idx="' + dIdx + '"]');
          var ipInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.ip"]');
          var vinInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.vin"]');
          var ip = ipInput ? ipInput.value.trim() : "";
          var vin = vinInput ? vinInput.value.trim() : "";
          if (!ip || !vin) {
            if (statusEl) statusEl.textContent = "Enter Proxy IP + VIN first";
            return;
          }
          if (statusEl) { statusEl.textContent = "Verifying…"; statusEl.style.color = "var(--text-dim)"; }
          vbtn.disabled = true;
          apiFetch("/api/drivers/verify_tesla", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ ip: ip, vin: vin }),
          }).then(function (r) {
            return r.json().then(function (j) { return { ok: r.ok, status: r.status, body: j }; });
          }).then(function (res) {
            if (!statusEl) return;
            if (res.ok && res.body && res.body.ok) {
              var soc = res.body.soc_pct != null ? Math.round(res.body.soc_pct) + "%" : "?";
              var lim = res.body.charge_limit_pct != null ? Math.round(res.body.charge_limit_pct) + "%" : "?";
              var st = res.body.charging_state || "";
              statusEl.style.color = "var(--green-e)";
              statusEl.textContent = "✓ SoC " + soc + " · limit " + lim + (st ? " · " + st : "");
            } else {
              statusEl.style.color = "var(--red-e)";
              statusEl.textContent = "✗ " + ((res.body && res.body.error) || "verification failed");
            }
          }).catch(function (e) {
            if (statusEl) {
              statusEl.style.color = "var(--red-e)";
              statusEl.textContent = "✗ " + e.message;
            }
          }).finally(function () {
            vbtn.disabled = false;
          });
        });
      });

      // MyUplink (and any OAuth authorization-code apicreds driver): open the
      // provider consent in a new tab. /start reads the SAVED client_id, so
      // the operator must save the row first; we pass the browser's own
      // callback URL (location.origin) so the server does not need to infer it.
      // The refresh_token lands server-side; the operator
      // reloads to see the badge flip to Connected.
      bodyEl.querySelectorAll(".myuplink-connect-btn").forEach(function (cbtn) {
        cbtn.addEventListener("click", function () {
          var dIdx = cbtn.dataset.driverIdx;
          var name = cbtn.dataset.driverName || "";
          var statusEl = bodyEl.querySelector('.myuplink-connect-status[data-driver-idx="' + dIdx + '"]');
          function setStatus(msg, color) {
            if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || "var(--text-dim)"; }
          }
          if (!name) { setStatus("Save the driver name first", "var(--red-e)"); return; }
          setStatus("Opening MyUplink…");
          cbtn.disabled = true;
          var redirectURI = location.origin + "/api/oauth/myuplink/callback";
          var qs = "?driver=" + encodeURIComponent(name) +
            "&redirect_uri=" + encodeURIComponent(redirectURI);
          apiFetch("/api/oauth/myuplink/start" + qs)
            .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
            .then(function (res) {
              if (!res.ok || !res.body || !res.body.authorize_url) {
                setStatus("✗ " + ((res.body && res.body.error) || "could not start consent — save Client ID + Secret first"), "var(--red-e)");
                return;
              }
              window.open(res.body.authorize_url, "_blank");
              setStatus("Complete the consent in the new tab. If it returns here, reload; if not, paste the URL below.", "var(--green-e)");
            })
            .catch(function (e) { setStatus("✗ " + e.message, "var(--red-e)"); })
            .finally(function () { cbtn.disabled = false; });
        });
      });

      // Manual fallback: exchange a pasted redirect URL (carries code + state)
      // server-side. Works on any origin since the Pi exchanges the code over
      // outbound HTTPS — no inbound callback required.
      bodyEl.querySelectorAll(".myuplink-manual-btn").forEach(function (mbtn) {
        mbtn.addEventListener("click", function () {
          var dIdx = mbtn.dataset.driverIdx;
          var input = bodyEl.querySelector('.myuplink-manual-url[data-driver-idx="' + dIdx + '"]');
          var statusEl = bodyEl.querySelector('.myuplink-connect-status[data-driver-idx="' + dIdx + '"]');
          function setStatus(msg, color) {
            if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || "var(--text-dim)"; }
          }
          var url = input ? input.value.trim() : "";
          if (!url) { setStatus("Paste the redirect URL first", "var(--red-e)"); return; }
          setStatus("Completing…");
          mbtn.disabled = true;
          apiFetch("/api/oauth/myuplink/exchange", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ redirect_url: url }),
          })
            .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
            .then(function (res) {
              if (res.ok && res.body && res.body.status === "connected") {
                setStatus("✓ Connected — reload to refresh the badge.", "var(--green-e)");
              } else {
                setStatus("✗ " + ((res.body && res.body.error) || "exchange failed"), "var(--red-e)");
              }
            })
            .catch(function (e) { setStatus("✗ " + e.message, "var(--red-e)"); })
            .finally(function () { mbtn.disabled = false; });
        });
      });

      // Generic driver probe. Runs the current row's unsaved config through a
      // short-lived backend driver instance and dumps live readings/metrics
      // inline so the operator can verify host, credentials, and protocol.
      bodyEl.querySelectorAll(".driver-test-btn").forEach(function (testBtn) {
        testBtn.addEventListener("click", function () {
          var dIdx = testBtn.dataset.driverIdx;
          var statusEl = bodyEl.querySelector('.driver-test-status[data-driver-idx="' + dIdx + '"]');
          var outputEl = bodyEl.querySelector('.driver-test-output[data-driver-idx="' + dIdx + '"]');
          ctx.captureCurrentTab();
          var driver = config.drivers && config.drivers[dIdx];
          if (!driver) return;
          if (statusEl) {
            statusEl.textContent = "Testing...";
            statusEl.className = "driver-test-status";
          }
          if (outputEl) {
            outputEl.hidden = false;
            outputEl.innerHTML = '<div class="driver-test-empty">Waiting for live values...</div>';
          }
          testBtn.disabled = true;
          apiFetch("/api/drivers/test", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(driver),
          }).then(function (r) {
            return r.json().then(function (j) { return { ok: r.ok, status: r.status, body: j }; });
          }).then(function (res) {
            var body = res.body || {};
            if (!res.ok) {
              body = { ok: false, error: body.error || ("HTTP " + res.status) };
            }
            if (statusEl) {
              statusEl.textContent = body.ok ? "Connected" : "Failed";
              statusEl.className = "driver-test-status " + (body.ok ? "ok" : "error");
            }
            if (outputEl) {
              outputEl.hidden = false;
              outputEl.innerHTML = renderProbeOutput(body);
            }
          }).catch(function (e) {
            if (statusEl) {
              statusEl.textContent = "Failed";
              statusEl.className = "driver-test-status error";
            }
            if (outputEl) {
              outputEl.hidden = false;
              outputEl.innerHTML = '<div class="driver-test-error">' + escHtml(e.message) + '</div>';
            }
          }).finally(function () {
            testBtn.disabled = false;
          });
        });
      });

      // Auto-sync capabilities.http.allowed_hosts from the configured
      // Proxy IP. Without this, a fresh tesla driver gets allowed_hosts=[]
      // (set by catalog-add) and every host.http_get call returns
      // "host not in allowed_hosts" — driver never reaches the proxy
      // and watchdog flips it stale. Strip any ":port" suffix; the
      // allowlist is matched on hostname only.
      bodyEl.querySelectorAll(".tesla-ip-input").forEach(function (inp) {
        function syncAllowedHosts() {
          var dIdx = inp.dataset.driverIdx;
          var d = config.drivers[dIdx];
          if (!d || !d.capabilities) return;
          if (!d.capabilities.http) d.capabilities.http = { allowed_hosts: [] };
          var ip = (inp.value || "").trim();
          var bare = ip.split(":")[0];
          d.capabilities.http.allowed_hosts = bare ? [bare] : [];
        }
        inp.addEventListener("input", syncAllowedHosts);
        inp.addEventListener("blur", syncAllowedHosts);
      });

      // Add/remove-device buttons.
      var addMqtt = document.getElementById("add-mqtt");
      var addModbus = document.getElementById("add-modbus");
      if (addMqtt) addMqtt.addEventListener("click", function () {
        ctx.captureCurrentTab();
        config.drivers.push({
          name: "new-device-" + (config.drivers.length + 1),
          lua: "drivers/new.lua",
          is_site_meter: false,
          battery_capacity_wh: 0,
          mqtt: { host: "", port: 1883, username: "", password: "" },
        });
        ctx.renderTab("devices");
      });
      if (addModbus) addModbus.addEventListener("click", function () {
        ctx.captureCurrentTab();
        config.drivers.push({
          name: "new-device-" + (config.drivers.length + 1),
          lua: "drivers/new.lua",
          is_site_meter: false,
          battery_capacity_wh: 0,
          modbus: { host: "", port: 502, unit_id: 1 },
        });
        ctx.renderTab("devices");
      });
      bodyEl.querySelectorAll("[data-remove-idx]").forEach(function (rmBtn) {
        rmBtn.addEventListener("click", function () {
          var idx = parseInt(rmBtn.dataset.removeIdx);
          ctx.captureCurrentTab();
          config.drivers.splice(idx, 1);
          ctx.renderTab("devices");
        });
      });
    },
  };
})();
