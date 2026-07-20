import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const diagnostics = readFileSync(join(webRoot, "diagnostics-modal.js"), "utf8");
const system = readFileSync(join(webRoot, "settings", "tabs", "system.js"), "utf8");

describe("driver feedback", () => {
  it("uses the public driver templates without app auth or tokens", () => {
    assert.match(diagnostics, /srcfl\/device-drivers\/issues\/new\?template=driver_issue\.yml/);
    assert.match(diagnostics, /srcfl\/device-drivers\/issues\/new\?template=driver_request\.yml/);
    assert.doesNotMatch(diagnostics, /github.*(?:oauth|token)|Authorization.*github/i);
  });

  it("requires review and offers copy before opening GitHub", () => {
    assert.match(diagnostics, /Review driver report/);
    assert.match(diagnostics, /Sanitized driver diagnostics/);
    assert.match(diagnostics, /Copy diagnostics/);
    assert.match(diagnostics, /Open GitHub issue/);
    assert.doesNotMatch(diagnostics, /DRIVER_ISSUE_URL\s*\+[^\n]*body=/);
  });

  it("marks local code unsigned and omits sensitive detail", () => {
    assert.match(diagnostics, /local \/ unsigned/);
    assert.match(diagnostics, /Last error:.*omitted here for privacy/);
    assert.match(diagnostics, /raw errors, logs, IP addresses, serial numbers, credentials, site IDs and config out/);
    assert.match(diagnostics, /entry\.source === "managed" && entry\.package_id && entry\.artifact_sha256/);
  });
});

describe("FTW feedback", () => {
  it("keeps FTW bugs and features in FTW with Discord as secondary help", () => {
    assert.match(system, /srcfl\/ftw\/issues\/new\?template=bug_report\.yml/);
    assert.match(system, /srcfl\/ftw\/issues\/new\?template=feature_request\.yml/);
    assert.match(system, /ask on Discord/);
  });
});
