import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const assistant = readFileSync(new URL("./assistant.js", import.meta.url), "utf8");
const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const system = readFileSync(new URL("./settings/tabs/system.js", import.meta.url), "utf8");
const appJs = readFileSync(new URL("./app.js", import.meta.url), "utf8");
const appCss = readFileSync(new URL("./app.css", import.meta.url), "utf8");
const issueTemplate = readFileSync(new URL("../.github/ISSUE_TEMPLATE/ask_why.md", import.meta.url), "utf8");
const goAssistant = readFileSync(new URL("../go/internal/assistant/assistant.go", import.meta.url), "utf8");
const goApi = readFileSync(new URL("../go/internal/api/api_assistant.go", import.meta.url), "utf8");
const goState = readFileSync(new URL("../go/internal/state/assistant_threads.go", import.meta.url), "utf8");
const goSecurity = readFileSync(new URL("../go/internal/api/security.go", import.meta.url), "utf8");
const goThreads = readFileSync(new URL("../go/internal/api/api_assistant_threads.go", import.meta.url), "utf8");

// escHtml escapes by writing textContent and reading innerHTML back, so
// the stub element has to do the same or the escaping tests prove nothing.
function stubElement() {
  const el = { style: {}, innerHTML: "", setAttribute() {} };
  Object.defineProperty(el, "textContent", {
    get: () => el.innerHTML,
    set: (v) => {
      el.innerHTML = String(v)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;");
    },
  });
  return el;
}

