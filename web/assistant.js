// Ask why — a short conversation about this box.
//
// The question is a chat line. The answer is a separate message. Progress
// streams while the model works. Opening a GitHub issue uses one filled
// markdown body, not the YAML bug form.
(function () {
  "use strict";

  var ISSUE_NEW = "https://github.com/srcfl/ftw/issues/new?template=ask_why.md";

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  function escHtml(s) {
    var div = document.createElement("div");
    div.textContent = s == null ? "" : String(s);
    return div.innerHTML;
  }

  // Inline marks. Code spans are lifted out first so ** and * inside a
  // span stay literal.
  function inlineMd(s) {
    var codes = [];
    s = escHtml(s);
    s = s.replace(/`([^`]+)`/g, function (m, c) {
      codes.push(c);
      return "\u0000" + (codes.length - 1) + "\u0000";
    });
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/(^|[\s(])\*([^*\n]+)\*/g, "$1<em>$2</em>");
    return s.replace(/\u0000(\d+)\u0000/g, function (m, i) {
      return "<code>" + codes[Number(i)] + "</code>";
    });
  }

  // The model is asked for markdown, so render markdown: headings, both
  // kinds of list, fenced code and paragraphs.
  function formatAnswer(text) {
    var lines = String(text == null ? "" : text).replace(/\r\n/g, "\n").split("\n");
    var out = [];
    var para = [];
    var listTag = null;
    var items = [];
    var code = null;
    function flushPara() {
      if (!para.length) return;
      out.push("<p>" + inlineMd(para.join("\n")).replace(/\n/g, "<br>") + "</p>");
      para = [];
    }
    function flushList() {
      if (!listTag) return;
      out.push("<" + listTag + ">" + items.map(function (i) {
        return "<li>" + inlineMd(i) + "</li>";
      }).join("") + "</" + listTag + ">");
      listTag = null;
      items = [];
    }
    function flushAll() { flushPara(); flushList(); }
    function openList(tag, item) {
      flushPara();
      if (listTag !== tag) { flushList(); listTag = tag; }
      items.push(item);
    }
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      if (code !== null) {
        if (/^\s*```/.test(line)) {
          out.push("<pre><code>" + escHtml(code.join("\n")) + "</code></pre>");
          code = null;
        } else {
          code.push(line);
        }
        continue;
      }
      if (/^\s*```/.test(line)) { flushAll(); code = []; continue; }
      if (!line.trim()) { flushAll(); continue; }
      var head = line.match(/^\s*(#{1,4})\s+(.*)$/);
      if (head) {
        flushAll();
        var lvl = Math.min(head[1].length + 2, 6);
        out.push("<h" + lvl + ">" + inlineMd(head[2].trim()) + "</h" + lvl + ">");
        continue;
      }
      var bullet = line.match(/^\s*[-*•]\s+(.*)$/);
      if (bullet) { openList("ul", bullet[1]); continue; }
      var numbered = line.match(/^\s*\d+[.)]\s+(.*)$/);
      if (numbered) { openList("ol", numbered[1]); continue; }
      flushList();
      para.push(line);
    }
    if (code !== null) {
      out.push("<pre><code>" + escHtml(code.join("\n")) + "</code></pre>");
    }
    flushAll();
    return out.join("");
  }

  function toolLabel(name) {
    if (name === "get_driver_health") return "Checking drivers";
    if (name === "get_recent_logs") return "Reading logs";
    if (name === "get_plan_now") return "Reading the plan";
    if (name === "get_support_report") return "Reading the help report";
    if (name === "get_version") return "Reading the version";
    return "Looking further";
  }

  function ensureStyles() {
    if (document.getElementById("ftw-ask-styles")) return;
    var style = document.createElement("style");
    style.id = "ftw-ask-styles";
    style.textContent = [
      ".ftw-ask-backdrop{position:fixed;inset:0;background:rgba(0,0,0,0.55);z-index:9000;display:flex;align-items:center;justify-content:center;}",
      ".ftw-ask-shell{width:min(680px,94vw);max-height:90vh;min-height:0;display:flex;flex-direction:column;background:var(--ink-raised,#161616);border:1px solid var(--line,#2a2a2a);border-radius:10px;overflow:hidden;}",
      ".ftw-ask-head{flex:0 0 auto;display:flex;align-items:center;justify-content:space-between;gap:12px;padding:14px 18px;border-bottom:1px solid var(--line,#2a2a2a);background:var(--ink,#111);}",
      ".ftw-ask-title{font-family:var(--mono,monospace);font-size:0.7rem;letter-spacing:0.18em;text-transform:uppercase;color:var(--accent-e,#f5b942);font-weight:500;}",
      ".ftw-ask-close{font-size:1.4rem;line-height:1;background:transparent;border:none;color:var(--fg-muted,#858585);cursor:pointer;padding:4px 8px;}",
      ".ftw-ask-close:hover{color:var(--fg,#e8e8e8);}",
      ".ftw-ask-body{flex:1 1 auto;min-height:0;display:flex;flex-direction:column;overflow:hidden;}",
      ".ftw-ask-thread{padding:16px 18px;overflow:auto;flex:1 1 auto;min-height:0;display:flex;flex-direction:column;gap:10px;}",
      ".ftw-ask-msg{max-width:92%;padding:10px 12px;border-radius:10px;font:0.9rem/1.5 var(--sans,sans-serif);overflow-wrap:anywhere;word-break:break-word;}",
      ".ftw-ask-msg.user{align-self:flex-end;background:var(--ink,#111);border:1px solid var(--line,#2a2a2a);color:var(--fg,#e8e8e8);}",
      ".ftw-ask-msg.assistant{align-self:flex-start;background:var(--ink-sunken,#0d0d0d);border:1px solid var(--line,#2a2a2a);color:var(--fg,#e8e8e8);}",
      ".ftw-ask-msg.assistant strong{color:var(--fg,#e8e8e8);}",
      ".ftw-ask-msg.assistant code{font-family:var(--mono,monospace);font-size:0.82em;}",
      ".ftw-ask-msg.assistant > *:first-child{margin-top:0;}",
      ".ftw-ask-msg.assistant > *:last-child{margin-bottom:0;}",
      ".ftw-ask-msg.assistant p{margin:0 0 0.7em;}",
      ".ftw-ask-msg.assistant h3,.ftw-ask-msg.assistant h4,.ftw-ask-msg.assistant h5,.ftw-ask-msg.assistant h6{margin:0.9em 0 0.4em;font-size:0.86rem;font-weight:600;color:var(--fg,#e8e8e8);}",
      ".ftw-ask-msg.assistant ul,.ftw-ask-msg.assistant ol{margin:0 0 0.7em;padding-left:1.35em;}",
      ".ftw-ask-msg.assistant li{margin:0.15em 0;}",
      ".ftw-ask-msg.assistant pre{margin:0 0 0.7em;padding:8px 10px;background:var(--ink,#111);border:1px solid var(--line,#2a2a2a);border-radius:6px;overflow-x:auto;}",
      ".ftw-ask-msg.assistant pre code{font-size:0.78em;white-space:pre;}",
      // The caret is the sign the model is still writing.
      ".ftw-ask-msg.is-streaming > *:last-child::after{content:'';display:inline-block;width:0.5em;height:1em;margin-left:2px;vertical-align:text-bottom;background:var(--accent-e,#f5b942);animation:ftw-ask-caret 1s steps(2,start) infinite;}",
      "@keyframes ftw-ask-caret{to{visibility:hidden;}}",
      "@media (prefers-reduced-motion:reduce){.ftw-ask-msg.is-streaming > *:last-child::after{animation:none;}}",
      ".ftw-ask-activity{align-self:flex-start;display:flex;flex-direction:column;gap:4px;padding:2px 0 4px;max-width:100%;}",
      ".ftw-ask-step{display:flex;align-items:center;gap:8px;color:var(--fg-dim,#a0a0a0);font:0.78rem/1.4 var(--sans,sans-serif);}",
      ".ftw-ask-step::before{content:'';width:6px;height:6px;border-radius:50%;background:var(--fg-muted,#858585);flex:0 0 auto;}",
      ".ftw-ask-step.is-live{color:var(--accent-e,#f5b942);}",
      ".ftw-ask-step.is-live::before{background:var(--accent-e,#f5b942);box-shadow:0 0 0 3px rgba(245,185,66,0.22);}",
      ".ftw-ask-step.is-tool{font-family:var(--mono,monospace);font-size:0.72rem;letter-spacing:0.02em;}",
      ".ftw-ask-note{color:var(--fg-dim,#a0a0a0);font-size:0.84rem;line-height:1.45;margin:0;}",
      ".ftw-ask-error{color:var(--red-e,#ef4444);font-size:0.84rem;}",
      ".ftw-ask-foot{flex:0 0 auto;display:flex;justify-content:space-between;align-items:center;gap:8px;padding:0 18px 10px;flex-wrap:wrap;}",
      ".ftw-ask-status{color:var(--fg-dim,#a0a0a0);font-size:0.78rem;}",
      ".ftw-ask-composer{flex:0 0 auto;display:flex;gap:8px;padding:12px 18px 16px;border-top:1px solid var(--line,#2a2a2a);}",
      ".ftw-ask-composer input{flex:1;min-width:0;background:var(--ink-sunken,#0d0d0d);color:var(--fg,#e8e8e8);border:1px solid var(--line,#2a2a2a);border-radius:8px;padding:10px 12px;font:0.9rem var(--sans,sans-serif);}",
      ".ftw-ask-btn{font-family:var(--sans,sans-serif);font-size:0.78rem;font-weight:500;letter-spacing:0.02em;padding:8px 14px;border:1px solid var(--line,#2a2a2a);border-radius:8px;background:transparent;color:var(--fg,#e8e8e8);cursor:pointer;}",
      ".ftw-ask-btn:hover{border-color:var(--fg-dim,#a0a0a0);}",
      ".ftw-ask-btn[disabled]{opacity:0.55;cursor:default;}",
      ".ftw-ask-btn-primary{background:var(--accent-e,#f5b942);color:#0a0a0a;border-color:var(--accent-e,#f5b942);}",
      ".ftw-ask-setup{padding:16px 18px;}",
      ".ftw-ask-setup a{color:var(--accent-e,#f5b942);}",
      ".ftw-ask-head-actions{display:flex;align-items:center;gap:4px;}",
      ".ftw-ask-icon{font-family:var(--sans,sans-serif);font-size:0.72rem;letter-spacing:0.02em;background:transparent;border:1px solid var(--line,#2a2a2a);border-radius:6px;color:var(--fg-dim,#a0a0a0);cursor:pointer;padding:4px 9px;}",
      ".ftw-ask-icon:hover{color:var(--fg,#e8e8e8);border-color:var(--fg-dim,#a0a0a0);}",
      ".ftw-ask-history{list-style:none;margin:0;padding:8px 10px;overflow:auto;flex:1 1 auto;min-height:0;}",
      ".ftw-ask-history li{display:flex;align-items:stretch;gap:4px;}",
      ".ftw-ask-thread{flex:1;min-width:0;text-align:left;background:transparent;border:none;border-radius:8px;color:var(--fg,#e8e8e8);cursor:pointer;padding:9px 10px;display:flex;flex-direction:column;gap:2px;}",
      ".ftw-ask-thread:hover{background:var(--ink-sunken,#0d0d0d);}",
      ".ftw-ask-thread-title{font:0.86rem/1.35 var(--sans,sans-serif);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}",
      ".ftw-ask-thread-meta{font:0.72rem var(--mono,monospace);color:var(--fg-muted,#858585);}",
      ".ftw-ask-thread-del{flex:0 0 auto;background:transparent;border:none;color:var(--fg-muted,#858585);cursor:pointer;padding:0 8px;font-size:1.1rem;line-height:1;border-radius:8px;}",
      ".ftw-ask-thread-del:hover{color:var(--red-e,#ef4444);background:var(--ink-sunken,#0d0d0d);}",
    ].join("");
    document.head.appendChild(style);
  }

  var state = {
    backdrop: null,
    last: null,
    keyHandler: null,
    trigger: null,
    offlineNames: [],
    busy: false,
    turns: [],
    generation: 0,
    abort: null,
    threadID: "",
  };

  function offlineDrivers(data) {
    var names = [];
    var drivers = (data && data.drivers) || {};
    Object.keys(drivers).sort().forEach(function (name) {
      var d = drivers[name] || {};
      if (d.status === "offline") names.push(name);
    });
    return names;
  }

  function updateChip(data) {
    var chip = document.getElementById("ask-why-chip");
    if (!chip) return;
    var names = offlineDrivers(data);
    state.offlineNames = names;
    if (names.length === 0) {
      chip.hidden = true;
      chip.textContent = "";
      chip.removeAttribute("title");
      return;
    }
    chip.hidden = false;
    if (names.length === 1) {
      chip.textContent = names[0] + " is offline — Ask why";
    } else {
      chip.textContent = names.length + " drivers offline — Ask why";
    }
    chip.title = names.join(", ") + " offline";
  }

  function close() {
    state.generation += 1;
    if (state.abort) {
      try { state.abort.abort(); } catch (e) { /* already closed */ }
      state.abort = null;
    }
    if (state.keyHandler) {
      document.removeEventListener("keydown", state.keyHandler);
      state.keyHandler = null;
    }
    if (state.backdrop && state.backdrop.parentNode) {
      state.backdrop.parentNode.removeChild(state.backdrop);
    }
    state.backdrop = null;
    state.busy = false;
    state.turns = [];
    state.threadID = "";
  }

  function stillOpen(gen) {
    return gen === state.generation && !!state.backdrop;
  }

  // Abort the in-flight question but keep the dialog and the thread.
  // close() aborts too, but bumps the generation and drops everything.
  function stop() {
    if (!state.abort) return;
    try { state.abort.abort(); } catch (e) { /* already finished */ }
  }

  function threadEl() {
    return state.backdrop && state.backdrop.querySelector('[data-role="thread"]');
  }

  function addUser(text) {
    var thread = threadEl();
    if (!thread) return;
    var el = document.createElement("div");
    el.className = "ftw-ask-msg user";
    el.textContent = text;
    thread.appendChild(el);
    thread.scrollTop = thread.scrollHeight;
  }

  function addAssistant(text) {
    var thread = threadEl();
    if (!thread) return;
    var el = document.createElement("div");
    el.className = "ftw-ask-msg assistant";
    el.innerHTML = formatAnswer(text);
    thread.appendChild(el);
    thread.scrollTop = thread.scrollHeight;
  }

  function visibleAnswer(s) {
    s = String(s || "");
    var cut = s.search(/\n#{1,3}\s*Issue title\b/i);
    if (cut < 0) cut = s.search(/\n\*\*Issue title\*\*/i);
    if (cut >= 0) s = s.slice(0, cut);
    s = s.replace(/^#{1,3}\s*Answer\s*\n+/i, "");
    s = s.replace(/^\*\*Answer\*\*\s*\n+/i, "");
    return s;
  }

  // One run is one question. Its activity log and draft bubble are held
  // by reference, never looked up in the thread: a DOM query would find
  // the previous question's nodes and append this answer above the one
  // before it.
  function newRun() {
    return { activity: null, draft: null, streamed: "" };
  }

  function addActivity(run, text, kind) {
    var thread = threadEl();
    if (!thread) return;
    if (!run.activity) {
      run.activity = document.createElement("div");
      run.activity.className = "ftw-ask-activity";
      thread.appendChild(run.activity);
    }
    var live = run.activity.querySelector(".is-live");
    if (live) live.classList.remove("is-live");
    var el = document.createElement("div");
    el.className = "ftw-ask-step" + (kind === "tool" ? " is-tool is-live" : " is-live");
    el.textContent = text;
    run.activity.appendChild(el);
    thread.scrollTop = thread.scrollHeight;
  }

  function settleActivity(run) {
    if (!run.activity) return;
    var live = run.activity.querySelector(".is-live");
    if (live) live.classList.remove("is-live");
  }

  // The last line is half-written while tokens arrive. Rendering it would
  // flash a partial "## Issue title" as a heading, so hold that line back
  // until it ends.
  function streamingText(s) {
    return visibleAnswer(s).replace(/\n#{1,4}[^\n]*$/, "");
  }

  function appendDelta(run) {
    var thread = threadEl();
    if (!thread) return;
    if (!run.draft) {
      run.draft = document.createElement("div");
      run.draft.className = "ftw-ask-msg assistant is-streaming";
      thread.appendChild(run.draft);
    }
    // Rendered as it arrives: raw ** and - for the length of a slow free
    // model's answer reads as broken output.
    run.draft.innerHTML = formatAnswer(streamingText(run.streamed));
    thread.scrollTop = thread.scrollHeight;
  }

  // A new model round discards what streamed in the previous one: that
  // text led up to a tool call, it is not the answer being written.
  function resetDraft(run) {
    run.streamed = "";
    if (run.draft && run.draft.parentNode) {
      run.draft.parentNode.removeChild(run.draft);
    }
    run.draft = null;
  }

  function finishAssistant(run, text) {
    var thread = threadEl();
    if (!run.draft) {
      addAssistant(text);
      return;
    }
    run.draft.classList.remove("is-streaming");
    run.draft.innerHTML = formatAnswer(text);
    run.draft = null;
    if (thread) thread.scrollTop = thread.scrollHeight;
  }

  function setFoot(html) {
    var foot = state.backdrop && state.backdrop.querySelector('[data-role="foot"]');
    if (foot) foot.innerHTML = html || "";
  }

  function openFilledIssue(result) {
    if (!result || !result.issue_title) return;
    var url = result.issue_url || (ISSUE_NEW + "&title=" + encodeURIComponent(result.issue_title) + "&body=" + encodeURIComponent(result.issue_body || result.answer || ""));
    var body = result.issue_body || result.answer || "";
    if (navigator.clipboard && navigator.clipboard.writeText && body) {
      navigator.clipboard.writeText(body).catch(function () {});
    }
    window.open(url, "_blank", "noopener");
  }

  function parseSSEChunk(buf, onEvent) {
    var parts = buf.split("\n\n");
    var rest = parts.pop() || "";
    parts.forEach(function (block) {
      var line = block.split("\n").filter(function (l) { return l.indexOf("data:") === 0; }).map(function (l) {
        return l.replace(/^data:\s?/, "");
      }).join("");
      if (!line) return;
      var ev;
      try { ev = JSON.parse(line); } catch (e) { return; }
      onEvent(ev);
    });
    return rest;
  }

  function ask(question) {
    var q = String(question || "").trim();
    if (!q || state.busy) return;
    var gen = state.generation;
    var run = newRun();
    state.busy = true;
    addUser(q);
    setFoot("");
    var askBtn = state.backdrop && state.backdrop.querySelector('[data-role="ask"]');
    var input = state.backdrop && state.backdrop.querySelector('[data-role="input"]');
    // The button becomes Stop while the model works. A free model can
    // run for a minute; leaving no way out but closing the dialog threw
    // the thread away.
    if (askBtn) askBtn.textContent = "Stop";
    if (input) { input.value = ""; input.disabled = true; }
    var payload = { question: q, history: state.turns.slice() };
    if (state.trigger) payload.trigger = state.trigger;
    if (state.threadID) payload.thread_id = state.threadID;
    var fetchOpts = {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
      body: JSON.stringify(payload),
    };
    if (typeof AbortController === "function") {
      state.abort = new AbortController();
      fetchOpts.signal = state.abort.signal;
    }
    apiFetch("/api/assistant/ask", fetchOpts)
      .then(function (r) {
        if (!stillOpen(gen)) return null;
        var ctype = (r.headers.get("Content-Type") || "");
        if (ctype.indexOf("text/event-stream") >= 0 && r.body && r.body.getReader) {
          var reader = r.body.getReader();
          var decoder = new TextDecoder();
          var buf = "";
          var donePayload = null;
          function pump() {
            return reader.read().then(function (chunk) {
              if (!stillOpen(gen)) return null;
              if (chunk.value) buf += decoder.decode(chunk.value, { stream: true });
              buf = parseSSEChunk(buf, function (ev) {
                if (!stillOpen(gen)) return;
                if (ev.type === "round") resetDraft(run);
                if (ev.type === "status") addActivity(run, ev.text || "Working");
                if (ev.type === "tool") addActivity(run, toolLabel(ev.text), "tool");
                if (ev.type === "delta") {
                  run.streamed += ev.text || "";
                  appendDelta(run);
                }
                if (ev.type === "error") throw new Error(ev.error || "Ask why failed");
                if (ev.type === "done") donePayload = ev;
              });
              if (chunk.done) return donePayload;
              return pump();
            });
          }
          return pump().then(function (ev) {
            if (!stillOpen(gen)) return null;
            if (!ev) throw new Error("No answer");
            return ev;
          });
        }
        return r.json().then(function (j) {
          if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
          return j;
        });
      })
      .then(function (j) {
        if (!stillOpen(gen) || !j) return;
        settleActivity(run);
        state.last = j;
        if (j.thread_id) state.threadID = j.thread_id;
        state.turns.push({ role: "user", text: q });
        state.turns.push({ role: "assistant", text: j.answer || "" });
        if (state.turns.length > 6) state.turns = state.turns.slice(-6);
        finishAssistant(run, j.answer || "");
        var used = j.resolved_model || j.model || "";
        var foot = '<span class="ftw-ask-status">' + escHtml(used ? "Answered by " + used : "Done") + "</span>";
        if (j.issue_title) {
          foot += '<button class="ftw-ask-btn ftw-ask-btn-primary" data-role="open-issue" type="button">Open GitHub issue</button>';
        }
        setFoot(foot);
        var issueBtn = state.backdrop && state.backdrop.querySelector('[data-role="open-issue"]');
        if (issueBtn) issueBtn.addEventListener("click", function () { openFilledIssue(state.last); });
      })
      .catch(function (err) {
        if (!stillOpen(gen)) return;
        settleActivity(run);
        // Stopped by the operator: keep whatever arrived, as an answer
        // that is simply cut short.
        if (err && err.name === "AbortError") {
          if (run.streamed.trim()) {
            finishAssistant(run, visibleAnswer(run.streamed));
            addActivity(run, "Stopped");
          } else {
            resetDraft(run);
            addActivity(run, "Stopped before the model answered");
          }
          return;
        }
        // A failed stream must not leave a half-written bubble behind:
        // the next question would otherwise stream into it.
        resetDraft(run);
        var thread = threadEl();
        if (thread) {
          var el = document.createElement("div");
          el.className = "ftw-ask-error";
          el.textContent = (err && err.message) || String(err);
          thread.appendChild(el);
          thread.scrollTop = thread.scrollHeight;
        }
      })
      .then(function () {
        if (!stillOpen(gen)) return;
        state.busy = false;
        state.abort = null;
        if (askBtn) { askBtn.disabled = false; askBtn.textContent = "Ask why"; }
        if (input) { input.disabled = false; input.focus(); }
      });
  }

  // "14:32" today, "Tue 14:32" this week, "3 Sep" beyond that. A stored
  // conversation is found by when it happened, so the stamp has to say
  // that without a full date on every row.
  function whenLabel(ms) {
    if (!ms) return "";
    var d = new Date(ms);
    if (isNaN(d.getTime())) return "";
    var now = new Date();
    var hhmm = String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
    if (d.toDateString() === now.toDateString()) return hhmm;
    if (now - d < 6 * 864e5) {
      return ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"][d.getDay()] + " " + hhmm;
    }
    return d.getDate() + " " + ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"][d.getMonth()];
  }

  function renderHistory(host) {
    host.innerHTML = '<div class="ftw-ask-setup"><p class="ftw-ask-note">Loading…</p></div>';
    var gen = state.generation;
    apiFetch("/api/assistant/threads")
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (data) {
        if (!stillOpen(gen)) return;
        var threads = (data && data.threads) || [];
        if (!threads.length) {
          host.innerHTML =
            '<div class="ftw-ask-setup"><p class="ftw-ask-note">No earlier conversations yet. ' +
            "Ask something and it is kept on the box.</p></div>";
          return;
        }
        var rows = threads.map(function (t) {
          return '<li><button class="ftw-ask-thread" type="button" data-thread="' + escHtml(t.id) + '">' +
            '<span class="ftw-ask-thread-title">' + escHtml(t.title || "Ask why") + "</span>" +
            '<span class="ftw-ask-thread-meta">' + escHtml(whenLabel(t.updated_ms)) +
            (t.turn_count ? " · " + Math.ceil(t.turn_count / 2) + (t.turn_count > 2 ? " questions" : " question") : "") +
            "</span></button>" +
            '<button class="ftw-ask-thread-del" type="button" data-del="' + escHtml(t.id) + '" aria-label="Delete conversation" title="Delete">×</button></li>';
        }).join("");
        host.innerHTML = '<ul class="ftw-ask-history">' + rows + "</ul>";
        host.querySelectorAll("[data-thread]").forEach(function (btn) {
          btn.addEventListener("click", function () { openThread(btn.getAttribute("data-thread")); });
        });
        host.querySelectorAll("[data-del]").forEach(function (btn) {
          btn.addEventListener("click", function () {
            var id = btn.getAttribute("data-del");
            apiFetch("/api/assistant/threads/" + encodeURIComponent(id), { method: "DELETE" })
              .then(function () { if (stillOpen(gen)) renderHistory(host); });
          });
        });
      })
      .catch(function (err) {
        if (!stillOpen(gen)) return;
        host.innerHTML = '<p class="ftw-ask-error" style="padding:16px 18px">Could not read earlier conversations: ' +
          escHtml((err && err.message) || String(err)) + "</p>";
      });
  }

  // Reopen a stored conversation: its turns become the thread on screen and
  // the composer continues it, so a follow-up lands in the same row.
  function openThread(id) {
    var gen = state.generation;
    var body = state.backdrop && state.backdrop.querySelector('[data-role="body"]');
    if (!body) return;
    apiFetch("/api/assistant/threads/" + encodeURIComponent(id))
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (t) {
        if (!stillOpen(gen)) return;
        state.threadID = t.id || "";
        state.turns = [];
        showHistory(false);
        renderReady(body, {}, {});
        (t.turns || []).forEach(function (turn) {
          if (turn.role === "user") addUser(turn.text || "");
          else addAssistant(turn.text || "");
          state.turns.push({ role: turn.role, text: turn.text || "" });
        });
        if (state.turns.length > 6) state.turns = state.turns.slice(-6);
        if (t.model) {
          setFoot('<span class="ftw-ask-status">' + escHtml("Answered by " + t.model) + "</span>");
        }
      })
      .catch(function (err) {
        if (!stillOpen(gen)) return;
        body.innerHTML = '<p class="ftw-ask-error" style="padding:16px 18px">Could not open that conversation: ' +
          escHtml((err && err.message) || String(err)) + "</p>";
      });
  }

  // Flip the header between the live conversation and the history list.
  function showHistory(on) {
    var root = state.backdrop;
    if (!root) return;
    var histBtn = root.querySelector('[data-role="history"]');
    var newBtn = root.querySelector('[data-role="new"]');
    if (histBtn) {
      histBtn.setAttribute("aria-expanded", on ? "true" : "false");
      histBtn.hidden = !!on;
    }
    if (newBtn) newBtn.hidden = !on;
  }

  function renderSetup(host, status) {
    host.innerHTML =
      '<div class="ftw-ask-setup">' +
      '  <p class="ftw-ask-note">' + escHtml(status.unavailable || "Ask why needs an OpenRouter key.") + "</p>" +
      '  <p class="ftw-ask-note">Create a free key (no card) and paste it in Settings → System. Pasting the key turns Ask why on.</p>' +
      '  <p class="ftw-ask-note"><a href="' + escHtml(status.setup_url || "https://openrouter.ai/keys") + '" target="_blank" rel="noopener">Get a free key</a></p>' +
      "</div>";
  }

  function renderReady(host, status, opts) {
    opts = opts || {};
    var placeholder = (opts.trigger && opts.trigger.kind === "plan")
      ? "Ask about this plan…"
      : "Ask about this house…";
    host.innerHTML =
      '<div class="ftw-ask-thread" data-role="thread"></div>' +
      '<div class="ftw-ask-foot" data-role="foot"></div>' +
      '<form class="ftw-ask-composer" data-role="form">' +
      '  <input type="text" data-role="input" maxlength="2000" placeholder="' + escHtml(placeholder) + '" aria-label="Question" autocomplete="off">' +
      '  <button class="ftw-ask-btn ftw-ask-btn-primary" data-role="ask" type="submit">Ask why</button>' +
      "</form>";
    var form = host.querySelector('[data-role="form"]');
    var input = host.querySelector('[data-role="input"]');
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      if (state.busy) { stop(); return; }
      ask(input.value);
    });
    if (opts.question) {
      ask(opts.question);
    } else {
      input.focus();
    }
  }

  function open(opts) {
    opts = opts || {};
    ensureStyles();
    close();
    state.trigger = opts.trigger || null;
    state.last = null;
    state.turns = [];
    state.threadID = "";
    var gen = state.generation;
    var title = (opts.trigger && opts.trigger.kind === "plan") ? "Ask why this plan" : "Ask why";
    var backdrop = document.createElement("div");
    backdrop.className = "ftw-ask-backdrop";
    backdrop.innerHTML =
      '<div class="ftw-ask-shell" role="dialog" aria-modal="true" aria-labelledby="ftw-ask-title">' +
      '  <div class="ftw-ask-head">' +
      '    <span class="ftw-ask-title" id="ftw-ask-title">' + escHtml(title) + "</span>" +
      '    <span class="ftw-ask-head-actions">' +
      '      <button class="ftw-ask-icon" data-role="history" type="button" aria-label="Earlier conversations" title="Earlier conversations" aria-expanded="false">Earlier</button>' +
      '      <button class="ftw-ask-icon" data-role="new" type="button" aria-label="New conversation" title="New conversation" hidden>New</button>' +
      '      <button class="ftw-ask-close" data-role="close" aria-label="Close" type="button">×</button>' +
      "    </span>" +
      "  </div>" +
      '  <div class="ftw-ask-body" data-role="body"><p class="ftw-ask-note" style="padding:16px 18px">Loading…</p></div>' +
      "</div>";
    document.body.appendChild(backdrop);
    state.backdrop = backdrop;
    backdrop.addEventListener("click", function (e) {
      if (e.target === backdrop) close();
    });
    backdrop.querySelector('[data-role="close"]').addEventListener("click", close);
    backdrop.querySelector('[data-role="history"]').addEventListener("click", function () {
      if (state.busy) return;
      showHistory(true);
      renderHistory(backdrop.querySelector('[data-role="body"]'));
    });
    backdrop.querySelector('[data-role="new"]').addEventListener("click", function () {
      state.threadID = "";
      state.turns = [];
      state.last = null;
      showHistory(false);
      renderReady(backdrop.querySelector('[data-role="body"]'), {}, opts);
    });
    state.keyHandler = function (e) {
      if (e.key === "Escape") close();
    };
    document.addEventListener("keydown", state.keyHandler);

    var body = backdrop.querySelector('[data-role="body"]');
    apiFetch("/api/assistant/status")
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (status) {
        if (!stillOpen(gen)) return;
        if (status.ready) renderReady(body, status, opts);
        else renderSetup(body, status);
      })
      .catch(function (err) {
        if (!stillOpen(gen)) return;
        body.innerHTML = '<p class="ftw-ask-error" style="padding:16px 18px">Could not load Ask why: ' + escHtml(err.message) + "</p>";
      });
  }

  function askAboutPlan(question) {
    var q = String(question || "").trim();
    if (!q) q = "Why did FTW plan the next hours this way?";
    open({
      question: q,
      trigger: { kind: "plan" },
    });
  }

  function bind() {
    var btn = document.getElementById("plan-ask-why");
    if (btn) btn.addEventListener("click", function () { open(); });
    var form = document.getElementById("plan-ask-why-form");
    var input = document.getElementById("plan-ask-why-input");
    if (form) {
      form.addEventListener("submit", function (e) {
        e.preventDefault();
        var q = input ? input.value : "";
        if (input) input.value = "";
        askAboutPlan(q);
      });
    }
    var chip = document.getElementById("ask-why-chip");
    if (chip) {
      chip.addEventListener("click", function () {
        var names = state.offlineNames || [];
        var question = names.length === 1
          ? names[0] + " is offline. Why?"
          : names.length + " drivers are offline. Why?";
        open({
          question: question,
          trigger: { kind: "driver_offline", driver: names[0] || "" },
        });
      });
    }
    window.addEventListener("ftw-status", function (e) {
      updateChip(e.detail);
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", bind);
  } else {
    bind();
  }

  window.FTWAskWhy = { open: open, close: close, updateChip: updateChip, _test: { parseSSEChunk: parseSSEChunk, visibleAnswer: visibleAnswer, formatAnswer: formatAnswer, whenLabel: whenLabel } };
})();
