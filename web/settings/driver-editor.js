// The driver editor: a full-height view over the settings modal, with syntax
// highlighting and two linters.
//
// A driver is tens of kilobytes of Lua. Editing that in a 300-pixel textarea
// wedged into a device row is not editing, it is squinting -- so this opens as
// its own surface with room to read the code.
//
// Ace is vendored under /vendor/ace/ rather than pulled from a CDN, for the
// same reason three.js is: a gateway has to work without the internet, and a
// driver editor is most needed exactly when something is wrong. It loads on
// first open, not on page load.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });

  var ACE_BASE = "/vendor/ace";
  var acePromise = null;

  function loadScript(src) {
    return new Promise(function (resolve, reject) {
      var el = document.createElement("script");
      el.src = src;
      el.onload = resolve;
      el.onerror = function () { reject(new Error("could not load " + src)); };
      document.head.appendChild(el);
    });
  }

  // Ace's own files reference each other by base path, and the Lua worker is
  // fetched separately when the mode starts.
  function loadAce() {
    if (acePromise) return acePromise;
    acePromise = loadScript(ACE_BASE + "/ace.js")
      .then(function () {
        window.ace.config.set("basePath", ACE_BASE);
        window.ace.config.set("workerPath", ACE_BASE);
        return Promise.all([
          loadScript(ACE_BASE + "/mode-lua.js"),
          loadScript(ACE_BASE + "/theme-tomorrow_night.js"),
          loadScript(ACE_BASE + "/ext-searchbox.js")
        ]);
      })
      .then(function () { return window.ace; })
      .catch(function (err) {
        // Let the next attempt retry rather than caching the failure.
        acePromise = null;
        throw err;
      });
    return acePromise;
  }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  // open() takes the driver's source payload and the actions the devices tab
  // owns, so this file knows how to edit a driver and nothing about how a
  // draft is run.
  //
  // actions: { runDraft(lua, minutes), keepDraft(), revertDraft(),
  //            draftStatus(), lint(lua), suggest(lua) }
  S.openDriverEditor = function (body, actions) {
    var overlay = el("div", "drv-editor-overlay");
    var shell = el("div", "drv-editor-shell");

    var head = el("div", "drv-editor-head");
    head.appendChild(el("strong", null, body.filename || body.id));
    var provenance = el("span", "drv-editor-provenance",
      [body.version ? "v" + body.version : "", body.sourceLabel, describeSize(body.bytes)]
        .filter(Boolean).join(" · "));
    head.appendChild(provenance);

    var spacer = el("span");
    spacer.style.flex = "1";
    head.appendChild(spacer);

    if (body.repository_url) {
      var link = document.createElement("a");
      link.href = body.repository_url;
      link.target = "_blank";
      link.rel = "noopener";
      link.className = "drv-editor-link";
      link.textContent = "Open in device-drivers";
      head.appendChild(link);
    }

    var close = el("button", "btn-add", "Close");
    close.type = "button";
    head.appendChild(close);
    shell.appendChild(head);

    var host = el("div", "drv-editor-surface");
    shell.appendChild(host);

    var footer = el("div", "drv-editor-foot");
    var problems = el("div", "drv-editor-problems");
    footer.appendChild(problems);
    shell.appendChild(footer);

    var controls = el("div", "drv-editor-controls");
    var windowPicker = document.createElement("select");
    [5, 10, 30, 60].forEach(function (minutes) {
      var opt = document.createElement("option");
      opt.value = String(minutes);
      opt.textContent = "Try for " + minutes + " min";
      if (minutes === 10) opt.selected = true;
      windowPicker.appendChild(opt);
    });
    controls.appendChild(windowPicker);

    var run = el("button", "btn-add", "Run this draft");
    run.type = "button";
    controls.appendChild(run);

    var suggest = el("button", "btn-add", "Suggest to repo");
    suggest.type = "button";
    controls.appendChild(suggest);

    // Its own class: the devices tab has a status line too, and two
    // elements sharing one name makes both harder to reason about.
    var status = el("span", "drv-editor-status");
    controls.appendChild(status);
    footer.appendChild(controls);

    overlay.appendChild(shell);
    document.body.appendChild(overlay);

    var editor = null;
    var closed = false;

    function dispose() {
      if (closed) return;
      closed = true;
      if (editor) editor.destroy();
      overlay.remove();
      document.removeEventListener("keydown", onKey);
    }
    function onKey(e) {
      // Escape closes, but not while the find bar has it.
      if (e.key === "Escape" && !overlay.querySelector(".ace_search")) dispose();
    }
    document.addEventListener("keydown", onKey);
    close.addEventListener("click", dispose);

    host.textContent = "Loading editor…";
    loadAce().then(function (ace) {
      host.textContent = "";
      editor = ace.edit(host);
      editor.setTheme("ace/theme/tomorrow_night");
      editor.session.setMode("ace/mode/lua");
      editor.session.setValue(body.lua || "", -1);
      editor.setOptions({
        fontSize: "12px",
        showPrintMargin: false,
        useSoftTabs: true,
        tabSize: 2,
        // A 35 kB driver is unreadable without these.
        showFoldWidgets: true,
        highlightActiveLine: true,
        scrollPastEnd: 0.4
      });
      editor.focus();

      wireLinting(editor, actions, problems);
      resumeDraft();
    }).catch(function (err) {
      host.textContent = err.message;
    });

    function currentSource() {
      return editor ? editor.getValue() : (body.lua || "");
    }

    run.addEventListener("click", function () {
      if (!editor) return;
      run.disabled = true;
      status.textContent = "Checking…";
      // The server's parser is the one that decides whether the driver
      // starts, so it gates running rather than merely advising.
      actions.lint(currentSource()).then(function (result) {
        if (result && result.ok === false) {
          status.textContent = "Not run: the draft does not compile.";
          run.disabled = false;
          return null;
        }
        status.textContent = "Starting…";
        return actions.runDraft(currentSource(), parseInt(windowPicker.value, 10));
      }).then(function (b) {
        if (!b) return;
        showRunning(b.expires_at_ms);
      }).catch(function (err) {
        status.textContent = err.message;
        run.disabled = false;
      });
    });

    suggest.addEventListener("click", function () {
      actions.suggest(currentSource(), function (message) {
        status.textContent = message;
      });
    });

    function resumeDraft() {
      actions.draftStatus().then(function (b) {
        if (b && b.running) showRunning(b.expires_at_ms);
      }).catch(function () { /* nothing running is the normal case */ });
    }

    var ticker = null;
    function showRunning(expiresAtMS) {
      run.disabled = true;
      controls.querySelectorAll(".drv-editor-action").forEach(function (n) { n.remove(); });

      function tick() {
        var left = Math.max(0, Math.round((expiresAtMS - Date.now()) / 1000));
        if (left <= 0) {
          status.textContent = "The draft expired and the previous driver is back.";
          clearInterval(ticker);
          run.disabled = false;
          controls.querySelectorAll(".drv-editor-action").forEach(function (n) { n.remove(); });
          return;
        }
        status.textContent = "Draft running · reverts in " + Math.floor(left / 60) + ":" +
          (left % 60 < 10 ? "0" : "") + (left % 60);
      }
      clearInterval(ticker);
      ticker = setInterval(tick, 1000);
      tick();

      function action(label, call, done) {
        var btn = el("button", "btn-add drv-editor-action", label);
        btn.type = "button";
        btn.addEventListener("click", function () {
          btn.disabled = true;
          call().then(function () {
            clearInterval(ticker);
            status.textContent = done;
            run.disabled = false;
            controls.querySelectorAll(".drv-editor-action").forEach(function (n) { n.remove(); });
          }).catch(function (err) {
            status.textContent = err.message;
            btn.disabled = false;
          });
        });
        controls.insertBefore(btn, status);
      }
      action("Keep it", actions.keepDraft, "Kept. This is your own file now.");
      action("Put it back", actions.revertDraft, "Reverted.");
    }

    return dispose;
  };

  // Ace lints as you type with its own luaparse; the server is asked once
  // typing pauses. They can disagree, and when they do the server wins on
  // screen -- it is the parser that decides whether the driver starts.
  function wireLinting(editor, actions, problemsEl) {
    var pending = null;

    function render(serverProblems) {
      problemsEl.textContent = "";
      var aceAnnotations = editor.session.getAnnotations() || [];
      var list = (serverProblems || []).slice();

      // Ace's syntax errors are already in the gutter. Only surface them here
      // when the server has not spoken yet, so the two do not stack up.
      if (list.length === 0) {
        aceAnnotations.forEach(function (a) {
          if (a.type === "error") {
            list.push({line: (a.row || 0) + 1, severity: "error", message: a.text});
          }
        });
      }

      if (list.length === 0) {
        problemsEl.appendChild(el("span", "drv-editor-clean", "No problems found."));
        return;
      }
      list.slice(0, 12).forEach(function (p) {
        var row = el("button", "drv-editor-problem drv-editor-" + (p.severity || "error"));
        row.type = "button";
        row.textContent = (p.line ? "line " + p.line + " · " : "") + p.message;
        if (p.line) {
          row.addEventListener("click", function () {
            editor.gotoLine(p.line, 0, true);
            editor.focus();
          });
        }
        problemsEl.appendChild(row);
      });
    }

    // The gutter updates on its own; this mirrors it into the panel and asks
    // the server once the operator stops typing.
    editor.session.on("changeAnnotation", function () { render(null); });
    editor.session.on("change", function () {
      clearTimeout(pending);
      pending = setTimeout(function () {
        actions.lint(editor.getValue()).then(function (result) {
          var problems = (result && result.problems) || [];
          // Mark the server's verdict in the gutter too, so the two linters
          // do not point at different lines.
          editor.session.setAnnotations(problems.map(function (p) {
            return {
              row: Math.max(0, (p.line || 1) - 1),
              column: 0,
              text: p.message,
              type: p.severity === "warning" ? "warning" : "error"
            };
          }));
          render(problems);
        }).catch(function () { /* keep whatever Ace already showed */ });
      }, 600);
    });
  }

  function describeSize(bytes) {
    if (typeof bytes !== "number" || bytes <= 0) return "";
    if (bytes < 1024) return bytes + " bytes";
    return (bytes / 1024).toFixed(1) + " kB";
  }
})();