// Runs assistant.js against a stub DOM and hands back window.FTWAskWhy.
function sandboxed() {
  const sandbox = {
    document: {
      readyState: "complete",
      getElementById: () => null,
      addEventListener() {},
      createElement: stubElement,
      head: { appendChild() {} },
      body: { appendChild() {} },
    },
    fetch: () => new Promise(() => {}),
    navigator: {},
    console,
    addEventListener() {},
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(assistant, sandbox);
  return sandbox.window.FTWAskWhy;
}

test("Ask why is wired from the plan card", () => {
  assert.match(index, /id="plan-ask-why"/);
  assert.match(index, /assistant\.js/);
  assert.match(assistant, /getElementById\("plan-ask-why"\)/);
  assert.match(assistant, /\/api\/assistant\/ask/);
  assert.match(assistant, /\/api\/assistant\/status/);
});

test("the plan briefing takes a question about the schedule", () => {
  assert.match(index, /id="plan-ask-why-form"/);
  assert.match(index, /id="plan-ask-why-plan"/);
  assert.match(index, /id="plan-ask-why-input"/);
  assert.match(assistant, /kind: "plan"/);
  assert.match(assistant, /Why did FTW plan the next hours this way\?/);
});

test("Ask why drafts a GitHub issue instead of posting one", () => {
  assert.match(assistant, /Open GitHub issue/);
  assert.match(assistant, /issues\/new\?template=ask_why\.md/);
  assert.doesNotMatch(assistant, /bug_report\.yml/);
  assert.doesNotMatch(assistant, /api\.github\.com/);
  assert.match(issueTemplate, /One text field/);
  assert.doesNotMatch(issueTemplate, /What happened/);
});

test("the conversation keeps the question and the answer in separate messages", () => {
  assert.match(assistant, /ftw-ask-msg user/);
  assert.match(assistant, /ftw-ask-msg assistant/);
  assert.match(assistant, /text\/event-stream/);
  assert.match(assistant, /innerHTML = formatAnswer/);
  assert.doesNotMatch(assistant, /innerHTML = j\.answer/);
});

test("the dialog keeps long answers inside the shell", () => {
  assert.match(assistant, /ftw-ask-body/);
  assert.match(assistant, /min-height:0/);
  assert.match(assistant, /word-break:break-word/);
});

test("tokens and tool calls append while waiting", () => {
  assert.match(assistant, /ev\.type === "delta"/);
  assert.match(assistant, /ftw-ask-activity/);
  assert.match(assistant, /ftw-ask-step/);
  assert.match(assistant, /addActivity\(run, toolLabel/);
});

test("each question owns its activity log and draft bubble", () => {
  // A DOM lookup would find the previous question's nodes and thread the
  // new answer above the old one.
  assert.doesNotMatch(assistant, /querySelector\('\[data-role="activity"\]'\)/);
  assert.doesNotMatch(assistant, /querySelector\('\[data-role="draft"\]'\)/);
  assert.match(assistant, /function newRun\(\)/);
  assert.match(assistant, /run\.activity/);
  assert.match(assistant, /run\.draft/);
});

test("a new model round drops the text that led to a tool call", () => {
  assert.match(assistant, /ev\.type === "round"/);
  assert.match(assistant, /function resetDraft\(run\)/);
  assert.match(goAssistant, /Progress\("round", ""\)/);
});

test("a failed answer leaves no half-written bubble behind", () => {
  // Without this the next question streams into the stale draft.
  const catchBlock = assistant.slice(assistant.indexOf(".catch(function (err)"));
  assert.match(catchBlock, /resetDraft\(run\)/);
});

test("the operator can stop a slow model without losing the thread", () => {
  assert.match(assistant, /function stop\(\)/);
  assert.match(assistant, /if \(state\.busy\) \{ stop\(\); return; \}/);
  assert.match(assistant, /askBtn\.textContent = "Stop"/);
  assert.match(assistant, /askBtn\.textContent = "Ask why"/);
});

test("the activity log says each step once", () => {
  // The handler used to announce "Asking the model" that Complete also
  // sends once per round.
  assert.doesNotMatch(goApi, /progress\("status", "Asking the model"\)/);
  assert.match(goAssistant, /Progress\("status", "Asking the model"\)/);
});

test("the answer renders markdown, not raw dashes and hashes", () => {
  const md = sandboxed()._test.formatAnswer;
  const html = md("## Answer\n\nCharging **now**.\n\n- SoC is 41 %\n- Grid limit is 11 kW\n\n1. Cheap slot\n2. Expensive later\n\n`sungrow` is fine.");
  assert.match(html, /<ul><li>SoC is 41 %<\/li><li>Grid limit is 11 kW<\/li><\/ul>/);
  assert.match(html, /<ol><li>Cheap slot<\/li><li>Expensive later<\/li><\/ol>/);
  assert.match(html, /<strong>now<\/strong>/);
  assert.match(html, /<code>sungrow<\/code>/);
  assert.doesNotMatch(html, /- SoC/);
  assert.doesNotMatch(html, /## Answer/);
});

test("a fenced block renders as code and cannot inject markup", () => {
  const md = sandboxed()._test.formatAnswer;
  const html = md("Logs:\n\n```\n<script>alert(1)</script>\n```\n");
  assert.match(html, /<pre><code>/);
  assert.match(html, /&lt;script&gt;/);
  assert.doesNotMatch(html, /<script>/);
});

test("the answer never renders attacker markup as HTML", () => {
  const md = sandboxed()._test.formatAnswer;
  const html = md('<img src=x onerror="alert(1)"> and **bold**');
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /&lt;img/);
  assert.match(html, /<strong>bold<\/strong>/);
});

test("visibleAnswer hides issue fields while the answer streams", () => {
  const sandbox = {
    document: {
      readyState: "complete",
      getElementById: () => null,
      addEventListener() {},
      createElement: () => ({ style: {}, setAttribute() {} }),
      head: { appendChild() {} },
      body: { appendChild() {} },
    },
    fetch: () => new Promise(() => {}),
    navigator: {},
    console,
    addEventListener() {},
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(assistant, sandbox);
  const vis = sandbox.window.FTWAskWhy._test.visibleAnswer;
  assert.equal(vis("## Answer\nHello\n\n## Issue title\nbug").trim(), "Hello");
});

test("follow-ups send the earlier turns", () => {
  assert.match(assistant, /history: state\.turns\.slice\(\)/);
  assert.match(assistant, /role: "user"/);
  assert.match(assistant, /role: "assistant"/);
});

test("a closed dialog does not take the late answer", () => {
  assert.match(assistant, /state\.generation/);
  assert.match(assistant, /AbortController/);
  assert.match(assistant, /AbortError/);
  assert.match(assistant, /stillOpen\(gen\)/);
});

test("SSE error events reach the request, torn JSON does not", () => {
  const sandbox = {
    document: {
      readyState: "complete",
      getElementById: () => null,
      addEventListener() {},
      createElement: () => ({ style: {}, setAttribute() {} }),
      head: { appendChild() {} },
      body: { appendChild() {} },
    },
    fetch: () => new Promise(() => {}),
    navigator: {},
    console,
    addEventListener() {},
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(assistant, sandbox);
  const parse = sandbox.window.FTWAskWhy._test.parseSSEChunk;
  let seen = null;
  parse('data: {"type":"status","text":"Reading"}\n\n', (ev) => { seen = ev; });
  assert.equal(seen.type, "status");
  assert.doesNotThrow(() => parse("data: {nope\n\n", () => {
    throw new Error("torn frame must not call onEvent");
  }));
  assert.throws(() => {
    parse('data: {"type":"error","error":"key expired"}\n\n', (ev) => {
      if (ev.type === "error") throw new Error(ev.error);
    });
  }, /key expired/);
});

test("settings hold the OpenRouter key on the box", () => {
  assert.match(system, /data-path="assistant.api_key"/);
  assert.match(system, /openrouter\.ai\/keys/);
});

test("the header chip sits outside the hamburger cluster", () => {
  const chipAt = index.indexOf('id="ask-why-chip"');
  const rightAt = index.indexOf('class="header-right"');
  assert.ok(chipAt > 0 && chipAt < rightAt, "chip must stay in the header row on phones");
  assert.match(appCss, /\.ask-why-chip/);
  assert.match(appCss, /outside \.header-right/);
  assert.match(appJs, /CustomEvent\("ftw-status"/);
  assert.match(assistant, /driver_offline/);
});

test("the offline chip names the driver and hides when it recovers", () => {
  const chip = {
    hidden: true,
    textContent: "",
    title: "",
    removeAttribute() { this.title = ""; },
    addEventListener() {},
  };
  const sandbox = {
    document: {
      readyState: "complete",
      getElementById: (id) => (id === "ask-why-chip" ? chip : null),
      addEventListener() {},
      createElement: () => ({ style: {}, setAttribute() {} }),
      head: { appendChild() {} },
      body: { appendChild() {} },
    },
    fetch: () => new Promise(() => {}),
    navigator: {},
    console,
    addEventListener() {},
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(assistant, sandbox);
  sandbox.window.FTWAskWhy.updateChip({
    drivers: { sungrow: { status: "offline" }, meter: { status: "ok" } },
  });
  assert.equal(chip.hidden, false);
  assert.match(chip.textContent, /sungrow is offline/);
  sandbox.window.FTWAskWhy.updateChip({
    drivers: { sungrow: { status: "ok" }, meter: { status: "ok" } },
  });
  assert.equal(chip.hidden, true);
});

test("Ask why history lives on the box, not in the browser", () => {
  // localStorage would not survive a different device; the box is the record.
  assert.doesNotMatch(assistant, /localStorage|sessionStorage/);
  assert.match(assistant, /\/api\/assistant\/threads/);
  assert.match(goApi, /recordAssistantTurn/);
  assert.match(goThreads, /AssistantThread/);
});

test("a follow-up names the thread it continues", () => {
  assert.match(assistant, /payload\.thread_id = state\.threadID/);
  assert.match(assistant, /if \(j\.thread_id\) state\.threadID = j\.thread_id/);
});

test("earlier conversations can be opened and deleted", () => {
  assert.match(assistant, /function renderHistory/);
  assert.match(assistant, /function openThread/);
  assert.match(assistant, /method: "DELETE"/);
  assert.match(assistant, /data-role="history"/);
  assert.match(assistant, /data-role="new"/);
});

test("history is capped so an SD card cannot fill", () => {
  assert.match(goState, /AssistantThreadCap\s*=\s*\d+/);
  assert.match(goState, /DELETE FROM assistant_threads WHERE id NOT IN/);
});

test("a stored conversation never reaches a public host", () => {
  assert.match(goSecurity, /HasPrefix\(path, "\/api\/assistant\/threads\/"\)/);
});

test("the history list is stamped by when it happened", () => {
  const when = sandboxed()._test.whenLabel;
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 14, 32);
  assert.equal(when(today.getTime()), "14:32");
  assert.equal(when(0), "");
  assert.equal(when(NaN), "");
});
